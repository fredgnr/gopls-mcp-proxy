// Package proxy maintains one upstream MCP session across backend generations.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/backend"
	"github.com/fredgnr/gopls-mcp-proxy/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Tracker interface {
	Scan(context.Context) (workspace.Snapshot, error)
	Watch(context.Context, func(string))
}

type Config struct {
	Debounce, PollInterval, DrainTimeout, StartupTimeout, RequestTimeout time.Duration
	RetryMin, RetryMax                                                   time.Duration
	QueueLimit, MaxConcurrent                                            int
	Logger                                                               *slog.Logger
}

func DefaultConfig() Config {
	return Config{Debounce: 500 * time.Millisecond, PollInterval: 2 * time.Second, DrainTimeout: 5 * time.Second,
		StartupTimeout: 45 * time.Second, RequestTimeout: 110 * time.Second, RetryMin: time.Second, RetryMax: 30 * time.Second,
		QueueLimit: 64, MaxConcurrent: 8, Logger: slog.Default()}
}

func (c Config) Validate() error {
	if c.Debounce < 0 || c.PollInterval <= 0 || c.DrainTimeout < 0 || c.StartupTimeout <= 0 || c.RequestTimeout <= 0 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin || c.QueueLimit < 1 || c.MaxConcurrent < 1 {
		return fmt.Errorf("invalid limits: durations must be positive (debounce/drain may be zero), retry-max >= retry-min, queue/concurrency >= 1")
	}
	return nil
}

type state struct {
	generation  uint64
	fingerprint workspace.Fingerprint
	session     backend.Session
	invalid     bool
	fault       string
	active      map[uint64]context.CancelFunc
	started     time.Time
}

type Supervisor struct {
	cfg                             Config
	tracker                         Tracker
	factory                         backend.Factory
	ctx                             context.Context
	cancel                          context.CancelFunc
	done, watchDone                 chan struct{}
	wake                            chan struct{}
	mu                              sync.Mutex
	changed                         chan struct{}
	current                         *state
	generation, requestID, eventSeq uint64
	ready, pending, closed          bool
	lastEvent, retryAt              time.Time
	reason                          string
	lastErr, fatal                  error
	failures                        int
	queued                          int
	tools                           []*mcp.Tool
	instructions                    string
	toolDigest                      [32]byte
}

func New(ctx context.Context, cfg Config, tracker Tracker, factory backend.Factory) (*Supervisor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Supervisor{cfg: cfg, tracker: tracker, factory: factory, ctx: ctx, cancel: cancel, done: make(chan struct{}), watchDone: make(chan struct{}), wake: make(chan struct{}, 1), changed: make(chan struct{}), pending: true, reason: "initial_start"}
	go func() { defer close(s.watchDone); tracker.Watch(ctx, s.Mark) }()
	go s.run()
	return s, nil
}

func (s *Supervisor) Close() { s.cancel(); <-s.done; <-s.watchDone }

// Mark only pauses dispatch. A single supervisor decides whether to replace a backend.
func (s *Supervisor) Mark(reason string) {
	s.mu.Lock()
	s.pending = true
	s.lastEvent = time.Now()
	s.reason = reason
	s.eventSeq++
	s.notifyLocked()
	s.mu.Unlock()
	s.signal()
}

func (s *Supervisor) observe(reason string) {
	s.mu.Lock()
	if !s.pending {
		s.pending = true
		s.lastEvent = time.Now()
		s.reason = reason
		s.eventSeq++
		s.notifyLocked()
	}
	s.mu.Unlock()
	s.signal()
}

func (s *Supervisor) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Supervisor) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }

func (s *Supervisor) run() {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		s.closed = true
		s.ready = false
		b := s.current
		if b != nil {
			b.invalid = true
			for _, cancel := range b.active {
				cancel()
			}
		}
		s.notifyLocked()
		s.mu.Unlock()
		if b != nil {
			b.session.Close()
		}
	}()
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for s.ctx.Err() == nil {
		s.mu.Lock()
		due := s.lastEvent.Add(s.cfg.Debounce)
		if s.retryAt.After(due) {
			due = s.retryAt
		}
		pending := s.pending && s.fatal == nil
		var dead <-chan struct{}
		b := s.current
		if b != nil && !b.invalid {
			dead = b.session.Done()
		}
		s.mu.Unlock()
		if pending && !time.Now().Before(due) {
			s.reconcile()
			continue
		}
		var timer *time.Timer
		var timerC <-chan time.Time
		if pending {
			timer = time.NewTimer(time.Until(due))
			timerC = timer.C
		}
		select {
		case <-s.ctx.Done():
		case <-s.wake:
		case <-timerC:
		case <-dead:
			s.retire(b)
			s.failed("backend_exit", errors.New("gopls MCP connection exited unexpectedly"))
		case <-ticker.C:
			snap, err := s.tracker.Scan(s.ctx)
			s.mu.Lock()
			b := s.current
			changed := err != nil || b == nil || snap.Fingerprint != b.fingerprint
			s.mu.Unlock()
			if changed {
				s.observe("poll_fingerprint")
			}
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Supervisor) reconcile() {
	started := time.Now()
	s.mu.Lock()
	seq, reason, b := s.eventSeq, s.reason, s.current
	s.mu.Unlock()
	snap, err := s.tracker.Scan(s.ctx)
	if err != nil {
		s.retire(b)
		s.failed("fingerprint", err)
		return
	}
	s.mu.Lock()
	alive := b != nil && !b.invalid && b.fault == ""
	if alive {
		select {
		case <-b.session.Done():
			alive = false
		default:
		}
	}
	if alive && b.fingerprint == snap.Fingerprint {
		if seq == s.eventSeq {
			s.pending = false
			s.ready = true
			s.lastErr = nil
			s.retryAt = time.Time{}
			s.notifyLocked()
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.retire(b)
	if s.ctx.Err() != nil {
		return
	}
	// Drain time is outside the start timeout; each queued tool still has one total deadline.
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.StartupTimeout)
	candidate, err := s.factory(ctx, snap.Roots)
	cancel()
	if err != nil {
		s.failed("initialize", err)
		return
	}
	after, err := s.tracker.Scan(s.ctx)
	if err != nil {
		candidate.Close()
		s.failed("post_start_fingerprint", err)
		return
	}
	select {
	case <-candidate.Done():
		candidate.Close()
		s.failed("initialize", errors.New("backend exited before publication"))
		return
	default:
	}
	s.mu.Lock()
	changed := seq != s.eventSeq || after.Fingerprint != snap.Fingerprint
	s.mu.Unlock()
	if changed {
		candidate.Close()
		s.Mark("changed_during_start")
		s.cfg.Logger.Info("discarded unstable backend", "reason", "changed_during_start", "elapsed", time.Since(started))
		return
	}
	digest, err := backend.ToolDigest(candidate.Tools())
	if err != nil {
		candidate.Close()
		s.failed("tool_definitions", err)
		return
	}
	s.mu.Lock()
	if len(s.tools) > 0 && digest != s.toolDigest {
		s.fatal = errors.New("gopls tool definitions changed; restart the proxy MCP session")
		s.ready = false
		s.pending = false
		s.notifyLocked()
		s.mu.Unlock()
		candidate.Close()
		s.cfg.Logger.Error("backend rejected", "stage", "tool_schema", "error", s.fatal)
		return
	}
	// Recheck the event sequence under the publication lock.
	if seq != s.eventSeq || s.ctx.Err() != nil {
		s.mu.Unlock()
		candidate.Close()
		return
	}
	if len(s.tools) == 0 {
		s.tools = candidate.Tools()
		s.instructions = candidate.Instructions()
		s.toolDigest = digest
	}
	s.generation++
	s.current = &state{generation: s.generation, fingerprint: after.Fingerprint, session: candidate, active: map[uint64]context.CancelFunc{}, started: time.Now()}
	s.ready = true
	s.pending = false
	s.lastErr = nil
	s.retryAt = time.Time{}
	generation, queued := s.generation, s.queued
	s.notifyLocked()
	s.mu.Unlock()
	s.cfg.Logger.Info("backend ready", "generation", generation, "reason", reason, "fingerprint", after.Fingerprint.String(), "elapsed", time.Since(started), "queued", queued)
}

// retire is called only by run/reconcile. Requests can finish while draining,
// but invalid is set first so their results cannot escape as current analysis.
func (s *Supervisor) retire(b *state) {
	if b == nil {
		return
	}
	s.mu.Lock()
	if b.invalid {
		s.mu.Unlock()
		return
	}
	s.ready = false
	b.invalid = true
	s.notifyLocked()
	s.mu.Unlock()
	timer := time.NewTimer(s.cfg.DrainTimeout)
	defer timer.Stop()
drain:
	for {
		s.mu.Lock()
		count, changed := len(b.active), s.changed
		s.mu.Unlock()
		if count == 0 {
			break
		}
		select {
		case <-changed:
		case <-timer.C:
			break drain
		case <-s.ctx.Done():
			break drain
		}
	}
	s.mu.Lock()
	for _, cancel := range b.active {
		cancel()
	}
	s.mu.Unlock()
	b.session.Close()
}

func (s *Supervisor) failed(stage string, err error) {
	if s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.failures++
	delay := s.cfg.RetryMin
	for i := 1; i < s.failures && delay < s.cfg.RetryMax; i++ {
		if delay > s.cfg.RetryMax/2 {
			delay = s.cfg.RetryMax
		} else {
			delay *= 2
		}
	}
	if delay > s.cfg.RetryMax {
		delay = s.cfg.RetryMax
	}
	s.lastErr = fmt.Errorf("%s: %w", stage, err)
	s.ready = false
	s.pending = true
	s.reason = "retry_after_" + stage
	s.retryAt = time.Now().Add(delay)
	generation, queued := s.generation, s.queued
	s.notifyLocked()
	s.mu.Unlock()
	s.cfg.Logger.Error("backend unavailable", "generation", generation, "stage", stage, "error", err, "retry_in", delay, "queued", queued)
}

func (s *Supervisor) WaitReady(ctx context.Context) ([]*mcp.Tool, string, error) {
	for {
		s.mu.Lock()
		ready, tools, instructions, ch, fatal, closed := s.ready && !s.pending, s.tools, s.instructions, s.changed, s.fatal, s.closed
		lastErr := s.lastErr
		s.mu.Unlock()
		if fatal != nil {
			return nil, "", fatal
		}
		if closed {
			return nil, "", errors.New("proxy closed")
		}
		if ready {
			return tools, instructions, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, "", fmt.Errorf("waiting for backend: %w (last failure: %v)", ctx.Err(), lastErr)
		}
	}
}

type Status struct {
	Generation     uint64
	Ready, Pending bool
	Queued, Active int
	Error          string
}

func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Generation: s.generation, Ready: s.ready && !s.pending, Pending: s.pending, Queued: s.queued}
	if s.current != nil {
		st.Active = len(s.current.active)
	}
	if s.lastErr != nil {
		st.Error = s.lastErr.Error()
	}
	if s.fatal != nil {
		st.Error = s.fatal.Error()
	}
	return st
}
