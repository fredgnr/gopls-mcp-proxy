// Package workspace detects disk input changes without understanding backend
// business logic. Every included regular file is streamed through SHA-256.
// Interior symlinks contribute their target text and are never followed. Explicit
// roots may be symlinks: each scan resolves them again and fingerprints both the
// configured locator and its current target. Additional targets need explicit roots.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

type Fingerprint [32]byte

func (f Fingerprint) String() string { return fmt.Sprintf("%x", f[:8]) }

// ErrUnstable means a bounded scan could not obtain an unchanged checkpoint.
var ErrUnstable = errors.New("workspace_unstable")

type Snapshot struct {
	Fingerprint       Fingerprint
	Epoch             uint64
	Roots             []string
	Directories       []string
	Dependencies      []string
	GoFiles           []string // Informational only; no special fingerprint rules.
	DiscoveryDegraded bool
	watchDirectories  []string
	rootScopes        []rootScope
}

type Config struct {
	Workspace   string
	WatchRoots  []string
	Include     []string
	Exclude     []string
	GoDiscovery bool
	// WorkFile applies only to optional Go discovery: empty means ancestor
	// discovery; otherwise it is "off" or an absolute path.
	WorkFile    string
	ReplaceDirs []string // Legacy explicit roots; equivalent to WatchRoots.
	ExcludeDirs []string // Explicit absolute directory exclusions.
}

type rootScope struct {
	locator string
	target  string
	file    bool
}

type Tracker struct {
	cfg       Config
	gate      chan struct{}
	mu        sync.Mutex
	watcher   *fsnotify.Watcher
	watchMark func(string)
	failed    map[string]bool
	latest    Snapshot
	covered   uint64
	hasLatest bool
	requests  atomic.Uint64
	epoch     atomic.Uint64
	logger    *slog.Logger
	// These fields are owned by the serialized scan gate.
	discovered            []string
	discoveryDependencies []string
	discoveryObservations map[string]discoveryObservation
}

func New(cfg Config, logger *slog.Logger) (*Tracker, error) {
	var err error
	cfg.Workspace, err = filepath.Abs(cfg.Workspace)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(cfg.Workspace); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace must be an existing directory: %s", cfg.Workspace)
	}
	if cfg.WorkFile != "" && cfg.WorkFile != "off" && !filepath.IsAbs(cfg.WorkFile) {
		return nil, fmt.Errorf("GOWORK must be off or an absolute path")
	}
	cfg.WatchRoots = slices.Clone(cfg.WatchRoots)
	cfg.ReplaceDirs = slices.Clone(cfg.ReplaceDirs)
	cfg.ExcludeDirs = slices.Clone(cfg.ExcludeDirs)
	cfg.Include = slices.Clone(cfg.Include)
	cfg.Exclude = slices.Clone(cfg.Exclude)
	for _, paths := range [][]string{cfg.WatchRoots, cfg.ReplaceDirs, cfg.ExcludeDirs} {
		for i, p := range paths {
			if !filepath.IsAbs(p) {
				p = filepath.Join(cfg.Workspace, p)
			}
			paths[i] = filepath.Clean(p)
		}
	}
	for i, p := range cfg.ExcludeDirs {
		resolved, err := resolveExistingPrefix(p)
		if err != nil {
			return nil, err
		}
		cfg.ExcludeDirs[i] = resolved
	}
	for _, patterns := range [][]string{cfg.Include, cfg.Exclude} {
		for _, pattern := range patterns {
			if err := validateGlob(pattern); err != nil {
				return nil, err
			}
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	t := &Tracker{cfg: cfg, gate: make(chan struct{}, 1), failed: map[string]bool{}, logger: logger}
	for _, root := range t.explicitRoots() {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("required root %s: %w", root, err)
		}
		if err := t.checkRoot(resolved); err != nil {
			return nil, err
		}
	}
	logger.Info("input monitoring configured", "roots", t.explicitRoots(), "include", cfg.Include, "exclude", cfg.Exclude, "exclude_directories", cfg.ExcludeDirs, "go_discovery", cfg.GoDiscovery)
	return t, nil
}

func (t *Tracker) explicitRoots() []string {
	roots := append([]string{t.cfg.Workspace}, t.cfg.WatchRoots...)
	return append(roots, t.cfg.ReplaceDirs...)
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveExistingPrefix(path string) (string, error) {
	for prefix := path; ; prefix = filepath.Dir(prefix) {
		resolved, err := filepath.EvalSymlinks(prefix)
		if err == nil {
			suffix, err := filepath.Rel(prefix, path)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || prefix == filepath.Dir(prefix) {
			return "", err
		}
	}
}

func (t *Tracker) checkRoot(root string) error {
	home, _ := os.UserHomeDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	if root == filepath.Dir(root) || root == home {
		return fmt.Errorf("refusing to scan filesystem root or home directory: %s", root)
	}
	for _, excluded := range t.cfg.ExcludeDirs {
		if resolved, err := filepath.EvalSymlinks(excluded); err == nil {
			excluded = resolved
		}
		if within(root, excluded) {
			return fmt.Errorf("scan root is inside excluded directory: %s", root)
		}
	}
	return nil
}

func field(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func keys[V any](m map[string]V) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	slices.Sort(result)
	return result
}

func cloneSnapshot(s Snapshot) Snapshot {
	s.Roots = slices.Clone(s.Roots)
	s.Directories = slices.Clone(s.Directories)
	s.Dependencies = slices.Clone(s.Dependencies)
	s.GoFiles = slices.Clone(s.GoFiles)
	s.watchDirectories = slices.Clone(s.watchDirectories)
	s.rootScopes = slices.Clone(s.rootScopes)
	return s
}

// Epoch is monotonic and independent of content hashes. Observed A→B→A and
// same-content rewrites remain invalidating even when the final hash is unchanged.
func (t *Tracker) Epoch() uint64 { return t.epoch.Load() }

// Scan serializes all traversals and coalesces callers only when their requested
// checkpoint preceded the shared scan's start and no new dirty event crossed it.
// A caller arriving during a traversal needs a subsequent traversal. Cancellation
// applies both to waiting for the gate and to streaming file reads.
func (t *Tracker) Scan(ctx context.Context) (Snapshot, error) {
	ticket := t.requests.Add(1)
	select {
	case t.gate <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	defer func() { <-t.gate }()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	t.mu.Lock()
	if t.hasLatest && ticket <= t.covered && t.latest.Epoch == t.Epoch() {
		s := cloneSnapshot(t.latest)
		t.mu.Unlock()
		return s, nil
	}
	t.mu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		covered, epoch := t.requests.Load(), t.Epoch()
		snap, discovered, deps, err := t.scan(ctx)
		if err != nil {
			if errors.Is(err, ErrUnstable) {
				continue
			}
			return Snapshot{}, fmt.Errorf("input_scan_failed: %w", err)
		}
		if epoch != t.Epoch() {
			continue
		}
		snap.Epoch = epoch
		t.mu.Lock()
		if epoch != t.Epoch() {
			t.mu.Unlock()
			continue
		}
		t.latest, t.covered, t.hasLatest = cloneSnapshot(snap), covered, true
		if !snap.DiscoveryDegraded {
			t.discovered, t.discoveryDependencies = discovered, deps
		}
		failed := t.syncWatchesLocked(snap)
		mark := t.watchMark
		t.mu.Unlock()
		if failed && mark != nil {
			t.epoch.Add(1)
			mark("watch_registration_failed")
		}
		return snap, nil
	}
	return Snapshot{}, ErrUnstable
}

func (t *Tracker) scan(ctx context.Context) (Snapshot, []string, []string, error) {
	// true marks required explicit/current roots; false is a retained historical
	// discovery root allowed to disappear with ENOENT during degraded discovery.
	roots := map[string]bool{}
	discovered, dependencies, degraded, err := t.discover(ctx)
	if err != nil {
		return Snapshot{}, nil, nil, err
	}
	for _, root := range discovered {
		roots[root] = !degraded
	}
	for _, root := range t.explicitRoots() {
		roots[root] = true
	}
	h := sha256.New()
	field(h, []byte("workspace-content-v2"))
	for _, patterns := range [][]string{t.cfg.Include, t.cfg.Exclude, t.cfg.ExcludeDirs} {
		field(h, []byte(fmt.Sprint(len(patterns))))
		for _, pattern := range patterns {
			field(h, []byte(pattern))
		}
	}
	field(h, []byte(fmt.Sprint(t.cfg.GoDiscovery)))
	field(h, []byte(t.cfg.WorkFile))
	dirs, watchDirs, goFiles := map[string]bool{}, map[string]bool{}, map[string]bool{}
	buffer := make([]byte, 64*1024)
	rootTargets := map[string]bool{}
	var scopes []rootScope
	for _, locator := range keys(roots) {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, nil, nil, err
		}
		field(h, []byte("root"))
		field(h, []byte(locator))
		// Watch the nearest existing locator parent and parents of symlink
		// components, without watching or traversing all unrelated ancestors.
		addExistingParents(locator, watchDirs)
		target, err := filepath.EvalSymlinks(locator)
		if err != nil {
			if !roots[locator] && errors.Is(err, fs.ErrNotExist) {
				field(h, []byte("missing"))
				rootTargets[locator] = true
				scopes = append(scopes, rootScope{locator: locator, target: locator})
				continue
			}
			return Snapshot{}, nil, nil, fmt.Errorf("required root %s: %w", locator, err)
		}
		if err := t.checkRoot(target); err != nil {
			return Snapshot{}, nil, nil, err
		}
		field(h, []byte(target))
		rootTargets[target] = true
		info, err := os.Lstat(target)
		if err != nil {
			return Snapshot{}, nil, nil, err
		}
		scopes = append(scopes, rootScope{locator: locator, target: target, file: !info.IsDir()})
		beforeDirs := map[string]fs.FileInfo{}
		err = filepath.WalkDir(target, func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if errors.Is(walkErr, fs.ErrNotExist) {
					return ErrUnstable
				}
				return walkErr
			}
			rel, err := filepath.Rel(target, path)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				rel = filepath.Base(path)
			}
			rel = filepath.ToSlash(rel)
			if t.excluded(path, rel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				stat, err := d.Info()
				if err != nil {
					return err
				}
				beforeDirs[path] = stat
				dirs[path], watchDirs[path] = true, true
				return nil
			}
			if !t.included(rel) {
				return nil
			}
			field(h, []byte(rel))
			if err := hashEntry(ctx, h, path, false, buffer, nil); err != nil {
				return err
			}
			if strings.HasSuffix(path, ".go") {
				goFiles[path] = true
			}
			return nil
		})
		if err != nil {
			return Snapshot{}, nil, nil, fmt.Errorf("scan %s: %w", locator, err)
		}
		afterRoot, err := os.Lstat(target)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return Snapshot{}, nil, nil, ErrUnstable
			}
			return Snapshot{}, nil, nil, err
		}
		if !os.SameFile(info, afterRoot) || info.Mode().Type() != afterRoot.Mode().Type() {
			return Snapshot{}, nil, nil, ErrUnstable
		}
		for path, before := range beforeDirs {
			after, err := os.Lstat(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return Snapshot{}, nil, nil, ErrUnstable
				}
				return Snapshot{}, nil, nil, err
			}
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				return Snapshot{}, nil, nil, ErrUnstable
			}
		}
		if after, err := filepath.EvalSymlinks(locator); err != nil || after != target {
			return Snapshot{}, nil, nil, ErrUnstable
		}
	}
	for _, path := range dependencies {
		field(h, []byte("discovery-dependency"))
		field(h, []byte(path))
		var observation *discoveryObservation
		if observed, ok := t.discoveryObservations[path]; ok {
			observation = &observed
		}
		if err := hashEntry(ctx, h, path, true, buffer, observation); err != nil {
			return Snapshot{}, nil, nil, err
		}
		addExistingParents(path, watchDirs)
	}
	var digest Fingerprint
	copy(digest[:], h.Sum(nil))
	return Snapshot{Fingerprint: digest, Roots: keys(rootTargets), Directories: keys(dirs), Dependencies: dependencies, GoFiles: keys(goFiles), DiscoveryDegraded: degraded, watchDirectories: keys(watchDirs), rootScopes: scopes}, discovered, dependencies, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Hash file type and contents, never metadata. Metadata comparisons are only
// used to reject an observed concurrent mutation, never to skip reading bytes.
func hashEntry(ctx context.Context, h hash.Hash, path string, allowMissing bool, buffer []byte, observation *discoveryObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if observation != nil && observation.exists {
				return ErrUnstable
			}
			if allowMissing {
				field(h, []byte("missing"))
				return nil
			}
			return ErrUnstable
		}
		return err
	}
	if observation != nil && !observation.exists {
		return ErrUnstable
	}
	if observation != nil && ((observation.content != nil && !before.Mode().IsRegular()) || (observation.linkTarget != nil && before.Mode()&os.ModeSymlink == 0)) {
		return ErrUnstable
	}
	field(h, []byte(before.Mode().Type().String()))
	if before.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		field(h, []byte(target))
		if observation != nil && observation.linkTarget != nil && target != *observation.linkTarget {
			return ErrUnstable
		}
	} else if before.Mode().IsRegular() {
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return ErrUnstable
			}
			return err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
			return ErrUnstable
		}
		content := sha256.New()
		if _, err := io.CopyBuffer(content, contextReader{ctx, f}, buffer); err != nil {
			return err
		}
		after, err := f.Stat()
		if err != nil {
			return err
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return ErrUnstable
		}
		field(h, content.Sum(nil))
		if observation != nil && observation.content != nil {
			var digest Fingerprint
			copy(digest[:], content.Sum(nil))
			if digest != *observation.content {
				return ErrUnstable
			}
		}
	}
	after, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrUnstable
		}
		return err
	}
	if !os.SameFile(before, after) || before.Mode().Type() != after.Mode().Type() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return ErrUnstable
	}
	return nil
}

func addExistingParents(path string, dirs map[string]bool) {
	foundNearest := false
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); !foundNearest && err == nil && info.IsDir() {
			dirs[dir] = true
			foundNearest = true
		}
		if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
			dirs[filepath.Dir(dir)] = true
		}
		if dir == filepath.Dir(dir) {
			return
		}
	}
}

// Watch supplies event hints; polling and dispatch scans must remain enabled.
// Metadata events request a scan but do not invalidate an unchanged fingerprint.
func (t *Tracker) Watch(ctx context.Context, mark func(string)) {
	if mark == nil {
		mark = func(string) {}
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.logger.Warn("watcher unavailable; polling remains active", "error", err)
		t.epoch.Add(1)
		mark("watcher_failure")
		return
	}
	t.mu.Lock()
	if t.watcher != nil {
		t.mu.Unlock()
		_ = w.Close()
		return
	}
	t.watcher, t.watchMark = w, mark
	snap := t.latest
	if !t.hasLatest {
		for _, root := range t.explicitRoots() {
			snap.rootScopes = append(snap.rootScopes, rootScope{locator: root, target: root})
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				snap.watchDirectories = append(snap.watchDirectories, root)
			}
			snap.watchDirectories = append(snap.watchDirectories, filepath.Dir(root))
		}
		t.latest = snap
	}
	failed := t.syncWatchesLocked(snap)
	t.mu.Unlock()
	if failed {
		t.epoch.Add(1)
		mark("watch_registration_failed")
	}
	defer func() { t.mu.Lock(); t.watcher, t.watchMark = nil, nil; t.mu.Unlock(); _ = w.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-w.Errors:
			if !ok {
				if ctx.Err() == nil {
					t.epoch.Add(1)
					mark("watcher_closed")
				}
				return
			}
			t.epoch.Add(1)
			mark("watcher_error_or_overflow")
		case event, ok := <-w.Events:
			if !ok {
				if ctx.Err() == nil {
					t.epoch.Add(1)
					mark("watcher_closed")
				}
				return
			}
			t.observe(event, mark)
		}
	}
}

func (t *Tracker) observe(event fsnotify.Event, mark func(string)) {
	if !t.relevant(event) {
		return
	}
	if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		t.epoch.Add(1)
		mark("filesystem_event")
	} else {
		mark("filesystem_metadata")
	}
}

func (t *Tracker) relevant(event fsnotify.Event) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	name := filepath.Clean(event.Name)
	if slices.Contains(t.latest.Dependencies, name) {
		return true
	}
	for _, scope := range t.latest.rootScopes {
		// Ancestor events matter only for the configured path itself, not for
		// arbitrary siblings encountered by a parent directory watch.
		if within(scope.locator, name) {
			return true
		}
		if !within(name, scope.target) || (scope.file && name != scope.target) {
			continue
		}
		rel, _ := filepath.Rel(scope.target, name)
		if scope.file {
			rel = filepath.Base(name)
		}
		rel = filepath.ToSlash(rel)
		if t.excluded(name, rel) {
			continue
		}
		if slices.Contains(t.latest.Directories, name) {
			return true
		}
		if info, err := os.Lstat(name); err == nil && info.IsDir() {
			return true
		}
		if t.included(rel) {
			return true
		}
	}
	return false
}

func (t *Tracker) syncWatchesLocked(s Snapshot) bool {
	if t.watcher == nil {
		return false
	}
	wanted, current := map[string]bool{}, map[string]bool{}
	for _, dir := range s.watchDirectories {
		wanted[dir] = true
	}
	for _, dir := range t.watcher.WatchList() {
		current[dir] = true
		if !wanted[dir] {
			_ = t.watcher.Remove(dir)
		}
	}
	newFailure := false
	for dir := range wanted {
		if current[dir] {
			continue
		}
		if err := t.watcher.Add(dir); err != nil {
			if !t.failed[dir] {
				newFailure = true
				t.logger.Warn("watch registration failed; polling remains active", "path", dir, "error", err)
			}
			t.failed[dir] = true
		} else {
			delete(t.failed, dir)
		}
	}
	for dir := range t.failed {
		if !wanted[dir] {
			delete(t.failed, dir)
		}
	}
	return newFailure
}
