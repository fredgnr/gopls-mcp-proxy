package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/backend"
	"github.com/fredgnr/gopls-mcp-proxy/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type quietTracker struct{ *workspace.Tracker }

func (q quietTracker) Watch(ctx context.Context, _ func(string)) { <-ctx.Done() }

type fakeSession struct {
	done  chan struct{}
	once  sync.Once
	value string
	tools []*mcp.Tool
	call  func(context.Context, *mcp.CallToolParams, func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error)
}

func (f *fakeSession) Tools() []*mcp.Tool    { return f.tools }
func (f *fakeSession) Instructions() string  { return "fixture instructions" }
func (f *fakeSession) Done() <-chan struct{} { return f.done }
func (f *fakeSession) Close()                { f.once.Do(func() { close(f.done) }) }
func (f *fakeSession) Call(ctx context.Context, p *mcp.CallToolParams, n func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
	if f.call != nil {
		return f.call(ctx, p, n)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: f.value}}, StructuredContent: map[string]any{"snapshot": f.value}}, nil
}
func newFake(value string) *fakeSession {
	return &fakeSession{done: make(chan struct{}), value: value, tools: []*mcp.Tool{{Name: "probe", Description: "fixture", InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}}}
}

func put(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) (string, *workspace.Tracker, Config) {
	t.Helper()
	root := t.TempDir()
	put(t, filepath.Join(root, "go.mod"), "module example.com/first\n\ngo 1.25.0\n")
	put(t, filepath.Join(root, "a.go"), "package first\n")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr, err := workspace.New(workspace.Config{Workspace: root, WorkFile: "off"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Debounce = 25 * time.Millisecond
	cfg.PollInterval = time.Hour
	cfg.DrainTimeout = 200 * time.Millisecond
	cfg.StartupTimeout = time.Second
	cfg.RequestTimeout = 2 * time.Second
	cfg.RetryMin = 20 * time.Millisecond
	cfg.RetryMax = 100 * time.Millisecond
	cfg.Logger = logger
	return root, tr, cfg
}
func startProxy(t *testing.T, cfg Config, tr Tracker, factory backend.Factory) *Supervisor {
	t.Helper()
	s, err := New(context.Background(), cfg, tr, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := s.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}
func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func call(t *testing.T, s *Supervisor) *mcp.CallToolResult {
	t.Helper()
	result, err := s.Call(context.Background(), &mcp.CallToolParams{Name: "probe"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func textOf(r *mcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if c, ok := r.Content[0].(*mcp.TextContent); ok {
		return c.Text
	}
	return ""
}

func TestMissingEventsTwoDependencyUpdatesSingleClient(t *testing.T) {
	root, tr, cfg := fixture(t)
	var starts atomic.Int32
	s := startProxy(t, cfg, quietTracker{tr}, func(ctx context.Context, roots []string) (backend.Session, error) {
		starts.Add(1)
		data, err := os.ReadFile(filepath.Join(root, "go.mod"))
		return newFake(string(data)), err
	})
	tools, instructions, err := s.WaitReady(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server := s.Server(tools, instructions)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), ct, &mcp.ClientSessionOptions{ProtocolVersion: backend.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	for i, name := range []string{"second", "third"} {
		put(t, filepath.Join(root, "go.mod"), "module example.com/"+name+"\n\ngo 1.25.0\n")
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe"})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError || !strings.Contains(textOf(result), name) {
			t.Fatalf("unexpected result: %+v", result)
		}
		if s.Status().Generation != uint64(i+2) {
			t.Fatalf("generation=%d", s.Status().Generation)
		}
		if err := cs.Ping(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 3 {
		t.Fatalf("starts=%d", starts.Load())
	}
}

func TestAtomicSaveAndGeneratorBurst(t *testing.T) {
	root, tr, cfg := fixture(t)
	s := startProxy(t, cfg, quietTracker{tr}, func(context.Context, []string) (backend.Session, error) { return newFake("ok"), nil })
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	s.Mark("atomic_delete")
	put(t, filepath.Join(root, "a.go"), "package changed\n")
	s.Mark("atomic_create")
	if result := call(t, s); result.IsError {
		t.Fatal(textOf(result))
	}
	if s.Status().Generation != 1 {
		t.Fatal("atomic save rebuilt backend")
	}
	for i := 0; i < 300; i++ {
		put(t, filepath.Join(root, fmt.Sprintf("generated_%d.go", i)), "package changed\n")
		s.Mark("generator")
	}
	if result := call(t, s); result.IsError {
		t.Fatal(textOf(result))
	}
	if s.Status().Generation != 2 {
		t.Fatalf("burst generation=%d", s.Status().Generation)
	}
}

func TestOldResultDiscardedWithoutReplay(t *testing.T) {
	root, tr, cfg := fixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := startProxy(t, cfg, quietTracker{tr}, func(context.Context, []string) (backend.Session, error) {
		f := newFake("old")
		f.call = func(ctx context.Context, _ *mcp.CallToolParams, _ func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
			calls.Add(1)
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "old"}}}, nil
		}
		return f, nil
	})
	resultCh := make(chan *mcp.CallToolResult, 1)
	go func() { r, _ := s.Call(context.Background(), &mcp.CallToolParams{Name: "probe"}, nil); resultCh <- r }()
	<-entered
	put(t, filepath.Join(root, "go.mod"), "module example.com/new\n")
	close(release)
	r := <-resultCh
	if !r.IsError || !strings.Contains(textOf(r), "PROXY_STALE_RESULT") {
		t.Fatalf("stale data escaped: %+v", r)
	}
	if calls.Load() != 1 {
		t.Fatal("dispatched request was replayed")
	}
}

func TestChangesDuringStartAreNeverPublished(t *testing.T) {
	root, tr, cfg := fixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	var discarded *fakeSession
	s := startProxy(t, cfg, quietTracker{tr}, func(ctx context.Context, _ []string) (backend.Session, error) {
		n := attempts.Add(1)
		f := newFake(fmt.Sprint(n))
		if n == 2 {
			discarded = f
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f, nil
	})
	put(t, filepath.Join(root, "new.go"), "package first\n")
	s.Mark("first_change")
	<-started
	put(t, filepath.Join(root, "another.go"), "package first\n")
	close(release)
	waitFor(t, func() bool { return s.Status().Ready && s.Status().Generation == 2 })
	if attempts.Load() != 3 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
	select {
	case <-discarded.Done():
	default:
		t.Fatal("unstable candidate not closed")
	}
}

func TestQueueCancellationLimitsAndPingDuringRebuild(t *testing.T) {
	root, tr, cfg := fixture(t)
	cfg.QueueLimit = 1
	cfg.RequestTimeout = 250 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	s := startProxy(t, cfg, quietTracker{tr}, func(ctx context.Context, _ []string) (backend.Session, error) {
		if starts.Add(1) == 2 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return newFake("ok"), nil
	})
	tools, instructions, _ := s.WaitReady(context.Background())
	st, ct := mcp.NewInMemoryTransports()
	ss, _ := s.Server(tools, instructions).Connect(context.Background(), st, nil)
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, &mcp.ClientSessionOptions{ProtocolVersion: backend.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	put(t, filepath.Join(root, "new.go"), "package first\n")
	s.Mark("change")
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Call(ctx, &mcp.CallToolParams{Name: "probe"}, nil); done <- err }()
	waitFor(t, func() bool { return s.Status().Queued == 1 })
	if r := call(t, s); !r.IsError || !strings.Contains(textOf(r), "PROXY_QUEUE_FULL") {
		t.Fatal("queue did not reject overload")
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer pingCancel()
	if err := cs.Ping(pingCtx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if s.Status().Queued != 0 {
		t.Fatal("canceled request retained queue slot")
	}
	r := call(t, s)
	if !r.IsError || !strings.Contains(textOf(r), "PROXY_WAIT_TIMEOUT") {
		t.Fatalf("missing queue deadline: %+v", r)
	}
	close(release)
}

func TestCrashLoadingFailureAndSchemaDrift(t *testing.T) {
	root, tr, cfg := fixture(t)
	var mu sync.Mutex
	var latest *fakeSession
	var schema atomic.Bool
	s := startProxy(t, cfg, quietTracker{tr}, func(context.Context, []string) (backend.Session, error) {
		f := newFake("ok")
		if schema.Load() {
			f.tools[0].Description = "different"
		}
		mu.Lock()
		latest = f
		mu.Unlock()
		return f, nil
	})
	mu.Lock()
	latest.Close()
	mu.Unlock()
	waitFor(t, func() bool { return s.Status().Generation == 2 && s.Status().Ready })
	put(t, filepath.Join(root, "go.mod"), "require (invalid\n")
	s.Mark("broken_dependency")
	waitFor(t, func() bool { return s.Status().Error != "" })
	if r := call(t, s); !r.IsError || !strings.Contains(textOf(r), "go.mod") {
		t.Fatalf("missing real load error: %s", textOf(r))
	}
	put(t, filepath.Join(root, "go.mod"), "module example.com/repaired\n")
	s.Mark("repaired")
	waitFor(t, func() bool { return s.Status().Ready && s.Status().Generation == 3 })
	schema.Store(true)
	put(t, filepath.Join(root, "new.go"), "package first\n")
	s.Mark("schema_change")
	waitFor(t, func() bool { return strings.Contains(s.Status().Error, "tool definitions changed") })
	if r := call(t, s); !strings.Contains(textOf(r), "PROXY_SESSION_RESTART_REQUIRED") {
		t.Fatal(textOf(r))
	}
}

func TestWatcherAndPollingEachDetectChanges(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint(watch), func(t *testing.T) {
			root, tr, cfg := fixture(t)
			var tracker Tracker = tr
			if !watch {
				tracker = quietTracker{tr}
				cfg.PollInterval = 20 * time.Millisecond
			}
			s := startProxy(t, cfg, tracker, func(context.Context, []string) (backend.Session, error) { return newFake("ok"), nil })
			put(t, filepath.Join(root, "new.go"), "package first\n")
			waitFor(t, func() bool { return s.Status().Generation == 2 })
		})
	}
}

func TestExecutionTimeoutAndBoundedDrain(t *testing.T) {
	root, tr, cfg := fixture(t)
	cfg.RequestTimeout = 100 * time.Millisecond
	cfg.DrainTimeout = 30 * time.Millisecond
	entered := make(chan struct{}, 2)
	s := startProxy(t, cfg, quietTracker{tr}, func(context.Context, []string) (backend.Session, error) {
		f := newFake("ok")
		f.call = func(ctx context.Context, _ *mcp.CallToolParams, _ func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return f, nil
	})
	r := call(t, s)
	if !r.IsError || !strings.Contains(textOf(r), "PROXY_EXECUTION_TIMEOUT") {
		t.Fatal(textOf(r))
	}
	waitFor(t, func() bool { return s.Status().Generation == 2 && s.Status().Ready })
	<-entered
	done := make(chan *mcp.CallToolResult, 1)
	go func() { r, _ := s.Call(context.Background(), &mcp.CallToolParams{Name: "probe"}, nil); done <- r }()
	<-entered
	put(t, filepath.Join(root, "new.go"), "package first\n")
	s.Mark("during_long_request")
	if r := <-done; !r.IsError {
		t.Fatal("drained request succeeded")
	}
	waitFor(t, func() bool { return s.Status().Generation == 3 && s.Status().Ready })
}

func TestContinuousEventsRespectTotalWaitBudget(t *testing.T) {
	_, tr, cfg := fixture(t)
	cfg.RequestTimeout = 100 * time.Millisecond
	s := startProxy(t, cfg, quietTracker{tr}, func(context.Context, []string) (backend.Session, error) { return newFake("ok"), nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	s.Mark("continuous_change")
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Mark("continuous_change")
			}
		}
	}()
	r := call(t, s)
	cancel()
	<-stopped
	if !r.IsError || !strings.Contains(textOf(r), "PROXY_WAIT_TIMEOUT") {
		t.Fatalf("unexpected result: %s", textOf(r))
	}
	if s.Status().Queued != 0 {
		t.Fatal("expired request retained queue capacity")
	}
}

func TestStartFailuresBackOffAndRecover(t *testing.T) {
	root, tr, cfg := fixture(t)
	cfg.RetryMin = 30 * time.Millisecond
	cfg.RetryMax = 60 * time.Millisecond
	var fail atomic.Bool
	var mu sync.Mutex
	var failures []time.Time
	s := startProxy(t, cfg, quietTracker{tr}, func(context.Context, []string) (backend.Session, error) {
		if fail.Load() {
			mu.Lock()
			failures = append(failures, time.Now())
			mu.Unlock()
			return nil, errors.New("deliberate startup failure")
		}
		return newFake("ok"), nil
	})
	fail.Store(true)
	put(t, filepath.Join(root, "new.go"), "package first\n")
	s.Mark("change")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(failures) >= 4 })
	mu.Lock()
	recorded := append([]time.Time(nil), failures...)
	mu.Unlock()
	for i := 1; i < 4; i++ {
		minimum := cfg.RetryMin
		if i > 1 {
			minimum = cfg.RetryMax
		}
		if recorded[i].Sub(recorded[i-1]) < minimum {
			t.Fatal("restart loop did not honor exponential backoff")
		}
	}
	if r := call(t, s); !r.IsError || !strings.Contains(textOf(r), "deliberate startup failure") {
		t.Fatal(textOf(r))
	}
	fail.Store(false)
	waitFor(t, func() bool { return s.Status().Ready && s.Status().Generation == 2 })
}
