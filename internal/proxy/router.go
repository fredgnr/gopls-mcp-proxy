package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/buildinfo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func toolError(code string, err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: code + ": " + err.Error()}}}
}

// Call shares one deadline across admission, fingerprint checks, queueing and execution.
// Once dispatched, a request is never replayed, including after a backend crash.
func (s *Supervisor) Call(parent context.Context, params *mcp.CallToolParams, progress func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.RequestTimeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	s.mu.Lock()
	if s.queued >= s.cfg.QueueLimit {
		s.mu.Unlock()
		return toolError("PROXY_QUEUE_FULL", errors.New("request queue is full; retry later")), nil
	}
	s.queued++
	s.mu.Unlock()
	queued := true
	defer func() {
		if queued {
			s.mu.Lock()
			s.queued--
			s.notifyLocked()
			s.mu.Unlock()
		}
	}()
	var b *state
	var id uint64
	for {
		if err := ctx.Err(); err != nil {
			if parent.Err() != nil {
				return nil, parent.Err()
			}
			return toolError("PROXY_WAIT_TIMEOUT", fmt.Errorf("workspace is still changing or backend is busy; retry: %w", err)), nil
		}
		s.mu.Lock()
		ch, fatal, lastErr, closed := s.changed, s.fatal, s.lastErr, s.closed
		b = s.current
		canDispatch := s.ready && !s.pending && b != nil && !b.invalid && len(b.active) < s.cfg.MaxConcurrent
		seq := s.eventSeq
		s.mu.Unlock()
		if closed {
			return toolError("PROXY_CLOSED", errors.New("proxy is shutting down")), nil
		}
		if fatal != nil {
			return toolError("PROXY_SESSION_RESTART_REQUIRED", fatal), nil
		}
		if lastErr != nil {
			return toolError("PROXY_BACKEND_UNAVAILABLE", lastErr), nil
		}
		if !canDispatch {
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				continue
			}
		}
		snap, err := s.tracker.Scan(ctx)
		if err != nil {
			s.observe("request_scan_failed")
			return toolError("PROXY_WORKSPACE_ERROR", err), nil
		}
		if snap.Fingerprint != b.fingerprint {
			s.observe("request_fingerprint")
			continue
		}
		s.mu.Lock()
		if s.current != b || !s.ready || s.pending || b.invalid || seq != s.eventSeq || len(b.active) >= s.cfg.MaxConcurrent {
			s.mu.Unlock()
			continue
		}
		select {
		case <-b.session.Done():
			s.mu.Unlock()
			s.observe("request_backend_exit")
			continue
		default:
		}
		s.requestID++
		id = s.requestID
		b.active[id] = cancel
		s.queued--
		queued = false
		s.mu.Unlock()
		break
	}
	defer func() { s.mu.Lock(); delete(b.active, id); s.notifyLocked(); s.mu.Unlock(); s.signal() }()
	result, err := b.session.Call(ctx, params, func(p *mcp.ProgressNotificationParams) {
		s.mu.Lock()
		valid := s.current == b && !b.invalid
		s.mu.Unlock()
		if valid && ctx.Err() == nil && progress != nil {
			progress(p)
		}
	})
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if ctx.Err() != nil {
		s.fault(b, "request_timeout")
		return toolError("PROXY_EXECUTION_TIMEOUT", errors.New("backend request deadline or drain limit exceeded; the request was not replayed")), nil
	}
	if err != nil {
		// A protocol-level tool error need not mean the backend died. Connection
		// failures are observed by the supervisor via Done.
		return nil, err
	}
	// Wait for the event quiet window, independently of the drain wait. Waiting
	// for backend readiness here would deadlock with the supervisor draining us.
	for {
		s.mu.Lock()
		lastEvent := s.lastEvent
		invalid := b.invalid || s.current != b
		s.mu.Unlock()
		if invalid {
			return toolError("PROXY_STALE_RESULT", fmt.Errorf("generation %d was invalidated; retry against the rebuilt backend", b.generation)), nil
		}
		remaining := time.Until(lastEvent.Add(s.cfg.Debounce))
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return toolError("PROXY_WAIT_TIMEOUT", errors.New("workspace is still changing; retry")), nil
		}
	}
	snap, scanErr := s.tracker.Scan(ctx)
	if scanErr != nil {
		s.observe("result_scan_failed")
		return toolError("PROXY_WORKSPACE_ERROR", scanErr), nil
	}
	if snap.Fingerprint != b.fingerprint {
		s.observe("result_fingerprint")
		return toolError("PROXY_STALE_RESULT", fmt.Errorf("workspace changed during generation %d; result discarded, request not replayed", b.generation)), nil
	}
	s.mu.Lock()
	invalid := b.invalid || s.current != b
	if !invalid {
		s.failures = 0
	}
	s.mu.Unlock()
	if invalid {
		return toolError("PROXY_STALE_RESULT", fmt.Errorf("generation %d is no longer current", b.generation)), nil
	}
	return result, nil
}

func (s *Supervisor) fault(b *state, reason string) {
	s.mu.Lock()
	if s.current == b {
		b.fault = reason
	}
	s.mu.Unlock()
	s.observe(reason)
}

func (s *Supervisor) Server(tools []*mcp.Tool, instructions string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "gopls-mcp-proxy", Version: buildinfo.Version}, &mcp.ServerOptions{
		Instructions:              instructions,
		Logger:                    s.cfg.Logger,
		Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		SupportedProtocolVersions: []string{backendProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05"},
	})
	for _, tool := range tools {
		// The non-generic AddTool path preserves the backend schema and result.
		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.Call(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments, Meta: req.Params.Meta}, func(p *mcp.ProgressNotificationParams) {
				_ = req.Session.NotifyProgress(ctx, p)
			})
		})
	}
	return server
}

const backendProtocolVersion = "2025-11-25"
