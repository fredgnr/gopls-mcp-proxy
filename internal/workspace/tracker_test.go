package workspace

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func write(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, tr *Tracker) Snapshot {
	t.Helper()
	s, err := tr.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tracker(t *testing.T, cfg Config) *Tracker {
	t.Helper()
	tr, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestFingerprintRules(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*testing.T, string)
		different bool
	}{
		{"go_content", func(t *testing.T, r string) { write(t, filepath.Join(r, "a.go"), "package other\n") }, true},
		{"mtime_only", func(t *testing.T, r string) {
			now := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(r, "go.mod"), now, now); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"dependency_same_mtime", func(t *testing.T, r string) {
			p := filepath.Join(r, "go.mod")
			info, _ := os.Stat(p)
			write(t, p, "module example.com/changed\n\ngo 1.25.0\n")
			if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"go_add", func(t *testing.T, r string) { write(t, filepath.Join(r, "b.go"), "package example\n") }, true},
		{"go_delete", func(t *testing.T, r string) {
			if err := os.Remove(filepath.Join(r, "a.go")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"go_rename", func(t *testing.T, r string) {
			if err := os.Rename(filepath.Join(r, "a.go"), filepath.Join(r, "b.go")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"atomic_go_save", func(t *testing.T, r string) {
			write(t, filepath.Join(r, "tmp"), "package example\n// changed\n")
			if err := os.Rename(filepath.Join(r, "tmp"), filepath.Join(r, "a.go")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"empty_sum_addition", func(t *testing.T, r string) { write(t, filepath.Join(r, "go.sum"), "") }, true},
		{"work_sum_addition", func(t *testing.T, r string) { write(t, filepath.Join(r, "go.work.sum"), "new sum\n") }, true},
		{"resource_content", func(t *testing.T, r string) { write(t, filepath.Join(r, "README.md"), "text") }, true},
		{"ignored_git", func(t *testing.T, r string) { write(t, filepath.Join(r, ".git", "objects", "a.go"), "ignored") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := t.TempDir()
			write(t, filepath.Join(r, "go.mod"), "module example.com/test\n\ngo 1.25.0\n")
			write(t, filepath.Join(r, "a.go"), "package example\n")
			tr := tracker(t, Config{Workspace: r, WorkFile: "off"})
			before := scan(t, tr)
			tc.change(t, r)
			after := scan(t, tr)
			if (before.Fingerprint != after.Fingerprint) != tc.different {
				t.Fatalf("unexpected fingerprint comparison: %x %x", before.Fingerprint, after.Fingerprint)
			}
		})
	}
}

func TestWorkspaceScopeDiscoveryAndChanges(t *testing.T) {
	base := t.TempDir()
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "app")
	other := filepath.Join(base, "other")
	replacement := filepath.Join(base, "replacement")
	unlisted := filepath.Join(base, "unlisted")
	for _, dir := range []string{root, other, replacement, unlisted} {
		write(t, filepath.Join(dir, "go.mod"), "module example.com/"+filepath.Base(dir)+"\n\ngo 1.25.0\n")
		write(t, filepath.Join(dir, "a.go"), "package example\n")
	}
	write(t, filepath.Join(base, "go.work"), "go 1.25.0\nuse (\n./app\n./other\n)\n")
	tr := tracker(t, Config{Workspace: root, ReplaceDirs: []string{replacement}, GoDiscovery: true})
	s := scan(t, tr)
	for _, dir := range []string{root, other, replacement} {
		if !slices.Contains(s.Roots, dir) {
			t.Fatalf("missing root %s: %v", dir, s.Roots)
		}
	}
	write(t, filepath.Join(unlisted, "unseen.go"), "package example\n")
	if scan(t, tr).Fingerprint != s.Fingerprint {
		t.Fatal("scanned unrelated sibling directory")
	}
	write(t, filepath.Join(other, "new.go"), "package example\n")
	if scan(t, tr).Fingerprint == s.Fingerprint {
		t.Fatal("missed external go.work member")
	}
	write(t, filepath.Join(base, "go.work"), "go 1.25.0\nuse ./app\n")
	if slices.Contains(scan(t, tr).Roots, other) {
		t.Fatal("removed go.work member remains tracked")
	}
	if err := os.Remove(filepath.Join(base, "go.work")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidDependencyAndSafeScanBoundaries(t *testing.T) {
	r := t.TempDir()
	write(t, filepath.Join(r, "go.mod"), "module broken\nrequire (invalid\n")
	tr := tracker(t, Config{Workspace: r, WorkFile: "off"})
	if _, err := tr.Scan(context.Background()); err != nil {
		t.Fatalf("raw go.mod bytes rejected: %v", err)
	}
	write(t, filepath.Join(r, "go.mod"), "module example.com/fixed\n\ngo 1.25.0\n")
	scan(t, tr)
	if _, err := New(Config{Workspace: filepath.VolumeName(r) + string(filepath.Separator)}, nil); err == nil {
		t.Fatal("filesystem root accepted")
	}
	write(t, filepath.Join(r, "go.work"), "go 1.25.0\nuse /\n")
	tr = tracker(t, Config{Workspace: r, GoDiscovery: true})
	if _, err := tr.Scan(context.Background()); err == nil {
		t.Fatal("unbounded go.work root accepted")
	}
}

func TestPollingScanFindsDirectoryMoveAndExcludesCache(t *testing.T) {
	r := t.TempDir()
	cache := filepath.Join(r, "cache")
	write(t, filepath.Join(r, "go.mod"), "module example.com/test\n")
	write(t, filepath.Join(cache, "a.go"), "ignored")
	tr := tracker(t, Config{Workspace: r, WorkFile: "off", ExcludeDirs: []string{cache}})
	before := scan(t, tr)
	write(t, filepath.Join(cache, "b.go"), "ignored")
	if scan(t, tr).Fingerprint != before.Fingerprint {
		t.Fatal("cache scanned")
	}
	write(t, filepath.Join(r, "dir", "a.go"), "package example\n")
	s := scan(t, tr)
	if err := os.Rename(filepath.Join(r, "dir"), filepath.Join(r, "moved")); err != nil {
		t.Fatal(err)
	}
	if scan(t, tr).Fingerprint == s.Fingerprint {
		t.Fatal("directory move missed")
	}
}
