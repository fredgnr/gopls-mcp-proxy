package main

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestBackendArgumentBoundary(t *testing.T) {
	raw := []string{"", "空 格", "single'quote", "double\"quote", "$HOME;$(false)", "--unknown", "--help", "--version", "--workspace", "--"}
	args := append([]string{"--watch-root", "/additional", "--", "literal-program"}, raw...)
	o, err := parseOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.program != "literal-program" || !slices.Equal(o.argv, raw) || !slices.Equal(o.watchRoots, []string{"/additional"}) {
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
	for _, args := range [][]string{nil, {"--"}, {"--", ""}, {"program"}, {"--watch-root", "x"}, {"--debounce", "-1s", "--", "program"}, {"--shutdown-timeout", "0s", "--", "program"}} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid command %q", args)
		}
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
