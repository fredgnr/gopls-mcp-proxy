// Package workspace detects dependency contents and Go source path changes.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/mod/modfile"
)

type Fingerprint [32]byte

func (f Fingerprint) String() string { return fmt.Sprintf("%x", f[:8]) }

type Snapshot struct {
	Fingerprint  Fingerprint
	Roots        []string
	Directories  []string
	Dependencies []string
	GoFiles      []string
}

type Config struct {
	Workspace string
	// WorkFile is empty for automatic ancestor discovery, "off", or an absolute path.
	WorkFile    string
	ReplaceDirs []string
	ExcludeDirs []string
}

type Tracker struct {
	cfg     Config
	gate    chan struct{}
	mu      sync.Mutex
	watcher *fsnotify.Watcher
	latest  Snapshot
	logger  *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Tracker, error) {
	var err error
	cfg.Workspace, err = canonical(cfg.Workspace)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(cfg.Workspace); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace must be an existing directory: %s", cfg.Workspace)
	}
	if cfg.WorkFile != "" && cfg.WorkFile != "off" && !filepath.IsAbs(cfg.WorkFile) {
		return nil, fmt.Errorf("GOWORK must be off or an absolute path")
	}
	for i, dir := range cfg.ReplaceDirs {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cfg.Workspace, dir)
		}
		cfg.ReplaceDirs[i], err = canonical(dir)
		if err != nil {
			return nil, err
		}
	}
	for i, dir := range cfg.ExcludeDirs {
		cfg.ExcludeDirs[i], err = canonical(dir)
		if err != nil {
			return nil, err
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	t := &Tracker{cfg: cfg, gate: make(chan struct{}, 1), logger: logger}
	for _, root := range append([]string{cfg.Workspace}, cfg.ReplaceDirs...) {
		if err := t.checkRoot(root); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if os.IsNotExist(err) {
		return filepath.Clean(abs), nil
	}
	return resolved, err
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (t *Tracker) checkRoot(root string) error {
	home, _ := os.UserHomeDir()
	if root == filepath.Dir(root) || root == home {
		return fmt.Errorf("refusing to scan filesystem root or home directory: %s", root)
	}
	for _, excluded := range t.cfg.ExcludeDirs {
		if within(root, excluded) {
			return fmt.Errorf("scan root is inside excluded Go installation/cache: %s", root)
		}
	}
	return nil
}

func field(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:])
	h.Write(value)
}

func keys[V any](m map[string]V) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	slices.Sort(result)
	return result
}

// Scan never caches dependency contents by mtime. All callers share a cancellable
// gate so concurrent tools do not perform competing full workspace traversals.
func (t *Tracker) Scan(ctx context.Context) (Snapshot, error) {
	select {
	case t.gate <- struct{}{}:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	defer func() { <-t.gate }()
	snap, err := t.scan(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	t.mu.Lock()
	t.latest = snap
	t.syncWatchesLocked(snap)
	t.mu.Unlock()
	return snap, nil
}

func (t *Tracker) scan(ctx context.Context) (Snapshot, error) {
	deps := map[string][]byte{}
	exists := map[string]bool{}
	dirs := map[string]bool{}
	files := map[string]bool{}
	roots := map[string]bool{t.cfg.Workspace: true}
	read := func(path string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if b, ok := deps[path]; ok {
			return b, nil
		}
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read dependency %s: %w", path, err)
		}
		deps[path] = b
		exists[path] = err == nil
		return b, nil
	}
	// Watch only ancestor dependency files, never recursively scan all ancestors.
	// The containing module is a scan root when workspace is a subdirectory.
	foundMod := false
	work := t.cfg.WorkFile
	for dir := t.cfg.Workspace; ; dir = filepath.Dir(dir) {
		if !foundMod {
			path := filepath.Join(dir, "go.mod")
			if _, err := read(path); err != nil {
				return Snapshot{}, err
			}
			if exists[path] {
				roots[dir] = true
				foundMod = true
			}
		}
		if work == "" {
			path := filepath.Join(dir, "go.work")
			if _, err := read(path); err != nil {
				return Snapshot{}, err
			}
			if exists[path] {
				work = path
			}
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	if work != "" && work != "off" {
		b, err := read(work)
		if err != nil {
			return Snapshot{}, err
		}
		if _, err = read(work + ".sum"); err != nil {
			return Snapshot{}, err
		}
		if !exists[work] && t.cfg.WorkFile != "" {
			return Snapshot{}, fmt.Errorf("configured go.work does not exist: %s", work)
		}
		if exists[work] {
			wf, err := modfile.ParseWork(work, b, nil)
			if err != nil {
				return Snapshot{}, fmt.Errorf("parse go.work: %w", err)
			}
			for _, use := range wf.Use {
				root := use.Path
				if !filepath.IsAbs(root) {
					root = filepath.Join(filepath.Dir(work), root)
				}
				root, err = canonical(root)
				if err != nil {
					return Snapshot{}, err
				}
				roots[root] = true
			}
		}
	}
	for _, dir := range t.cfg.ReplaceDirs {
		roots[dir] = true
	}
	for _, root := range keys(roots) {
		if err := t.checkRoot(root); err != nil {
			return Snapshot{}, err
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			return Snapshot{}, fmt.Errorf("scan root unavailable: %s", root)
		}
		// Include missing dependency files in the fingerprint, including empty additions.
		for _, name := range []string{"go.mod", "go.sum", "go.work", "go.work.sum"} {
			if _, err := read(filepath.Join(root, name)); err != nil {
				return Snapshot{}, err
			}
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if path != root && (d.Name() == ".git" || d.Name() == ".hg" || d.Name() == ".svn" || d.Name() == "node_modules") {
					return filepath.SkipDir
				}
				for _, excluded := range t.cfg.ExcludeDirs {
					if within(path, excluded) {
						return filepath.SkipDir
					}
				}
				if dirs[path] {
					return filepath.SkipDir
				}
				dirs[path] = true
				return nil
			}
			// Do not follow directory symlinks: explicit roots are canonicalized above.
			switch d.Name() {
			case "go.mod", "go.sum", "go.work", "go.work.sum":
				b, err := read(path)
				if err != nil {
					return err
				}
				if d.Name() == "go.mod" {
					if _, err := modfile.Parse(path, b, nil); err != nil {
						return fmt.Errorf("parse go.mod: %w", err)
					}
				}
			default:
				if strings.HasSuffix(d.Name(), ".go") {
					files[path] = true
				}
			}
			return nil
		})
		if err != nil {
			return Snapshot{}, fmt.Errorf("scan %s: %w", root, err)
		}
	}
	depHash, goHash := sha256.New(), sha256.New()
	for _, path := range keys(deps) {
		field(depHash, []byte(path))
		if exists[path] {
			field(depHash, []byte{1})
		} else {
			field(depHash, []byte{0})
		}
		field(depHash, deps[path])
	}
	for _, path := range keys(files) {
		field(goHash, []byte(path))
	}
	combined := append(depHash.Sum(nil), goHash.Sum(nil)...)
	return Snapshot{Fingerprint: sha256.Sum256(combined), Roots: keys(roots), Directories: keys(dirs), Dependencies: keys(deps), GoFiles: keys(files)}, nil
}

// Watch marks events only; the supervisor always verifies the final fingerprint.
// Polling and dispatch checks remain active even when notifications are unavailable.
func (t *Tracker) Watch(ctx context.Context, mark func(string)) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.logger.Warn("watcher unavailable; polling remains active", "error", err)
		mark("watcher_failure")
		return
	}
	t.mu.Lock()
	t.watcher = w
	t.syncWatchesLocked(t.latest)
	t.mu.Unlock()
	defer func() { t.mu.Lock(); t.watcher = nil; t.mu.Unlock(); w.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
			mark("watcher_error_or_overflow")
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if t.relevant(event) {
				mark("filesystem_event")
			}
		}
	}
}

func (t *Tracker) relevant(e fsnotify.Event) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if slices.Contains(t.latest.Dependencies, e.Name) {
		return true
	}
	for _, root := range t.latest.Roots {
		if !within(e.Name, root) {
			continue
		}
		switch filepath.Base(e.Name) {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			return true
		}
		if strings.HasSuffix(e.Name, ".go") {
			return true
		}
		if slices.Contains(t.latest.Directories, e.Name) {
			return true
		}
		if e.Has(fsnotify.Create) {
			if info, err := os.Stat(e.Name); err == nil && info.IsDir() {
				return true
			}
		}
	}
	return false
}

func (t *Tracker) syncWatchesLocked(s Snapshot) {
	if t.watcher == nil {
		return
	}
	wanted := map[string]bool{}
	for _, dir := range s.Directories {
		wanted[dir] = true
	}
	for _, dep := range s.Dependencies {
		wanted[filepath.Dir(dep)] = true
	}
	current := map[string]bool{}
	for _, dir := range t.watcher.WatchList() {
		current[dir] = true
		if !wanted[dir] {
			_ = t.watcher.Remove(dir)
		}
	}
	for dir := range wanted {
		if !current[dir] {
			if err := t.watcher.Add(dir); err != nil {
				t.logger.Warn("watch registration failed; polling remains active", "path", dir, "error", err)
			}
		}
	}
}
