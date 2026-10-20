package launcher

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func rTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func rWriteExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func rIdentity(t *testing.T, path string) *SelfIdentity {
	t.Helper()
	self, err := CaptureIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func rAssertSpec(t *testing.T, spec Spec, locator, source string) {
	t.Helper()
	if spec.Locator != locator || spec.SelectionSource != source || !reflect.DeepEqual(spec.Args, []string{"mcp"}) {
		t.Fatalf("got spec %#v, want locator=%q source=%q argv=[mcp]", spec, locator, source)
	}
	if !filepath.IsAbs(spec.Locator) {
		t.Fatalf("locator is not absolute: %q", spec.Locator)
	}
}

func TestResolveDefaultSiblingWins(t *testing.T) {
	base := rTempDir(t)
	self := rIdentity(t, rWriteExecutable(t, filepath.Join(base, "install", "proxy")))
	want := rWriteExecutable(t, filepath.Join(base, "install", "gopls"))
	other := rWriteExecutable(t, filepath.Join(base, "PATH", "gopls"))
	cwd := filepath.Join(base, "project")
	rWriteExecutable(t, filepath.Join(cwd, "gopls"))
	t.Chdir(cwd)
	oldArg0 := os.Args[0]
	os.Args[0] = filepath.Join(base, "PATH", "proxy")
	t.Cleanup(func() { os.Args[0] = oldArg0 })
	spec, err := ResolveDefault(self, filepath.Dir(other))
	if err != nil {
		t.Fatal(err)
	}
	rAssertSpec(t, spec, want, "sibling")
}

func TestResolveDefaultPhysicalProxyDirectory(t *testing.T) {
	base := rTempDir(t)
	physical := rWriteExecutable(t, filepath.Join(base, "physical", "proxy"))
	want := rWriteExecutable(t, filepath.Join(base, "physical", "gopls"))
	alias := filepath.Join(base, "aliases", "proxy")
	rWriteExecutable(t, filepath.Join(base, "aliases", "gopls"))
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	self := rIdentity(t, alias)
	if self.Path() != physical {
		t.Fatalf("physical self path=%q, want %q", self.Path(), physical)
	}
	spec, err := ResolveDefault(self, filepath.Dir(alias))
	if err != nil {
		t.Fatal(err)
	}
	rAssertSpec(t, spec, want, "sibling")
}

func TestResolveDefaultHardlinkUsesCurrentDirectory(t *testing.T) {
	base := rTempDir(t)
	original := rWriteExecutable(t, filepath.Join(base, "original", "proxy"))
	rWriteExecutable(t, filepath.Join(base, "original", "gopls"))
	alias := filepath.Join(base, "alias", "proxy")
	want := rWriteExecutable(t, filepath.Join(base, "alias", "gopls"))
	if err := os.Link(original, alias); err != nil {
		t.Fatal(err)
	}
	self := rIdentity(t, alias)
	if self.Path() != alias {
		t.Fatalf("hardlink self path=%q, want current path %q", self.Path(), alias)
	}
	spec, err := ResolveDefault(self, filepath.Dir(original))
	if err != nil {
		t.Fatal(err)
	}
	rAssertSpec(t, spec, want, "sibling")
}

func TestResolveDefaultAbsolutePATHOrder(t *testing.T) {
	base := rTempDir(t)
	self := rIdentity(t, rWriteExecutable(t, filepath.Join(base, "install", "proxy")))
	want := rWriteExecutable(t, filepath.Join(base, "first 空格 $literal", "gopls"))
	other := rWriteExecutable(t, filepath.Join(base, "second", "gopls"))
	path := strings.Join([]string{filepath.Join(base, "absent"), filepath.Dir(want), filepath.Dir(other)}, string(os.PathListSeparator))
	spec, err := ResolveDefault(self, path)
	if err != nil {
		t.Fatal(err)
	}
	rAssertSpec(t, spec, want, "PATH")
}

func TestResolveDefaultSkipsRelativePATH(t *testing.T) {
	base := rTempDir(t)
	self := rIdentity(t, rWriteExecutable(t, filepath.Join(base, "install", "proxy")))
	cwd := filepath.Join(base, "project")
	rWriteExecutable(t, filepath.Join(cwd, "gopls"))
	rWriteExecutable(t, filepath.Join(cwd, "relative", "gopls"))
	t.Chdir(cwd)
	for _, path := range []string{"", ".", "relative", ":.:relative:"} {
		t.Run(path, func(t *testing.T) {
			if spec, err := ResolveDefault(self, path); err == nil {
				t.Fatalf("relative/empty PATH %q selected %#v", path, spec)
			}
		})
	}
	spec, err := ResolveDefault(self, strings.Join([]string{"", ".", "relative", cwd}, string(os.PathListSeparator)))
	if err != nil {
		t.Fatal(err)
	}
	rAssertSpec(t, spec, filepath.Join(cwd, "gopls"), "PATH")
}

func TestResolveDefaultExistingInvalidCandidateStopsSearch(t *testing.T) {
	makers := map[string]func(*testing.T, string, string){
		"broken-symlink": func(t *testing.T, path, _ string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(path), "absent"), path); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path, _ string) {
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, path, _ string) {
			if err := syscall.Mkfifo(path, 0755); err != nil {
				t.Fatal(err)
			}
		},
		"non-executable": func(t *testing.T, path, _ string) {
			if err := os.WriteFile(path, []byte("no execute bit"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"self-symlink": func(t *testing.T, path, self string) {
			if err := os.Symlink(self, path); err != nil {
				t.Fatal(err)
			}
		},
		"self-hardlink": func(t *testing.T, path, self string) {
			if err := os.Link(self, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for _, source := range []string{"sibling", "PATH"} {
		for kind, makeCandidate := range makers {
			t.Run(source+"/"+kind, func(t *testing.T) {
				base := rTempDir(t)
				selfPath := rWriteExecutable(t, filepath.Join(base, "install", "proxy"))
				self := rIdentity(t, selfPath)
				candidateDir := filepath.Dir(selfPath)
				if source == "PATH" {
					candidateDir = filepath.Join(base, "first")
					if err := os.Mkdir(candidateDir, 0755); err != nil {
						t.Fatal(err)
					}
				}
				candidate := filepath.Join(candidateDir, "gopls")
				makeCandidate(t, candidate, selfPath)
				fallback := rWriteExecutable(t, filepath.Join(base, "fallback", "gopls"))
				path := strings.Join([]string{candidateDir, filepath.Dir(fallback)}, string(os.PathListSeparator))
				if spec, err := ResolveDefault(self, path); err == nil {
					t.Fatalf("invalid bound candidate %q fell back to %#v", candidate, spec)
				}
			})
		}
	}
}

func TestResolveDefaultPreservesBackendSymlinkLocator(t *testing.T) {
	for _, source := range []string{"sibling", "PATH"} {
		t.Run(source, func(t *testing.T) {
			base := rTempDir(t)
			self := rIdentity(t, rWriteExecutable(t, filepath.Join(base, "install", "proxy")))
			target := rWriteExecutable(t, filepath.Join(base, "versions", "gopls-v1"))
			candidateDir := filepath.Dir(self.Path())
			if source == "PATH" {
				candidateDir = filepath.Join(base, "bin")
				if err := os.Mkdir(candidateDir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			locator := filepath.Join(candidateDir, "gopls")
			if err := os.Symlink(target, locator); err != nil {
				t.Fatal(err)
			}
			spec, err := ResolveDefault(self, candidateDir)
			if err != nil {
				t.Fatal(err)
			}
			rAssertSpec(t, spec, locator, source)
		})
	}
}

func TestResolveDefaultMissingCandidate(t *testing.T) {
	base := rTempDir(t)
	self := rIdentity(t, rWriteExecutable(t, filepath.Join(base, "install", "proxy")))
	if spec, err := ResolveDefault(self, filepath.Join(base, "absent")); err == nil {
		t.Fatalf("missing gopls selected %#v", spec)
	}
}
