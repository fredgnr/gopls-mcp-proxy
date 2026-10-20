package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func identityFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func newTestIdentity(t *testing.T) (*SelfIdentity, string) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "proxy")
	identityFile(t, path)
	self, err := CaptureIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	return self, root
}

func TestCaptureSelfUsesPhysicalExecutable(t *testing.T) {
	self, err := CaptureSelf()
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if self.Path() != want || !filepath.IsAbs(self.Path()) {
		t.Fatalf("physical path = %q, want %q", self.Path(), want)
	}
	if err := self.Check(context.Background(), path); err == nil {
		t.Fatal("actual proxy executable accepted as backend")
	}
}

func TestIdentityRejectsAllSelfAliases(t *testing.T) {
	self, root := newTestIdentity(t)
	symlink := filepath.Join(root, "symlink")
	hardlink := filepath.Join(root, "hardlink")
	if err := os.Symlink(self.Path(), symlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(self.Path(), hardlink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{self.Path(), symlink, hardlink, filepath.Join(root, ".", "proxy")} {
		if err := self.Check(context.Background(), path); err == nil || !strings.Contains(err.Error(), "proxy itself") {
			t.Errorf("Check(%q) = %v, want self rejection", path, err)
		}
	}
	legitimate := filepath.Join(root, "backend")
	identityFile(t, legitimate)
	if err := self.Check(context.Background(), legitimate); err != nil {
		t.Fatalf("independent executable rejected: %v", err)
	}
}

func TestIdentityChecksStartupAndCurrentInodes(t *testing.T) {
	self, root := newTestIdentity(t)
	old := filepath.Join(root, "old-proxy")
	if err := os.Link(self.Path(), old); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	identityFile(t, replacement)
	if err := os.Rename(replacement, self.Path()); err != nil {
		t.Fatal(err)
	}
	newAlias := filepath.Join(root, "new-proxy")
	if err := os.Link(self.Path(), newAlias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old, newAlias} {
		if err := self.Check(context.Background(), path); err == nil || !strings.Contains(err.Error(), "proxy itself") {
			t.Errorf("Check(%q) = %v, want retained/current self rejection", path, err)
		}
	}
	good := filepath.Join(root, "backend")
	identityFile(t, good)
	if err := self.Check(context.Background(), good); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityAllowsBackendUpgradeAndRejectsRetargetedSelf(t *testing.T) {
	self, root := newTestIdentity(t)
	first := filepath.Join(root, "backend-v1")
	second := filepath.Join(root, "backend-v2")
	identityFile(t, first)
	identityFile(t, second)
	locator := filepath.Join(root, "gopls")
	for _, target := range []string{first, second, self.Path()} {
		_ = os.Remove(locator)
		if err := os.Symlink(target, locator); err != nil {
			t.Fatal(err)
		}
		err := self.Check(context.Background(), locator)
		if target == self.Path() {
			if err == nil {
				t.Fatal("retargeted self accepted")
			}
		} else if err != nil {
			t.Fatalf("legitimate upgrade rejected: %v", err)
		}
	}
}

func TestIdentityFailsClosedWhenCurrentSelfUnknown(t *testing.T) {
	for _, corruption := range []string{"missing", "broken-link", "directory", "non-executable"} {
		t.Run(corruption, func(t *testing.T) {
			self, root := newTestIdentity(t)
			backend := filepath.Join(root, "backend")
			identityFile(t, backend)
			if err := os.Remove(self.Path()); err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "broken-link":
				if err := os.Symlink(filepath.Join(root, "missing"), self.Path()); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(self.Path(), 0700); err != nil {
					t.Fatal(err)
				}
			case "non-executable":
				if err := os.WriteFile(self.Path(), []byte("new"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := self.Check(context.Background(), backend); err == nil {
				t.Fatal("unknown current self identity accepted")
			}
		})
	}
}

func TestIdentityRejectsInvalidCandidatesAndCancellation(t *testing.T) {
	self, root := newTestIdentity(t)
	if err := self.Check(context.Background(), ""); err == nil {
		t.Fatal("empty candidate accepted")
	}
	if err := (*SelfIdentity)(nil).Check(context.Background(), self.Path()); err == nil {
		t.Fatal("nil identity accepted")
	}
	if got := (*SelfIdentity)(nil).Path(); got != "" {
		t.Fatalf("nil Path = %q", got)
	}
	missing := filepath.Join(root, "missing")
	if err := self.Check(context.Background(), missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing candidate = %v", err)
	}
	backend := filepath.Join(root, "backend")
	identityFile(t, backend)
	if err := os.Chmod(backend, 0600); err != nil {
		t.Fatal(err)
	}
	if err := self.Check(context.Background(), backend); err == nil {
		t.Fatal("non-executable accepted")
	}
	if err := self.Check(context.Background(), root); err == nil {
		t.Fatal("directory accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	self.fs.stat = func(string) (os.FileInfo, error) { t.Fatal("filesystem accessed after cancellation"); return nil, nil }
	if err := self.Check(ctx, backend); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Check = %v", err)
	}
}

func TestCaptureIdentityErrors(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if _, err := CaptureIdentity(path); err == nil {
			t.Fatalf("CaptureIdentity(%q) accepted", path)
		}
	}
	self, root := newTestIdentity(t)
	plain := filepath.Join(root, "plain")
	if err := os.WriteFile(plain, []byte("plain"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureIdentity(plain); err == nil {
		t.Fatal("non-executable self accepted")
	}
	for _, operation := range []string{"eval", "stat"} {
		t.Run(operation, func(t *testing.T) {
			fs := localFS()
			if operation == "eval" {
				fs.evalSymlinks = func(path string) (string, error) {
					return "", &os.PathError{Op: "readlink", Path: path, Err: os.ErrPermission}
				}
			} else {
				fs.stat = func(path string) (os.FileInfo, error) {
					return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
				}
			}
			if _, err := captureIdentity(self.Path(), fs); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("permission failure = %v", err)
			}
		})
	}
}

// Permission failures are injected system-call results, not chmod assertions
// that silently succeed when the suite runs as root.
func TestResolverAndGuardFailClosedOnPermissionErrors(t *testing.T) {
	for _, operation := range []string{"sibling-lstat", "sibling-stat", "path-lstat", "path-stat", "self-stat"} {
		t.Run(operation, func(t *testing.T) {
			self, root := newTestIdentity(t)
			sibling := filepath.Join(root, "gopls")
			first := filepath.Join(t.TempDir(), "gopls")
			fallback := filepath.Join(t.TempDir(), "gopls")
			identityFile(t, first)
			identityFile(t, fallback)
			blocked := sibling
			if strings.HasPrefix(operation, "path-") {
				blocked = first
			} else {
				identityFile(t, sibling)
			}
			if operation == "self-stat" {
				blocked = self.Path()
			}
			lstat, stat := self.fs.lstat, self.fs.stat
			seen := false
			self.fs.lstat = func(path string) (os.FileInfo, error) {
				if path == fallback {
					t.Fatal("permission error fell back to another candidate")
				}
				if path == blocked && strings.HasSuffix(operation, "lstat") {
					seen = true
					return nil, &os.PathError{Op: "lstat", Path: path, Err: os.ErrPermission}
				}
				return lstat(path)
			}
			self.fs.stat = func(path string) (os.FileInfo, error) {
				if path == blocked && strings.HasSuffix(operation, "stat") && !strings.HasSuffix(operation, "lstat") {
					seen = true
					return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
				}
				return stat(path)
			}
			_, err := ResolveDefault(self, filepath.Dir(first)+string(os.PathListSeparator)+filepath.Dir(fallback))
			if !seen || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("seen=%t error=%v", seen, err)
			}
		})
	}
}
