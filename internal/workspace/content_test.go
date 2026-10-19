package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAllInputKindsAndSameLengthSameMtime(t *testing.T) {
	for _, name := range []string{"main.go", "go.mod", "go.sum", "go.work", "go.work.sum", ".hidden", "vendor/lib/source.go", "assets/page.html", "native/header.h", "native/source.c", "node_modules/package/index.js"} {
		t.Run(name, func(t *testing.T) {
			root := canonicalTempDir(t)
			p := filepath.Join(root, name)
			write(t, p, "input-A")
			tr := tracker(t, Config{Workspace: root})
			before := scan(t, tr)
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			write(t, p, "input-B")
			if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if scan(t, tr).Fingerprint == before.Fingerprint {
				t.Fatalf("missed same-size/same-mtime content in %s", name)
			}
		})
	}
}

func TestGlobScope(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		matches       bool
	}{
		{"*.go", "nested/main.go", true}, {"*.go", "nested/main.ts", false},
		{"src/*.go", "src/main.go", true}, {"src/*.go", "src/sub/main.go", false},
		{"src/**/*.go", "src/main.go", true}, {"src/**/*.go", "src/sub/main.go", true},
		{"**/asset?.[ch]", "asset1.c", true}, {"**/asset?.[ch]", "nested/asset2.h", true},
		{"src/**", "src", true}, {"**/vendor/**", "src/vendor/input", true},
	} {
		if got := globMatch(tc.pattern, tc.path); got != tc.matches {
			t.Errorf("glob(%q, %q)=%v", tc.pattern, tc.path, got)
		}
	}
	for _, bad := range []string{"", "/abs", "../a", "src//a", "a\\b", "a/**b", "!a", "[bad", "C:/a"} {
		if _, err := New(Config{Workspace: canonicalTempDir(t), Include: []string{bad}}, nil); err == nil {
			t.Errorf("accepted malformed glob %q", bad)
		}
	}
	root := canonicalTempDir(t)
	for _, name := range []string{"src/ok.go", "src/generated/skip.go", "src/ignore.go", "src/.git/secret.go", "src/other.txt"} {
		write(t, filepath.Join(root, name), "A")
	}
	tr := tracker(t, Config{Workspace: root, Include: []string{"src/**/*.go"}, Exclude: []string{"generated", "ignore.go"}})
	before := scan(t, tr)
	for _, name := range []string{"src/generated/skip.go", "src/ignore.go", "src/.git/secret.go", "src/other.txt"} {
		write(t, filepath.Join(root, name), "B")
	}
	if scan(t, tr).Fingerprint != before.Fingerprint {
		t.Fatal("excluded contents entered fingerprint")
	}
	write(t, filepath.Join(root, "src/ok.go"), "B")
	if scan(t, tr).Fingerprint == before.Fingerprint {
		t.Fatal("included content missing")
	}
	for _, name := range []string{"src/generated/skip.go", "src/ignore.go", "src/.git/secret.go", "src/other.txt"} {
		if tr.relevant(fsnotify.Event{Name: filepath.Join(root, name), Op: fsnotify.Write}) {
			t.Errorf("excluded event relevant: %s", name)
		}
	}
}

func TestExplicitFileAndScopeIdentity(t *testing.T) {
	root, external := canonicalTempDir(t), canonicalTempDir(t)
	file := filepath.Join(external, "GOENV")
	write(t, file, "value=A")
	tr := tracker(t, Config{Workspace: root, WatchRoots: []string{file}})
	before := scan(t, tr)
	write(t, filepath.Join(external, "unlisted"), "outside scope")
	if scan(t, tr).Fingerprint != before.Fingerprint {
		t.Fatal("explicit file root expanded to sibling files")
	}
	write(t, file, "value=B")
	if scan(t, tr).Fingerprint == before.Fingerprint {
		t.Fatal("external file modification missed")
	}
	withoutRoot := scan(t, tracker(t, Config{Workspace: root}))
	if withoutRoot.Fingerprint == before.Fingerprint {
		t.Fatal("scope missing from fingerprint")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Scan(context.Background()); err == nil {
		t.Fatal("missing explicit root reported fresh")
	}
}

func TestSymlinkContentTypeAndExplicitRootRetarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges are platform configured")
	}
	root, targets := canonicalTempDir(t), canonicalTempDir(t)
	first, second := filepath.Join(targets, "first"), filepath.Join(targets, "second")
	write(t, first, "A")
	write(t, second, "B")
	link := filepath.Join(root, "input")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	tr := tracker(t, Config{Workspace: root})
	a := scan(t, tr)
	write(t, first, "outside changed")
	if scan(t, tr).Fingerprint != a.Fingerprint {
		t.Fatal("interior symlink target was followed")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	b := scan(t, tr)
	if a.Fingerprint == b.Fingerprint {
		t.Fatal("link target text not fingerprinted")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	write(t, link, second)
	if scan(t, tr).Fingerprint == b.Fingerprint {
		t.Fatal("link/file type transition missed")
	}
	// A directory symlink is not recursively traversed, even when it cycles.
	if err := os.Symlink(root, filepath.Join(root, "cycle")); err != nil {
		t.Fatal(err)
	}
	scan(t, tr)
	// Explicit root locators are resolved afresh; target identity matters even
	// when both targets contain identical bytes.
	one, two := filepath.Join(targets, "one"), filepath.Join(targets, "two")
	write(t, filepath.Join(one, "same"), "same")
	write(t, filepath.Join(two, "same"), "same")
	alias := filepath.Join(targets, "alias")
	if err := os.Symlink(one, alias); err != nil {
		t.Fatal(err)
	}
	tr = tracker(t, Config{Workspace: root, WatchRoots: []string{alias}})
	a = scan(t, tr)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(two, alias); err != nil {
		t.Fatal(err)
	}
	b = scan(t, tr)
	if a.Fingerprint == b.Fingerprint || !slices.Contains(b.Roots, two) {
		t.Fatal("explicit symlink redirection missed")
	}
}

func TestObservedEpochSurvivesReversionAndSameContentWrite(t *testing.T) {
	root := canonicalTempDir(t)
	file := filepath.Join(root, "input")
	write(t, file, "A")
	tr := tracker(t, Config{Workspace: root})
	before := scan(t, tr)
	var reasons []string
	mark := func(reason string) { reasons = append(reasons, reason) }
	write(t, file, "B")
	tr.observe(fsnotify.Event{Name: file, Op: fsnotify.Write}, mark)
	write(t, file, "A")
	tr.observe(fsnotify.Event{Name: file, Op: fsnotify.Write}, mark)
	after := scan(t, tr)
	if before.Fingerprint != after.Fingerprint || after.Epoch != before.Epoch+2 {
		t.Fatal("A→B→A lost event invalidation")
	}
	write(t, file, "A")
	tr.observe(fsnotify.Event{Name: file, Op: fsnotify.Write}, mark)
	if tr.Epoch() != after.Epoch+1 {
		t.Fatal("same-content observed write did not invalidate")
	}
	tr.observe(fsnotify.Event{Name: file, Op: fsnotify.Chmod}, mark)
	if tr.Epoch() != after.Epoch+1 || reasons[len(reasons)-1] != "filesystem_metadata" {
		t.Fatal("metadata-only event incremented dirty epoch")
	}
}

func TestDiscoveryDegradationMissingHistoryAndRecovery(t *testing.T) {
	base := canonicalTempDir(t)
	root, other := filepath.Join(base, "app"), filepath.Join(base, "other")
	write(t, filepath.Join(root, "go.mod"), "module app\nunknown future directive\n")
	write(t, filepath.Join(other, "data"), "A")
	work := filepath.Join(base, "go.work")
	write(t, work, "go 1.25.0\nuse (\n./app\n./other\n)\n")
	tr := tracker(t, Config{Workspace: root, GoDiscovery: true})
	good := scan(t, tr)
	if !slices.Contains(good.Roots, other) {
		t.Fatal("external use root not discovered")
	}
	write(t, work, "future unknown syntax\n")
	degraded := scan(t, tr)
	if !degraded.DiscoveryDegraded || !slices.Contains(degraded.Roots, other) || good.Fingerprint == degraded.Fingerprint {
		t.Fatal("invalid discovery lost previous scope or bytes")
	}
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}
	missing := scan(t, tr)
	if missing.Fingerprint == degraded.Fingerprint || !slices.Contains(missing.watchDirectories, base) {
		t.Fatal("historical missing root not recorded/monitored")
	}
	write(t, filepath.Join(other, "data"), "B")
	if scan(t, tr).Fingerprint == missing.Fingerprint {
		t.Fatal("restored historical root missed")
	}
	write(t, work, "go 1.25.0\nuse ./app\n")
	recovered := scan(t, tr)
	if recovered.DiscoveryDegraded || slices.Contains(recovered.Roots, other) {
		t.Fatal("successful discovery did not clear stale roots")
	}
	// A currently confirmed root is required, unlike retained history.
	write(t, work, "go 1.25.0\nuse ./missing\n")
	if _, err := tr.Scan(context.Background()); err == nil {
		t.Fatal("currently confirmed missing root reported fresh")
	}
}

func TestFirstDiscoveryFailureStillHashesArbitraryBytes(t *testing.T) {
	root := canonicalTempDir(t)
	write(t, filepath.Join(root, "go.mod"), "arbitrary\x00future\xff")
	write(t, filepath.Join(root, "go.work"), "malformed (")
	tr := tracker(t, Config{Workspace: root, GoDiscovery: true})
	s := scan(t, tr)
	if !s.DiscoveryDegraded || len(s.Roots) != 1 || s.Roots[0] != root {
		t.Fatal("first discovery failure exceeded explicit guarantee")
	}
	write(t, filepath.Join(root, "go.mod"), "changed raw bytes")
	if scan(t, tr).Fingerprint == s.Fingerprint {
		t.Fatal("invalid Go bytes not fingerprinted")
	}
}

func TestDiscoverySourceCannotChangeAfterParse(t *testing.T) {
	root := canonicalTempDir(t)
	write(t, filepath.Join(root, "go.mod"), "module app\n")
	work := filepath.Join(root, "go.work")
	write(t, work, "go 1.25.0\nuse .\n")
	tr := tracker(t, Config{Workspace: root, GoDiscovery: true})
	_, deps, degraded, err := tr.discover(context.Background())
	if err != nil || degraded || !slices.Contains(deps, work) {
		t.Fatalf("discover failed: %v", err)
	}
	observed := tr.discoveryObservations[work]
	write(t, work, "go 1.25.0\nuse ./changed\n")
	if err := hashEntry(context.Background(), sha256.New(), work, true, make([]byte, 64*1024), &observed); !errors.Is(err, ErrUnstable) {
		t.Fatalf("changed parse source accepted: %v", err)
	}
}

func TestGoWorkSymlinkContentAndMissingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges are platform configured")
	}
	root, external := canonicalTempDir(t), canonicalTempDir(t)
	work := filepath.Join(external, "metadata")
	write(t, filepath.Join(root, "go.mod"), "module app\n")
	write(t, work, "go 1.25.0\nuse .\n")
	if err := os.Symlink(work, filepath.Join(root, "go.work")); err != nil {
		t.Fatal(err)
	}
	tr := tracker(t, Config{Workspace: root, GoDiscovery: true})
	before := scan(t, tr)
	write(t, work, "go 1.25.0\nuse .\n// changed\n")
	if scan(t, tr).Fingerprint == before.Fingerprint {
		t.Fatal("Go discovery followed untracked metadata target")
	}
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	s := scan(t, tr)
	if !s.DiscoveryDegraded {
		t.Fatal("broken discovery symlink did not degrade")
	}
}

func TestPersistentWatchRegistrationFailureStillAllowsPolling(t *testing.T) {
	tr := tracker(t, Config{Workspace: canonicalTempDir(t)})
	scan(t, tr)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// A closed watcher makes every Add fail deterministically, including after
	// retries. Persistent errors must not continually advance the dirty epoch.
	marks := 0
	tr.watcher, tr.watchMark = w, func(string) { marks++ }
	first := scan(t, tr)
	if marks != 1 || first.Epoch == tr.Epoch() {
		t.Fatal("new registration failure did not invalidate once")
	}
	second := scan(t, tr)
	if marks != 1 || second.Epoch != tr.Epoch() {
		t.Fatal("persistent registration failure prevented polling checkpoint")
	}
	write(t, filepath.Join(tr.cfg.Workspace, "input"), "A")
	third := scan(t, tr)
	if third.Fingerprint == second.Fingerprint || third.Epoch != tr.Epoch() {
		t.Fatal("polling fallback missed input after watcher failure")
	}
}

func TestCancelledGateAndCheckpointCoalescing(t *testing.T) {
	tr := tracker(t, Config{Workspace: canonicalTempDir(t)})
	tr.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked scan cancellation: %v", err)
	}
	const callers = 20
	var wg sync.WaitGroup
	results := make(chan Snapshot, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s, err := tr.Scan(context.Background()); results <- s; errs <- err }()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for tr.requests.Load() != callers+1 {
		select {
		case <-deadline.C:
			t.Fatal("callers did not reach checkpoint")
		default:
			runtime.Gosched()
		}
	}
	<-tr.gate
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first Snapshot
	for s := range results {
		if first.Roots == nil {
			first = s
		}
		if s.Fingerprint != first.Fingerprint || s.Epoch != first.Epoch {
			t.Fatal("coalesced callers got incompatible checkpoints")
		}
	}
	if tr.covered != callers+1 {
		t.Fatalf("covered checkpoint = %d", tr.covered)
	}
	// Returned slices do not grant callers access to the published watch state.
	first.Roots[0] = "changed"
	if tr.latest.Roots[0] == "changed" {
		t.Fatal("returned snapshot aliases tracker state")
	}
	write(t, filepath.Join(tr.cfg.Workspace, "new"), "after scan")
	if scan(t, tr).Fingerprint == first.Fingerprint {
		t.Fatal("new caller reused pre-checkpoint scan")
	}
}

type mutationContext struct {
	context.Context
	calls  atomic.Int64
	mutate func(int64)
}

func (c *mutationContext) Err() error { c.mutate(c.calls.Add(1)); return c.Context.Err() }

func TestEpochDuringScanRetriesBoundedly(t *testing.T) {
	root := canonicalTempDir(t)
	write(t, filepath.Join(root, "input"), "A")
	tr := tracker(t, Config{Workspace: root})
	ctx := &mutationContext{Context: context.Background(), mutate: func(int64) { tr.epoch.Add(1) }}
	if _, err := tr.Scan(ctx); !errors.Is(err, ErrUnstable) {
		t.Fatalf("continuous dirty epoch: %v", err)
	}
	if ctx.calls.Load() > 100 {
		t.Fatal("scan retry count was not bounded")
	}
	ctx = &mutationContext{Context: context.Background(), mutate: func(n int64) {
		if n == 4 {
			tr.epoch.Add(1)
		}
	}}
	s, err := tr.Scan(ctx)
	if err != nil || s.Epoch != tr.Epoch() {
		t.Fatalf("single concurrent event not reconciled: %v epoch=%d current=%d", err, s.Epoch, tr.Epoch())
	}
}

func TestStreamingLargeFileAndCancellation(t *testing.T) {
	root := canonicalTempDir(t)
	file := filepath.Join(root, "large.bin")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	block := strings.Repeat("input-data", 8192)
	for i := 0; i < 200; i++ {
		if _, err := io.WriteString(f, block); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	tr := tracker(t, Config{Workspace: root})
	before := scan(t, tr)
	f, err = os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("changed"), int64(len(block)*199)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if scan(t, tr).Fingerprint == before.Fingerprint {
		t.Fatal("large file tail not hashed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	counted := &mutationContext{Context: ctx, mutate: func(n int64) {
		if n == 10 {
			cancel()
		}
	}}
	if _, err := tr.Scan(counted); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream scan not cancellable: %v", err)
	}
}

func TestRequiredUnreadableFileFails(t *testing.T) {
	root := canonicalTempDir(t)
	file := filepath.Join(root, "input")
	write(t, file, "secret")
	tr := tracker(t, Config{Workspace: root})
	scan(t, tr)
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(file, 0600)
	if f, err := os.Open(file); err == nil {
		f.Close()
		t.Skip("runtime can bypass file permissions; non-root CI exercises this assertion")
	}
	if _, err := tr.Scan(context.Background()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable input not rejected: %v", err)
	}
}

func TestRealWatcherDetectsContentAndDirectoryCreation(t *testing.T) {
	root := canonicalTempDir(t)
	file := filepath.Join(root, "input.txt")
	write(t, file, "A")
	tr := tracker(t, Config{Workspace: root})
	scan(t, tr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan string, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Watch(ctx, func(reason string) {
			select {
			case events <- reason:
			default:
			}
		})
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		tr.mu.Lock()
		ready := tr.watcher != nil && slices.Contains(tr.watcher.WatchList(), root)
		tr.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("watch registration timeout")
		}
	}
	before := tr.Epoch()
	write(t, file, "B")
	for tr.Epoch() == before {
		select {
		case <-events:
		case <-deadline.C:
			t.Fatal("content watcher event missing")
		}
	}
	scan(t, tr)
	write(t, filepath.Join(root, "newdir", "input.c"), "C")
	for {
		select {
		case <-events:
			s := scan(t, tr)
			if slices.Contains(s.Directories, filepath.Join(root, "newdir")) {
				cancel()
				<-done
				return
			}
		case <-deadline.C:
			t.Fatal("new directory event missing")
		}
	}
}
