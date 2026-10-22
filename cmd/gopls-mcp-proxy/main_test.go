package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/proxy"
)

func TestBackendArgumentBoundary(t *testing.T) {
	raw := []string{"", "空 格", "single'quote", "double\"quote", "$HOME;$(false)", "--unknown", "--help", "--version", "--workspace", "--"}
	args := append([]string{"--watch-root", "/additional", "--", "literal-program"}, raw...)
	o, err := parseOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.mode != explicitBackend || o.program != "literal-program" || !slices.Equal(o.argv, raw) || !slices.Equal(o.watchRoots, []string{"/additional"}) {
		t.Fatalf("arguments changed: %+v", o)
	}
	args[len(args)-1] = "mutated"
	if o.argv[len(o.argv)-1] != "--" {
		t.Fatal("backend arguments were not copied")
	}
}

func TestProxyHelpVersionAndMissingProgram(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}} {
		var output bytes.Buffer
		o, err := parseOptions(args, &output)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !o.help && !o.version {
			t.Fatalf("%v did not exit locally", args)
		}
	}
	for _, args := range [][]string{{"--"}, {"--", ""}, {"program"}, {"mcp"}, {"version"}, {"--unknown"}, {"--debounce", "wrong"}, {"--debounce", "-1s"}, {"--shutdown-timeout", "0s"}, {"--watch-root", ""}, {"--debounce", "-1s", "--", "program"}, {"--shutdown-timeout", "0s", "--", "program"}} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid command %q", args)
		}
	}
}

func TestDefaultGoplsIntent(t *testing.T) {
	o, err := parseOptions(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.mode != defaultGopls || o.program != "" || !slices.Equal(o.argv, []string{"mcp"}) {
		t.Fatalf("incorrect default launch intent: %+v", o)
	}
	if o.cfg != proxy.DefaultConfig() || o.shutdown != 2*time.Second || o.goDiscovery || len(o.watchRoots) != 0 {
		t.Fatalf("default configuration changed: %+v", o)
	}

	args := []string{"--watch-root", "one", "--watch-root", "two", "--include", "**/*.go", "--include", "*.mod", "--exclude", "build/**", "--go-discovery", "--disable-watcher", "--debounce", "500ms", "--max-debounce", "2s", "--shutdown-timeout", "3s"}
	o, err = parseOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.mode != defaultGopls || o.program != "" || !slices.Equal(o.argv, []string{"mcp"}) || !slices.Equal(o.watchRoots, []string{"one", "two"}) || !slices.Equal(o.include, []string{"**/*.go", "*.mod"}) || !slices.Equal(o.exclude, []string{"build/**"}) || !o.goDiscovery || !o.cfg.DisableWatcher || o.cfg.Debounce != 500*time.Millisecond || o.cfg.MaxDebounce != 2*time.Second || o.shutdown != 3*time.Second {
		t.Fatalf("default mode lost options: %+v", o)
	}
	want := proxy.DefaultConfig()
	want.Debounce, want.MaxDebounce, want.DisableWatcher = 500*time.Millisecond, 2*time.Second, true
	if o.cfg != want {
		t.Fatalf("unspecified defaults changed: got %+v, want %+v", o.cfg, want)
	}
	if _, err := parseOptions([]string{"--watch-root", "x"}, io.Discard); err != nil {
		t.Fatalf("legal proxy-only command rejected: %v", err)
	}
}

func TestExplicitProgramIsNotRewritten(t *testing.T) {
	for _, program := range []string{"gopls", "./local backend", "../工具/gopls", "/absolute/program"} {
		o, err := parseOptions([]string{"--", program}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if o.mode != explicitBackend || o.program != program || len(o.argv) != 0 {
			t.Fatalf("explicit program rewritten or default argv inserted: %+v", o)
		}
	}
}

func TestRefreshModeOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		mode string
	}{
		{"default", nil, proxy.RefreshOnChange},
		{"explicit default", []string{"--refresh-mode", "on-change"}, proxy.RefreshOnChange},
		{"default launcher per call", []string{"--refresh-mode", "per-call"}, proxy.RefreshPerCall},
		{"explicit backend per call", []string{"--refresh-mode=per-call", "--", "gopls", "mcp"}, proxy.RefreshPerCall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseOptions(tc.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if o.cfg.RefreshMode != tc.mode || !slices.Equal(o.argv, []string{"mcp"}) {
				t.Fatalf("refresh mode or backend arguments changed: %+v", o)
			}
		})
	}
	// Arguments after -- remain the backend's, even if they match a proxy flag.
	o, err := parseOptions([]string{"--", "program", "--refresh-mode", "unknown"}, io.Discard)
	if err != nil || o.cfg.RefreshMode != proxy.RefreshOnChange || !slices.Equal(o.argv, []string{"--refresh-mode", "unknown"}) {
		t.Fatalf("proxy interpreted backend refresh arguments: %+v, %v", o, err)
	}
	for _, mode := range []string{"", "always", "PER-CALL"} {
		called := false
		err := runWithSetup(context.Background(), []string{"--refresh-mode=" + mode}, io.Discard, func(context.Context, options) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("invalid refresh mode %q entered runtime: err=%v called=%v", mode, err, called)
		}
	}
}

func TestBackendLifecycleOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		mode string
		idle time.Duration
	}{
		{nil, proxy.BackendResident, 10 * time.Second},
		{[]string{"--refresh-mode=per-call", "--backend-lifecycle=on-demand"}, proxy.BackendOnDemand, 10 * time.Second},
		{[]string{"--refresh-mode=per-call", "--backend-lifecycle=on-demand", "--backend-idle-timeout=2s", "--", "gopls", "mcp"}, proxy.BackendOnDemand, 2 * time.Second},
	} {
		o, err := parseOptions(tc.args, io.Discard)
		if err != nil || o.cfg.BackendLifecycle != tc.mode || o.cfg.BackendIdleTimeout != tc.idle {
			t.Fatalf("%q: config=%+v err=%v", tc.args, o.cfg, err)
		}
	}
	for _, args := range [][]string{
		{"--backend-lifecycle=on-demand"},
		{"--backend-lifecycle=unknown"},
		{"--backend-lifecycle="},
		{"--refresh-mode=per-call", "--backend-lifecycle=on-demand", "--backend-idle-timeout=0s"},
		{"--refresh-mode=per-call", "--backend-lifecycle=on-demand", "--backend-idle-timeout=-1s"},
	} {
		called := false
		err := runWithSetup(context.Background(), args, io.Discard, func(context.Context, options) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("invalid lifecycle entered setup: %q err=%v called=%v", args, err, called)
		}
	}
	args := []string{"--backend-lifecycle", "unknown", "--backend-idle-timeout", "0s"}
	o, err := parseOptions(append([]string{"--", "program"}, args...), io.Discard)
	if err != nil || !slices.Equal(o.argv, args) || o.cfg.BackendLifecycle != proxy.BackendResident {
		t.Fatalf("backend arguments interpreted by proxy: %+v %v", o, err)
	}
}

func TestLocalDiagnosticsDoNotEnterRuntimeSetup(t *testing.T) {
	// The injected setup owns every operation that can capture cwd/env or self
	// identity, resolve a backend, create a tracker, run Go, or spawn a process.
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}, {"--help", "--"}, {"--version", "--"}, {"--refresh-mode", "per-call", "--help"}, {"--refresh-mode", "per-call", "--version"}} {
		var output bytes.Buffer
		called := false
		err := runWithSetup(context.Background(), args, &output, func(context.Context, options) error {
			called = true
			return errors.New("runtime setup must not run")
		})
		if err != nil || called || output.Len() == 0 {
			t.Fatalf("%q: err=%v setup=%v output=%q", args, err, called, output.String())
		}
	}
	for _, args := range [][]string{{"mcp"}, {"--"}, {"--", ""}, {"--debounce", "-1s"}} {
		called := false
		err := runWithSetup(context.Background(), args, io.Discard, func(context.Context, options) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("invalid command %q entered setup: err=%v setup=%v", args, err, called)
		}
	}
}

func TestHelpAndVersionWithMissingCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}} {
		if err := run(context.Background(), args); err != nil {
			t.Fatalf("local diagnostic requires runtime context: %v", err)
		}
	}
}

func TestLaunchFailurePrecedesWorkspaceSetup(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	aliases := []string{self}
	dir := t.TempDir()
	for name, link := range map[string]func(string, string) error{"symlink": os.Symlink, "hardlink": os.Link} {
		alias := filepath.Join(dir, name)
		if err := link(self, alias); err != nil {
			t.Fatal(err)
		}
		aliases = append(aliases, alias)
	}
	// The filesystem root is rejected by tracker setup. Seeing the specific
	// launch error here proves that identity/discovery fails before that setup.
	t.Chdir(string(filepath.Separator))
	for _, alias := range aliases {
		if err := run(context.Background(), []string{"--", alias}); err == nil || !strings.Contains(err.Error(), "proxy itself") {
			t.Fatalf("self alias did not fail before workspace setup: %q: %v", alias, err)
		}
	}
	if err := run(context.Background(), []string{"--", filepath.Join(dir, "absent")}); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backend resolution did not fail before workspace setup: %v", err)
	}
}

func TestLegacyFlagsGiveMigrationInstructions(t *testing.T) {
	for _, flag := range []string{"--gopls=x", "--workspace=x", "--replace-dir=x", "-gopls"} {
		if _, err := parseOptions([]string{flag, "--", "program"}, io.Discard); err == nil || !strings.Contains(err.Error(), "was removed") || !strings.Contains(err.Error(), "-- program argv") {
			t.Fatalf("missing migration hint for %s: %v", flag, err)
		}
	}
}

func TestLimitsAndTrackingFlags(t *testing.T) {
	args := []string{"--watch-root", "one", "--watch-root", "two", "--include", "**/*.go", "--exclude", "build/**", "--go-discovery", "--disable-watcher", "--queue-bytes", "8192", "--max-frame-bytes", "4096", "--", "program"}
	o, err := parseOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.watchRoots) != 2 || !o.goDiscovery || !o.cfg.DisableWatcher || o.cfg.QueueBytes != 8192 || o.cfg.MaxFrameBytes != 4096 {
		t.Fatalf("configuration lost: %+v", o)
	}
}

func TestVersionNeedsNoGoOrBackendExecutable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := run(context.Background(), []string{"--version"}); err != nil {
		t.Fatal(err)
	}
}
