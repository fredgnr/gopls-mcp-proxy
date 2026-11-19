package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestIncludeDirectoryPrefixes(t *testing.T) {
	for _, tc := range []struct {
		pattern, directory string
		want               bool
	}{
		{"*.go", "docs/nested", true}, {"**", "anywhere", true},
		{"src/*.go", "src", true}, {"src/*.go", "src/sub", false},
		{"src/*.go", "src/empty.go", false}, {"src/*.go", "docs", false},
		{"src/**/*.go", "src/sub", true}, {"src/**", "src/sub", true},
		{"**/src/*.go", "other/src/sub", true},
		{"src/**/test/**/a?.[ch]", "src/deep/test/more", true},
		{"src/[ab]/*.go", "src/c", false}, {"src/[ab]/*.go", "src/a", true},
		{"src/**/test/*.go", "src/a/test/deep", true},
	} {
		t.Run(tc.pattern+"/"+tc.directory, func(t *testing.T) {
			if got := globMatchPath(tc.pattern, tc.directory, true); got != tc.want {
				t.Fatalf("prefix(%q, %q) = %v, want %v", tc.pattern, tc.directory, got, tc.want)
			}
		})
	}
}

func TestIncludeFileChangesWithoutWatcher(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*testing.T, string)
		changed bool
	}{
		{"modify", func(t *testing.T, r string) {
			name := filepath.Join(r, "src/pkg/main.go")
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			write(t, name, "B")
			if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"new_nested_file", func(t *testing.T, r string) { write(t, filepath.Join(r, "src/new/deep/new.go"), "A") }, true},
		{"delete", func(t *testing.T, r string) { removeIncludePath(t, filepath.Join(r, "src/pkg/main.go")) }, true},
		{"rename_out", func(t *testing.T, r string) { renameIncludePath(t, r, "src/pkg/main.go", "src/pkg/main.txt") }, true},
		{"rename_in", func(t *testing.T, r string) { renameIncludePath(t, r, "src/pkg/readme.txt", "src/pkg/added.go") }, true},
		{"move_directory_out", func(t *testing.T, r string) { renameIncludePath(t, r, "src/pkg", "docs/pkg") }, true},
		{"move_directory_in", func(t *testing.T, r string) { renameIncludePath(t, r, "docs", "src/imported") }, true},
		{"delete_directory", func(t *testing.T, r string) { removeIncludePath(t, filepath.Join(r, "src/pkg")) }, true},
		{"atomic_save", func(t *testing.T, r string) {
			write(t, filepath.Join(r, "temporary"), "B")
			renameIncludePath(t, r, "temporary", "src/pkg/main.go")
		}, true},
		{"second_pattern", func(t *testing.T, r string) { write(t, filepath.Join(r, "go.mod"), "B") }, true},
		{"unmatched_content", func(t *testing.T, r string) { write(t, filepath.Join(r, "src/pkg/readme.txt"), "B") }, false},
		{"unmatched_new_tree", func(t *testing.T, r string) { write(t, filepath.Join(r, "cache/new/ignored.go"), "B") }, false},
		{"empty_directory", func(t *testing.T, r string) {
			if err := os.MkdirAll(filepath.Join(r, "src/new/empty.go"), 0700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"excluded", func(t *testing.T, r string) { write(t, filepath.Join(r, "src/generated/new.go"), "B") }, false},
		{"vcs", func(t *testing.T, r string) { write(t, filepath.Join(r, "src/.git/new.go"), "B") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			for _, name := range []string{"src/pkg/main.go", "src/pkg/readme.txt", "docs/other.go", "go.mod"} {
				write(t, filepath.Join(root, name), "A")
			}
			tr := tracker(t, Config{Workspace: root, Include: []string{"src/**/*.go", "go.mod"}, Exclude: []string{"generated"}})
			before := scan(t, tr)
			tc.change(t, root)
			after := scan(t, tr)
			if (before.Fingerprint != after.Fingerprint) != tc.changed {
				t.Fatalf("fingerprint changed = %v, want %v", before.Fingerprint != after.Fingerprint, tc.changed)
			}
		})
	}
}

func removeIncludePath(t *testing.T, name string) {
	t.Helper()
	if err := os.RemoveAll(name); err != nil {
		t.Fatal(err)
	}
}

func renameIncludePath(t *testing.T, root, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(root, from), filepath.Join(root, to)); err != nil {
		t.Fatal(err)
	}
}

func TestIncludeEventInvalidation(t *testing.T) {
	root := canonicalTempDir(t)
	for _, name := range []string{"src/pkg/main.go", "src/pkg/readme.txt", "docs/other.go", "src/generated/no.go"} {
		write(t, filepath.Join(root, name), "A")
	}
	if err := os.MkdirAll(filepath.Join(root, "src/empty.go"), 0700); err != nil {
		t.Fatal(err)
	}
	tr := tracker(t, Config{Workspace: root, Include: []string{"src/**/*.go"}, Exclude: []string{"generated"}})
	scan(t, tr)
	removeIncludePath(t, filepath.Join(root, "src/pkg/main.go"))
	for _, tc := range []struct {
		name  string
		op    fsnotify.Op
		dirty bool
	}{
		{"src/pkg/main.go", fsnotify.Remove, true},
		{"src/pkg/new.go", fsnotify.Create, true},
		{"src/pkg/main.go", fsnotify.Write, true},
		{"src/pkg/main.go", fsnotify.Chmod, false},
		{"src/pkg/readme.txt", fsnotify.Write, false},
		{"src/pkg/missing.txt", fsnotify.Remove, false},
		{"docs/other.go", fsnotify.Write, false},
		{"src/generated/no.go", fsnotify.Write, false},
		{"src/.git/new.go", fsnotify.Create, false},
		{"src/pkg", fsnotify.Write, false},
		{"src/pkg", fsnotify.Rename, true},
		{"src/empty.go", fsnotify.Create, false},
		{"src/empty.go", fsnotify.Remove, false},
		{".", fsnotify.Write, false},
	} {
		before := tr.Epoch()
		tr.observe(fsnotify.Event{Name: filepath.Join(root, tc.name), Op: tc.op}, func(string) {})
		if (tr.Epoch() != before) != tc.dirty {
			t.Errorf("%s %s: dirty = %v, want %v", tc.name, tc.op, tr.Epoch() != before, tc.dirty)
		}
	}
	// The removed tree only produces a directory rename on some platforms.
	removeIncludePath(t, filepath.Join(root, "src/pkg"))
	before := tr.Epoch()
	tr.observe(fsnotify.Event{Name: filepath.Join(root, "src/pkg"), Op: fsnotify.Rename}, func(string) {})
	if tr.Epoch() == before {
		t.Fatal("lost included descendants of removed directory")
	}
}

func TestIncludePrunesUnrelatedDirectories(t *testing.T) {
	root := canonicalTempDir(t)
	for _, name := range []string{"src/pkg/main.go", "docs/nested/other.go", "src/empty.go/ignored.txt"} {
		write(t, filepath.Join(root, name), "A")
	}
	tr := tracker(t, Config{Workspace: root, Include: []string{"src/*.go"}})
	s := scan(t, tr)
	if len(s.GoFiles) != 0 {
		t.Fatal("nonmatching subtree entered whitelist")
	}
	for _, name := range []string{"docs", "src/pkg", "src/empty.go"} {
		if slices.Contains(s.watchDirectories, filepath.Join(root, name)) {
			t.Fatalf("unnecessary watch: %s", name)
		}
	}
	removeIncludePath(t, filepath.Join(root, "src/empty.go"))
	before := tr.Epoch()
	tr.observe(fsnotify.Event{Name: filepath.Join(root, "src/empty.go"), Op: fsnotify.Remove}, func(string) {})
	if tr.Epoch() != before {
		t.Fatal("pruned directory was mistaken for a matching file")
	}
	write(t, filepath.Join(root, "src/new.go"), "A")
	if scan(t, tr).Fingerprint == s.Fingerprint {
		t.Fatal("initially empty whitelist missed new file")
	}
}

func TestIncludeExplicitRoots(t *testing.T) {
	root, external := canonicalTempDir(t), canonicalTempDir(t)
	file := filepath.Join(external, "unmatched.txt")
	write(t, file, "A")
	write(t, filepath.Join(external, "src/a.go"), "A")
	tr := tracker(t, Config{Workspace: root, WatchRoots: []string{external, file}, Include: []string{"src/*.go"}})
	before := scan(t, tr)
	write(t, file, "B")
	tr.observe(fsnotify.Event{Name: file, Op: fsnotify.Write}, func(string) {})
	if tr.Epoch() != before.Epoch || scan(t, tr).Fingerprint != before.Fingerprint {
		t.Fatal("file root bypassed whitelist")
	}
	write(t, filepath.Join(external, "src/a.go"), "B")
	if scan(t, tr).Fingerprint == before.Fingerprint {
		t.Fatal("additional root did not use root-relative patterns")
	}
}

func TestIncludeGoDiscovery(t *testing.T) {
	base := canonicalTempDir(t)
	root, other := filepath.Join(base, "app"), filepath.Join(base, "other")
	write(t, filepath.Join(root, "main.go"), "A")
	write(t, filepath.Join(root, "go.mod"), "module app\n")
	write(t, filepath.Join(other, "other.go"), "A")
	work := filepath.Join(base, "go.work")
	write(t, work, "go 1.25.0\nuse (\n./app\n./other\n)\n")
	tr := tracker(t, Config{Workspace: root, GoDiscovery: true, Include: []string{"*.go"}})
	before := scan(t, tr)
	if len(before.Dependencies) != 0 || slices.Contains(before.Roots, other) {
		t.Fatal("unmatched metadata expanded whitelist")
	}
	write(t, work, "unmatched broken metadata")
	write(t, filepath.Join(root, "go.mod"), "changed")
	tr.observe(fsnotify.Event{Name: work, Op: fsnotify.Write}, func(string) {})
	if after := scan(t, tr); after.Fingerprint != before.Fingerprint || after.Epoch != before.Epoch {
		t.Fatal("unmatched discovery input invalidated")
	}
	write(t, work, "go 1.25.0\nuse (\n./app\n./other\n)\n")
	tr = tracker(t, Config{Workspace: root, GoDiscovery: true, Include: []string{"*.go", "go.work"}})
	before = scan(t, tr)
	if !slices.Contains(before.Roots, other) {
		t.Fatal("included go.work did not discover workspace member")
	}
	write(t, work+".sum", "unmatched")
	if scan(t, tr).Fingerprint != before.Fingerprint {
		t.Fatal("unmatched go.work.sum invalidated")
	}
	write(t, filepath.Join(other, "other.go"), "B")
	if scan(t, tr).Fingerprint == before.Fingerprint {
		t.Fatal("discovered matching file not monitored")
	}
}

func TestIncludeRealWatcherNewDirectory(t *testing.T) {
	root := canonicalTempDir(t)
	tr := tracker(t, Config{Workspace: root, Include: []string{"src/**/*.go"}})
	scan(t, tr)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan string, 256)
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
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		tr.mu.Lock()
		ready := tr.watcher != nil && slices.Contains(tr.watcher.WatchList(), root)
		tr.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watch registration timeout")
		}
		time.Sleep(time.Millisecond)
	}
	// The initial whitelist is empty and no src directory exists. The only
	// initial notification can be for a nonmatching directory name.
	file := filepath.Join(root, "src/new/deep/input.go")
	write(t, file, "A")
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("directory discovery hint missing")
	}
	var s Snapshot
	for {
		var err error
		s, err = tr.Scan(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrUnstable) || time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	if !slices.Contains(s.GoFiles, file) {
		t.Fatal("new nested matching file was not discovered")
	}
	// A same-byte write proves that the new directory acquired a real watch,
	// rather than merely being picked up by content polling.
	before := tr.Epoch()
	write(t, file, "A")
	for tr.Epoch() == before {
		select {
		case <-events:
		case <-time.After(5 * time.Second):
			t.Fatal("new directory was not watched")
		}
	}
}
