package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// No MCP SDK or handshake is involved: the process layer transports arbitrary
// bytes, and the higher protocol layer decides which bytes are legitimate MCP.
func TestRawProcessHelper(t *testing.T) {
	marker := slices.Index(os.Args, "raw-backend-helper")
	if marker < 0 {
		return
	}
	args := os.Args[marker+1:]
	switch args[0] {
	case "echo":
		cwd, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": args[1:], "cwd": cwd, "env": os.Getenv("RAW_BACKEND_TEST"), "gowork": os.Getenv("GOWORK"), "goenv": os.Getenv("GOENV")})
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "hang":
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, "ready")
		for {
			time.Sleep(time.Hour)
		}
	case "exit":
		os.Exit(7)
	case "descendant":
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestRawProcessHelper$", "--", "raw-backend-helper", "descendant")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	os.Exit(0)
}

func helperConfig(t *testing.T, mode string, extra ...string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Executable: exe, Args: append([]string{"-test.run=^TestRawProcessHelper$", "--", "raw-backend-helper", mode}, extra...), Workspace: t.TempDir(), ShutdownTimeout: 40 * time.Millisecond, CleanupTimeout: time.Second, Stderr: io.Discard, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}
func launchHelper(t *testing.T, cfg Config) *Process {
	t.Helper()
	f, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}
func line(t *testing.T, p *Process) string {
	t.Helper()
	out := make(chan string, 1)
	go func() { s, _ := bufio.NewReader(p.Stdout).ReadString('\n'); out <- s }()
	select {
	case value := <-out:
		return strings.TrimSpace(value)
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not respond")
		return ""
	}
}

func TestRawArgumentsEnvironmentAndWorkingDirectory(t *testing.T) {
	args := []string{"--unknown", "", "空 格", "quote\"'", "$HOME; $(false)", "--help", "--version", "--"}
	cfg := helperConfig(t, "echo", args...)
	cfg.Env = append(os.Environ(), "RAW_BACKEND_TEST=frozen", "GOWORK=/literal/work", "GOENV=/literal/env")
	f, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Mutation after factory construction must not alter later spawns.
	cfg.Args[len(cfg.Args)-1] = "changed"
	cfg.Env[len(cfg.Env)-1] = "GOENV=changed"
	p, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var got struct {
		Args                    []string
		Cwd, Env, Gowork, Goenv string
	}
	if err := json.Unmarshal([]byte(line(t, p)), &got); err != nil {
		t.Fatal(err)
	}
	actualCWD, _ := filepath.EvalSymlinks(cfg.Workspace)
	if !slices.Equal(got.Args, args) || got.Cwd != actualCWD || got.Env != "frozen" || got.Gowork != "/literal/work" || got.Goenv != "/literal/env" {
		t.Fatalf("invocation changed: %+v", got)
	}
	payload := "raw bytes without protocol interpretation\x00\n"
	if _, err := io.WriteString(p.Stdin, payload); err != nil {
		t.Fatal(err)
	}
	gotBytes := make([]byte, len(payload))
	if _, err := io.ReadFull(p.Stdout, gotBytes); err != nil || string(gotBytes) != payload {
		t.Fatalf("raw IO: %q %v", gotBytes, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done:
	default:
		t.Fatal("Close returned without Wait")
	}
}

func TestIgnoredTERMAndBlockedWritesAreBounded(t *testing.T) {
	p := launchHelper(t, helperConfig(t, "hang"))
	if got := line(t, p); got != "ready" {
		t.Fatalf("got %q", got)
	}
	writeDone := make(chan error, 1)
	go func() { _, err := p.Stdin.Write(make([]byte, 8<<20)); writeDone <- err }()
	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cleanup exceeded budget")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("unexpected complete blocked write")
		}
	case <-time.After(time.Second):
		t.Fatal("closing stdin did not release writer")
	}
	select {
	case <-p.Done:
	default:
		t.Fatal("process not reaped")
	}
	if p.Err() == nil {
		t.Fatal("killed process lost Wait error")
	}
}

func TestCrashAndConcurrentClose(t *testing.T) {
	p := launchHelper(t, helperConfig(t, "exit"))
	select {
	case <-p.Done:
	case <-time.After(3 * time.Second):
		t.Fatal("exit not observed")
	}
	var ee *exec.ExitError
	if !errors.As(p.Err(), &ee) || ee.ExitCode() != 7 {
		t.Fatalf("Wait result: %v", p.Err())
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestFailedSpawnConsumesBootIdentity(t *testing.T) {
	f, err := NewFactory(helperConfig(t, "exit"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled spawn: %v", err)
	}
	next, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.ID <= first.ID+1 {
		t.Fatalf("failed spawn did not consume ID: %d -> %d", first.ID, next.ID)
	}
}

func TestExecutableLocatorAcceptsSymlinkUpgrade(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	// The OS executes these scripts directly using their shebang. The proxy
	// neither constructs nor invokes a shell command string.
	for path, body := range map[string]string{first: "#!/bin/sh\nprintf 'first\\n'\n", second: "#!/bin/sh\nprintf 'second\\n'\n"} {
		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	locator := filepath.Join(dir, "backend")
	if err := os.Symlink(first, locator); err != nil {
		t.Fatal(err)
	}
	f, err := NewFactory(Config{Executable: "backend", Env: []string{"PATH=" + dir}, Workspace: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := line(t, p); got != "first" {
		t.Fatalf("got %q", got)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.Symlink(second, replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, locator); err != nil {
		t.Fatal(err)
	}
	q, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := line(t, q); got != "second" {
		t.Fatalf("old target remained fixed: %q", got)
	}
	if p.Digest == q.Digest {
		t.Fatal("new image digest not observed")
	}
}
