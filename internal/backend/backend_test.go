package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPHelperProcess(t *testing.T) {
	if !slices.Contains(os.Args, "proxy-backend-helper") {
		return
	}
	if slices.Contains(os.Args, "hang-init") {
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "helper", Version: "1"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}, SupportedProtocolVersions: []string{ProtocolVersion}})
	server.AddTool(&mcp.Tool{Name: "probe", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var params struct {
			ID    string `json:"id"`
			Wait  bool   `json:"wait"`
			Crash bool   `json:"crash"`
			Error bool   `json:"error"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &params)
		if params.Crash {
			os.Exit(7)
		}
		if token := req.Params.GetProgressToken(); token != nil {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 1, Total: 2, Message: params.ID})
		}
		if params.Wait {
			<-ctx.Done()
			_ = os.WriteFile("canceled", []byte(params.ID), 0644)
			return nil, ctx.Err()
		}
		roots, err := req.Session.ListRoots(ctx, nil)
		if err != nil {
			return nil, err
		}
		// Allow the independently forwarded progress notification to arrive first.
		time.Sleep(20 * time.Millisecond)
		return &mcp.CallToolResult{Meta: mcp.Meta{"test": "preserved"}, Content: []mcp.Content{&mcp.TextContent{Text: params.ID}, &mcp.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"}}, StructuredContent: map[string]any{"roots": roots.Roots, "id": params.ID}, IsError: params.Error}, nil
	})
	err := server.Run(context.Background(), &mcp.StdioTransport{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func helperFactory(t *testing.T, dir string, extra ...string) Factory {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-test.run=^TestMCPHelperProcess$", "--", "proxy-backend-helper"}, extra...)
	factory, err := NewFactory(Config{Executable: exe, Args: args, Workspace: dir, ShutdownTimeout: 50 * time.Millisecond, Stderr: io.Discard, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestDualSessionPreservesResultsRootsAndProgress(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := helperFactory(t, dir)(ctx, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			updates := make(chan *mcp.ProgressNotificationParams, 4)
			p := &mcp.CallToolParams{Name: "probe", Arguments: map[string]any{"id": id, "error": true}}
			p.SetProgressToken("same-upstream-token")
			r, err := s.Call(ctx, p, func(p *mcp.ProgressNotificationParams) { updates <- p })
			if err != nil {
				t.Error(err)
				return
			}
			if !r.IsError || r.Meta["test"] != "preserved" || len(r.Content) != 2 || r.Content[1].(*mcp.ImageContent).MIMEType != "image/png" {
				t.Errorf("result lost fields: %+v", r)
			}
			encoded, _ := json.Marshal(r.StructuredContent)
			if !strings.Contains(string(encoded), FileURI(dir)) {
				t.Errorf("roots missing: %s", encoded)
			}
			select {
			case p := <-updates:
				if p.ProgressToken != "same-upstream-token" || p.Message != id {
					t.Errorf("misrouted progress: %+v", p)
				}
			case <-ctx.Done():
				t.Error("progress missing")
			}
		}(id)
	}
	wg.Wait()
}

func TestCancellationAndCrash(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := helperFactory(t, dir)(ctx, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	requestCtx, stop := context.WithCancel(ctx)
	progress := make(chan struct{}, 1)
	done := make(chan error, 1)
	p := &mcp.CallToolParams{Name: "probe", Arguments: map[string]any{"wait": true, "id": "cancel-me"}}
	p.SetProgressToken(7)
	go func() {
		_, err := s.Call(requestCtx, p, func(*mcp.ProgressNotificationParams) { progress <- struct{}{} })
		done <- err
	}()
	select {
	case <-progress:
	case <-ctx.Done():
		t.Fatal("request not started")
	}
	stop()
	if err := <-done; err == nil {
		t.Fatal("cancellation succeeded")
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "canceled")); err == nil && string(b) == "cancel-me" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("downstream cancellation not received")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := s.Call(ctx, &mcp.CallToolParams{Name: "probe", Arguments: map[string]any{"crash": true}}, nil); err == nil {
		t.Fatal("crashed backend returned success")
	}
	select {
	case <-s.Done():
	case <-ctx.Done():
		t.Fatal("crash not observed")
	}
}

func TestUnresponsiveInitializationIsKilledAndReaped(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	if s, err := helperFactory(t, dir, "hang-init")(ctx, []string{dir}); err == nil {
		s.Close()
		t.Fatal("unresponsive server initialized")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("initialization cleanup was unbounded")
	}
}

func TestShutdownWaitsForProcess(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := helperFactory(t, dir)(ctx, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	proc := s.(*session).proc
	select {
	case <-proc.done:
	default:
		t.Fatal("child was not reaped")
	}
	if proc.cmd.ProcessState == nil {
		t.Fatal("missing Wait result")
	}
}
