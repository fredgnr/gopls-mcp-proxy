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

	"github.com/fredgnr/gopls-mcp-proxy/internal/launcher"
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

func TestBeforeStartChecksEachAttemptAfterCleanup(t *testing.T) {
	cfg := helperConfig(t, "echo", "original")
	cfg.Env = append(os.Environ(), "RAW_BACKEND_TEST=frozen")
	wantLocator := cfg.Executable
	wantWorkspace := cfg.Workspace
	denied := errors.New("identity no longer allowed")
	allow := true
	checks := 0
	cfg.BeforeStart = func(ctx context.Context, locator string) error {
		checks++
		if locator != wantLocator || ctx.Err() != nil {
			t.Fatalf("guard received locator=%q, context error=%v", locator, ctx.Err())
		}
		if !allow {
			return denied
		}
		return nil
	}
	f, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if f.Locator() != wantLocator {
		t.Fatalf("fixed locator=%q, want %q", f.Locator(), wantLocator)
	}
	// All launch inputs, including the installed hook, were captured already.
	cfg.Executable = "/does-not-exist"
	cfg.Workspace = "/does-not-exist"
	cfg.Args[len(cfg.Args)-1] = "mutated"
	cfg.Env[len(cfg.Env)-1] = "RAW_BACKEND_TEST=mutated"
	cfg.BeforeStart = func(context.Context, string) error { t.Fatal("mutated hook ran"); return nil }
	checkInvocation := func(p *Process) {
		t.Helper()
		var got struct {
			Args []string
			Cwd  string
			Env  string
		}
		if err := json.Unmarshal([]byte(line(t, p)), &got); err != nil {
			t.Fatal(err)
		}
		physicalCWD, err := filepath.EvalSymlinks(wantWorkspace)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Args, []string{"original"}) || got.Cwd != physicalCWD || got.Env != "frozen" {
			t.Fatalf("launch snapshot changed: %+v", got)
		}
	}
	first, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	checkInvocation(first)
	if p, err := f.Start(context.Background()); err == nil || p != nil || checks != 1 {
		t.Fatalf("guard ran before cleanup: process=%v, err=%v, checks=%d", p, err, checks)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	allow = false
	before := bootSequence.Load()
	if p, err := f.Start(context.Background()); !errors.Is(err, denied) || p != nil {
		t.Fatalf("guard failure: process=%v, err=%v", p, err)
	}
	if bootSequence.Load() != before+1 || checks != 2 || f.previous != first {
		t.Fatalf("failed guard did not preserve attempt/publication boundaries: id=%d, checks=%d, previous=%p", bootSequence.Load(), checks, f.previous)
	}
	allow = true
	next, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	checkInvocation(next)
	if checks != 3 || next.ID <= before+1 {
		t.Fatalf("guard was not rerun after repair: id=%d, checks=%d", next.ID, checks)
	}
}

func TestBeforeStartHonorsCancellation(t *testing.T) {
	for _, returnContextError := range []bool{false, true} {
		t.Run(fmt.Sprintf("return-error-%t", returnContextError), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := helperConfig(t, "echo")
			calls := 0
			cfg.BeforeStart = func(received context.Context, _ string) error {
				calls++
				if received != ctx {
					t.Fatal("guard did not receive spawn context")
				}
				cancel()
				if returnContextError {
					return received.Err()
				}
				return nil
			}
			f, err := NewFactory(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if p, err := f.Start(ctx); p != nil || !errors.Is(err, context.Canceled) || f.previous != nil || calls != 1 {
				t.Fatalf("canceled guard published process: process=%v, err=%v, previous=%p, calls=%d", p, err, f.previous, calls)
			}
			if _, err := f.Start(ctx); !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("already canceled attempt reached guard: err=%v, calls=%d", err, calls)
			}
		})
	}
}

func TestExplicitResolverRetainsRelativePATHAndSkipsInvalidCandidates(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"bad", "bin"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bad", "program"), []byte("not executable"), 0644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "bin", "program")
	if err := os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"program", "./bin/program"} {
		f, err := NewFactory(Config{Executable: input, Workspace: dir, Env: []string{"PATH=bad" + string(os.PathListSeparator) + "bin"}})
		if err != nil {
			t.Fatal(err)
		}
		if f.Locator() != want {
			t.Fatalf("explicit %q resolved to %q, want %q", input, f.Locator(), want)
		}
	}
}

func TestInjectedIdentityAllowsHelperAndRejectsReplacedSelf(t *testing.T) {
	dir := t.TempDir()
	selfPath := filepath.Join(dir, "proxy")
	if err := os.WriteFile(selfPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	self, err := launcher.CaptureIdentity(selfPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := helperConfig(t, "echo")
	cfg.BeforeStart = self.Check
	f, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.Start(context.Background())
	if err != nil {
		t.Fatalf("independently injected self identity forbade the test helper: %v", err)
	}
	defer first.Close()
	_ = line(t, first)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// The current self locator can now identify a different inode than the
	// startup snapshot. A fresh check must still reject that identity.
	replacement := filepath.Join(dir, "replacement")
	if err := os.Symlink(cfg.Executable, replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, selfPath); err != nil {
		t.Fatal(err)
	}
	before := bootSequence.Load()
	if p, err := f.Start(context.Background()); p != nil || err == nil || !strings.Contains(err.Error(), "proxy itself") {
		t.Fatalf("current self alias was not rejected before spawn: process=%v, err=%v", p, err)
	}
	if bootSequence.Load() != before+1 || f.previous != first {
		t.Fatal("identity failure did not consume exactly one attempt without publication")
	}
}
