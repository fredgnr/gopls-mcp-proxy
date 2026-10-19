// Package proxy relays opaque MCP messages across independently refreshed processes.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/backend"
	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
	"github.com/fredgnr/gopls-mcp-proxy/internal/workspace"
)

type Tracker interface {
	Scan(context.Context) (workspace.Snapshot, error)
	Watch(context.Context, func(string))
	Epoch() uint64
}
type Config struct {
	Debounce, MaxDebounce, PollInterval, DrainTimeout, StartupTimeout, RequestTimeout time.Duration
	RefreshTimeout, CleanupTimeout, RetryMin, RetryMax                                time.Duration
	QueueLimit, QueueBytes, MaxFrameBytes, MaxConcurrent                              int
	DisableWatcher                                                                    bool
	Logger                                                                            *slog.Logger
}

func DefaultConfig() Config {
	return Config{Debounce: 500 * time.Millisecond, MaxDebounce: 2 * time.Second, PollInterval: 2 * time.Second, DrainTimeout: 5 * time.Second, StartupTimeout: 45 * time.Second, RequestTimeout: 110 * time.Second, RefreshTimeout: 60 * time.Second, CleanupTimeout: 6 * time.Second, RetryMin: time.Second, RetryMax: 30 * time.Second, QueueLimit: 64, QueueBytes: 64 << 20, MaxFrameBytes: 16 << 20, MaxConcurrent: 8, Logger: slog.Default()}
}
func (c Config) Validate() error {
	if c.Debounce < 0 || c.DrainTimeout < 0 || c.MaxDebounce <= 0 || c.PollInterval <= 0 || c.StartupTimeout <= 0 || c.RequestTimeout <= 0 || c.RefreshTimeout <= 0 || c.CleanupTimeout <= 0 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin || c.QueueLimit < 1 || c.MaxConcurrent < 1 || c.QueueBytes < 1 || c.MaxFrameBytes < 1 {
		return errors.New("invalid proxy budgets")
	}
	return nil
}

type incoming struct {
	raw  []byte
	boot uint64
	err  error
}
type writeItem struct {
	raw []byte
	at  time.Time
}
type writeDone struct {
	boot uint64
	size int
	err  error
}
type scanDone struct {
	batches  []*batchGroup
	results  []resultFrame
	dispatch *queued
	purpose  string
	snap     workspace.Snapshot
	err      error
}
type startDone struct {
	proc *backend.Process
	snap workspace.Snapshot
	err  error
}
type stopDone struct {
	boot uint64
	err  error
}
type recoveryDone struct {
	boot uint64
	err  error
}
type internalCall struct {
	ctx    context.Context
	raw    []byte
	reply  chan internalReply
	notify bool
}
type internalReply struct {
	raw []byte
	err error
}
type controlRequest struct {
	reply chan internalReply
	id    wire.ID
}
type batchGroup struct {
	records      []*record
	upstream     []bool
	queued, done bool
	remaining    int
	responses    [][]byte
	batch        bool
}
type record struct {
	buffered                              bool
	msg                                   wire.Message
	group                                 *batchGroup
	deadline                              time.Time
	boot, generation                      uint64
	dispatched, cancelled, received, long bool
	progress                              wire.ID
	mappedProgress                        wire.ID
}
type queued struct {
	msgs     []wire.Message
	records  []*record
	batch    bool
	bytes    int
	deadline time.Time
}
type resultFrame struct {
	msgs  []wire.Message
	boot  uint64
	bytes int
}
type reverseGroup struct {
	remaining int
	responses [][]byte
	batch     bool
}
type reverseRecord struct {
	group                    *reverseGroup
	progress, mappedProgress wire.ID
	original                 wire.ID
	boot                     uint64
	deadline                 time.Time
}

type relay struct {
	reverseBytes                                  int
	readyBatches                                  []*batchGroup
	startResult                                   chan startDone
	startFinished                                 chan struct{}
	ctx                                           context.Context
	cancel                                        context.CancelFunc
	cfg                                           Config
	tracker                                       Tracker
	factory                                       *backend.Factory
	input                                         io.ReadCloser
	output                                        io.WriteCloser
	events                                        chan any
	front                                         chan writeItem
	back                                          chan writeItem
	proc                                          *backend.Process
	phase                                         string
	generation, sequence                          uint64
	snapshot                                      workspace.Snapshot
	candidate                                     workspace.Snapshot
	queue                                         []*queued
	queueCount, queueBytes                        int
	pending                                       map[string]*record
	reverse                                       map[string]reverseRecord
	controls                                      map[string]controlRequest
	progress                                      map[string]*record
	results                                       []resultFrame
	resultBytes                                   int
	recovery                                      *Recovery
	scanning                                      bool
	poll                                          bool
	dirtyAt, lastEvent, drainAt, startAt, retryAt time.Time
	retryDelay                                    time.Duration
	frontPending, backPending                     []writeItem
	frontBytes, backBytes                         int
	fatal                                         error
}

// Run owns both streams. Closing them is essential to interrupt blocked stdio.
// Each connection has one reader and one ordered writer; only this event loop
// changes generations or dispatches a never-before-written business request.
func Run(parent context.Context, cfg Config, tracker Tracker, factory *backend.Factory, input io.ReadCloser, output io.WriteCloser) (runErr error) {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	r := &relay{ctx: ctx, cancel: cancel, cfg: cfg, tracker: tracker, factory: factory, input: input, output: output, events: make(chan any, 16), startResult: make(chan startDone), front: make(chan writeItem, 16), phase: "idle", pending: map[string]*record{}, reverse: map[string]reverseRecord{}, controls: map[string]controlRequest{}, progress: map[string]*record{}, recovery: &Recovery{}, retryDelay: cfg.RetryMin}
	defer func() {
		cancel()
		if r.startFinished != nil {
			select {
			case <-r.startFinished:
			case <-time.After(cfg.CleanupTimeout):
				runErr = errors.Join(runErr, errors.New("startup cleanup timeout"))
			}
		}
		_ = input.Close()
		_ = output.Close()
		if r.proc != nil {
			c, cc := context.WithTimeout(context.Background(), cfg.CleanupTimeout)
			defer cc()
			if err := r.proc.CloseContext(c); err != nil {
				cfg.Logger.Error("backend cleanup failed", "error", err)
				runErr = errors.Join(runErr, err)
			}
		}
	}()
	go r.read(input, 0)
	go r.write(output, r.front, 0)
	if !cfg.DisableWatcher {
		go tracker.Watch(ctx, func(reason string) { r.emit(reason) })
	}
	poll := time.NewTicker(cfg.PollInterval)
	defer poll.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		r.advance()
		if r.fatal != nil {
			r.cfg.Logger.Error("proxy relay terminated", "error", r.fatal)
			return r.fatal
		}
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			r.poll = true
		case <-tick.C:
			r.expire()
		case e := <-r.startResult:
			r.started(e)
		case ev := <-r.events:
			switch e := ev.(type) {
			case incoming:
				if e.boot == 0 {
					if e.err != nil {
						if errors.Is(e.err, io.EOF) {
							return nil
						}
						return e.err
					}
					r.fromFront(e.raw)
				} else if r.proc != nil && e.boot == r.proc.ID {
					if e.err != nil {
						r.invalidate("backend_unavailable")
						if r.phase == "recovering" {
							r.failControls(e.err)
						}
						if r.phase == "ready" {
							r.phase = "draining"
							r.drainAt = time.Now()
						}
					} else {
						r.fromBack(e.raw)
					}
				}
			case writeDone:
				r.written(e)
			case string:
				r.lastEvent = time.Now()
				r.poll = true
				if r.proc != nil && tracker.Epoch() != r.snapshot.Epoch {
					r.invalidate(e)
				}
			case scanDone:
				r.scanned(e)
			case startDone:
				r.started(e)
			case stopDone:
				r.stopped(e)
			case recoveryDone:
				if r.proc != nil && r.proc.ID == e.boot {
					if e.err != nil {
						r.fatal = fmt.Errorf("protocol_restart_required: %w", e.err)
					} else {
						r.phase = "publishing"
					}
				}
			case internalCall:
				r.internal(e)
			}
		}
	}
}
func (r *relay) emit(e any) {
	select {
	case r.events <- e:
	case <-r.ctx.Done():
	}
}
func (r *relay) read(in io.Reader, boot uint64) {
	rd := wire.NewReader(in, r.cfg.MaxFrameBytes)
	for {
		b, err := rd.ReadFrame()
		r.emit(incoming{b, boot, err})
		if err != nil {
			return
		}
	}
}
func (r *relay) write(out io.Writer, ch <-chan writeItem, boot uint64) {
	r.writeBackend(out, ch, boot, nil)
}
func (r *relay) writeBackend(out io.Writer, ch <-chan writeItem, boot uint64, dead <-chan struct{}) {
	wr := wire.NewWriter(out, r.cfg.MaxFrameBytes)
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-dead:
			return
		case item := <-ch:
			err := wr.WriteFrame(item.raw)
			r.emit(writeDone{boot, len(item.raw), err})
			if err != nil {
				return
			}
		}
	}
}
func (r *relay) sendFront(raw []byte) {
	if r.fatal != nil {
		return
	}
	if r.frontBytes+len(raw) > r.cfg.QueueBytes {
		r.fatal = errors.New("client output byte budget exceeded")
		return
	}
	item := writeItem{raw, time.Now()}
	select {
	case r.front <- item:
		r.frontPending = append(r.frontPending, item)
		r.frontBytes += len(raw)
	default:
		r.fatal = errors.New("client output backpressure limit exceeded")
	}
}
func (r *relay) sendBack(raw []byte) {
	if r.proc == nil {
		r.fatal = errors.New("backend unavailable for control message")
		return
	}
	if r.backBytes+len(raw) > r.cfg.QueueBytes {
		r.fatal = errors.New("backend output byte budget exceeded")
		return
	}
	item := writeItem{raw, time.Now()}
	select {
	case r.back <- item:
		r.backPending = append(r.backPending, item)
		r.backBytes += len(raw)
	default:
		r.fatal = errors.New("backend control/output backpressure limit exceeded")
	}
}
func (r *relay) written(e writeDone) {
	if e.boot == 0 {
		if len(r.frontPending) > 0 {
			r.frontPending = r.frontPending[1:]
			r.frontBytes -= e.size
		}
		if e.err != nil {
			r.fatal = e.err
		}
		return
	}
	if r.proc == nil || r.proc.ID != e.boot {
		return
	}
	if len(r.backPending) > 0 {
		r.backPending = r.backPending[1:]
		r.backBytes -= e.size
	}
	if e.err != nil {
		r.invalidate("backend_write_failed")
	}
}
func (r *relay) invalidate(reason string) {
	if r.proc == nil {
		return
	}
	if r.dirtyAt.IsZero() {
		r.dirtyAt = time.Now()
		r.cfg.Logger.Info("backend invalidated", "reason", reason, "generation", r.generation, "boot_id", r.proc.ID)
	}
	if r.phase == "ready" {
		r.phase = "draining"
		r.drainAt = time.Now()
		r.endSubscriptions()
	}
}
func (r *relay) scan(purpose string) {
	if r.scanning {
		return
	}
	r.scanning = true
	var groups []*batchGroup
	var frames []resultFrame
	var head *queued
	if purpose == "results" {
		frames = r.results
		r.results = nil
		groups = r.readyBatches
		r.readyBatches = nil
	}
	if purpose == "dispatch" && len(r.queue) > 0 {
		head = r.queue[0]
	}
	deadline := time.Now().Add(r.cfg.RequestTimeout)
	if !r.dirtyAt.IsZero() {
		deadline = r.dirtyAt.Add(r.cfg.RefreshTimeout)
	}
	if (purpose == "initial" || purpose == "publish") && !r.startAt.IsZero() && r.startAt.Add(r.cfg.StartupTimeout).Before(deadline) {
		deadline = r.startAt.Add(r.cfg.StartupTimeout)
	}
	go func() {
		ctx, c := context.WithDeadline(r.ctx, deadline)
		defer c()
		snap, err := r.tracker.Scan(ctx)
		r.emit(scanDone{purpose: purpose, snap: snap, err: err, results: frames, dispatch: head, batches: groups})
	}()
}
func (r *relay) advance() {
	if r.fatal != nil {
		return
	}
	if r.proc != nil && r.phase == "ready" && r.tracker.Epoch() != r.snapshot.Epoch {
		r.invalidate("dirty_epoch")
	}
	if r.phase == "draining" {
		if r.active() == 0 || time.Since(r.drainAt) >= r.cfg.DrainTimeout {
			r.stopCurrent("workspace_changed")
		}
		return
	}
	if r.phase == "recovering" || r.phase == "starting" || r.phase == "stopping" {
		return
	}
	if r.phase == "idle" && len(r.queue) > 0 {
		if r.startAt.IsZero() {
			r.startAt = time.Now()
		}
		r.scan("initial")
		return
	}
	if r.phase == "restarting" {
		due := r.lastEvent.Add(r.cfg.Debounce)
		max := r.dirtyAt.Add(r.cfg.MaxDebounce)
		if due.After(max) {
			due = max
		}
		if r.retryAt.After(due) {
			due = r.retryAt
		}
		if time.Now().Before(due) {
			return
		}
		r.scan("initial")
		return
	}
	if r.phase == "publishing" {
		r.scan("publish")
		return
	}
	if r.phase != "ready" {
		return
	}
	if len(r.results) > 0 || len(r.readyBatches) > 0 {
		r.scan("results")
		return
	}
	if len(r.queue) > 0 && r.canDispatch(r.queue[0]) {
		r.scan("dispatch")
		return
	}
	if r.poll {
		r.poll = false
		r.scan("poll")
	}
}
func (r *relay) scanned(e scanDone) {
	r.scanning = false
	if e.err != nil {
		if r.proc != nil {
			r.invalidate("input_scan_failed")
		}
		if r.phase == "idle" || r.phase == "restarting" {
			r.retryAt = time.Now().Add(r.retryDelay)
			r.phase = "restarting"
			if r.dirtyAt.IsZero() {
				r.dirtyAt = time.Now()
			}
		}
		if e.purpose == "results" {
			r.deliverResults(e.results, false, "input_scan_failed")
			for _, g := range e.batches {
				r.deliverBatch(g, false)
			}
		}
		return
	}
	if e.snap.Epoch != r.tracker.Epoch() {
		r.poll = true
		if e.purpose == "results" {
			r.results = append(e.results, r.results...)
			r.readyBatches = append(e.batches, r.readyBatches...)
		}
		return
	}
	switch e.purpose {
	case "initial":
		if r.phase != "idle" && r.phase != "restarting" {
			return
		}
		r.candidate = e.snap
		r.phase = "starting"
		r.startAt = time.Now()
		r.startFinished = make(chan struct{})
		finished := r.startFinished
		go func() {
			defer close(finished)
			ctx, c := context.WithTimeout(r.ctx, r.cfg.StartupTimeout)
			defer c()
			p, err := r.factory.Start(ctx)
			select {
			case r.startResult <- startDone{p, e.snap, err}:
			case <-r.ctx.Done():
				if p != nil {
					_ = p.Close()
				}
			}
		}()
	case "publish":
		if r.phase != "publishing" {
			return
		}
		if e.snap.Fingerprint != r.candidate.Fingerprint || e.snap.Epoch != r.candidate.Epoch {
			r.invalidate("changed_during_start")
			r.stopCurrent("workspace_changed")
			return
		}
		r.snapshot = e.snap
		r.generation++
		r.phase = "ready"
		r.dirtyAt = time.Time{}
		r.retryAt = time.Time{}
		r.retryDelay = r.cfg.RetryMin
		r.cfg.Logger.Info("backend ready", "generation", r.generation, "boot_id", r.proc.ID, "pid", r.proc.PID(), "fingerprint", e.snap.Fingerprint.String(), "epoch", e.snap.Epoch, "queued", r.queueCount, "queue_bytes", r.queueBytes)
	default:
		if r.proc == nil {
			return
		}
		fresh := r.phase == "ready" && r.dirtyAt.IsZero() && e.snap.Fingerprint == r.snapshot.Fingerprint && e.snap.Epoch == r.snapshot.Epoch && r.tracker.Epoch() == e.snap.Epoch
		if !fresh {
			r.invalidate("input_checkpoint")
		}
		if e.purpose == "results" {
			r.deliverResults(e.results, fresh, "workspace_changed")
			for _, g := range e.batches {
				r.deliverBatch(g, fresh)
			}
		}
		if e.purpose == "dispatch" && fresh && len(r.queue) > 0 && r.queue[0] == e.dispatch && r.canDispatch(r.queue[0]) {
			r.dispatch()
		}
	}
}
func (r *relay) started(e startDone) {
	if e.err != nil {
		r.cfg.Logger.Warn("backend start failed", "error", e.err)
		r.phase = "restarting"
		if r.dirtyAt.IsZero() {
			r.dirtyAt = time.Now()
		}
		r.retryAt = time.Now().Add(r.retryDelay)
		r.retryDelay = min(r.retryDelay*2, r.cfg.RetryMax)
		return
	}
	r.proc = e.proc
	r.back = make(chan writeItem, 16)
	r.backPending = nil
	r.backBytes = 0
	go r.read(e.proc.Stdout, e.proc.ID)
	go func(p *backend.Process) {
		select {
		case <-p.Done:
			r.emit(incoming{boot: p.ID, err: errors.New("backend process exited")})
		case <-r.ctx.Done():
		}
	}(e.proc)
	go r.writeBackend(e.proc.Stdin, r.back, e.proc.ID, e.proc.Done)
	if r.generation == 0 {
		r.phase = "publishing"
		return
	}
	r.phase = "recovering"
	saved := r.recovery.Clone()
	boot := e.proc.ID
	refreshDeadline := r.dirtyAt.Add(r.cfg.RefreshTimeout)
	go func() {
		deadline := refreshDeadline
		stage := time.Now().Add(r.cfg.StartupTimeout)
		if stage.Before(deadline) {
			deadline = stage
		}
		ctx, c := context.WithDeadline(r.ctx, deadline)
		defer c()
		exchange := func(ctx context.Context, b []byte) ([]byte, error) {
			reply := make(chan internalReply, 1)
			r.emit(internalCall{ctx, b, reply, false})
			select {
			case v := <-reply:
				return v.raw, v.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		send := func(ctx context.Context, b []byte) error {
			reply := make(chan internalReply, 1)
			r.emit(internalCall{ctx, b, reply, true})
			select {
			case v := <-reply:
				return v.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := saved.Recover(ctx, exchange, send, func(b []byte) error { return sendFrontInternal(r, ctx, b) })
		r.emit(recoveryDone{boot, err})
	}()
}

// Internal frontend notifications are serialized by the same event loop.
func sendFrontInternal(r *relay, ctx context.Context, b []byte) error {
	reply := make(chan internalReply, 1)
	r.emit(internalCall{ctx, b, reply, true})
	select {
	case v := <-reply:
		return v.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *relay) stopCurrent(reason string) {
	if r.proc == nil || r.phase == "stopping" {
		return
	}
	r.phase = "stopping"
	r.failControls(errors.New(reason))
	for _, rec := range r.pending {
		if !rec.cancelled {
			r.finish(rec, r.proxyError(rec, reason), false)
		}
		delete(r.pending, rec.msg.ID.Key)
	}
	r.results = nil
	for _, g := range r.readyBatches {
		r.deliverBatch(g, false)
	}
	r.readyBatches = nil
	r.resultBytes = 0
	r.progress = map[string]*record{}
	r.reverse = map[string]reverseRecord{}
	r.reverseBytes = 0
	p := r.proc
	go func() {
		ctx, c := context.WithTimeout(context.Background(), r.cfg.CleanupTimeout)
		defer c()
		r.emit(stopDone{p.ID, p.CloseContext(ctx)})
	}()
}
func (r *relay) stopped(e stopDone) {
	if r.proc == nil || r.proc.ID != e.boot {
		return
	}
	if e.err != nil {
		r.fatal = fmt.Errorf("backend cleanup failed: %w", e.err)
		return
	}
	r.proc = nil
	r.back = nil
	r.phase = "restarting"
	r.startAt = time.Time{}
}
func (r *relay) active() int {
	n := 0
	for _, rec := range r.pending {
		if !rec.long {
			n++
		}
	}
	return n
}
func (r *relay) expire() {
	now := time.Now()
	if !r.dirtyAt.IsZero() && now.Sub(r.dirtyAt) >= r.cfg.RefreshTimeout {
		r.fatal = errors.New("refresh_timeout: reconnect required")
		return
	}
	if (r.phase == "starting" || r.phase == "publishing" || r.phase == "recovering") && !r.startAt.IsZero() && now.Sub(r.startAt) >= r.cfg.StartupTimeout {
		r.fatal = errors.New("startup_timeout: reconnect required")
		return
	}
	if len(r.frontPending) > 0 && now.Sub(r.frontPending[0].at) >= r.cfg.RequestTimeout {
		r.fatal = errors.New("client output timeout")
		return
	}
	if len(r.backPending) > 0 && now.Sub(r.backPending[0].at) >= r.cfg.RequestTimeout {
		r.invalidate("backend_write_timeout")
	}
	for _, q := range r.queue {
		for _, rec := range q.records {
			if !rec.cancelled && now.After(rec.deadline) {
				r.finish(rec, r.proxyError(rec, "request_timeout"), false)
				rec.cancelled = true
			}
		}
	}
	r.pruneQueue()
	for _, rec := range r.pending {
		if !rec.cancelled && !rec.long && now.After(rec.deadline) {
			r.finish(rec, r.proxyError(rec, "request_timeout"), false)
			rec.cancelled = true
			r.invalidate("request_timeout")
		}
	}
	for id, rev := range r.reverse {
		if now.After(rev.deadline) {
			delete(r.reverse, id)
			r.invalidate("reverse_request_timeout")
		}
	}
}
func (r *relay) failControls(err error) {
	for id, c := range r.controls {
		c.reply <- internalReply{err: err}
		delete(r.controls, id)
	}
}
func (r *relay) internal(c internalCall) {
	if c.ctx.Err() != nil {
		c.reply <- internalReply{err: c.ctx.Err()}
		return
	}
	if r.proc == nil {
		c.reply <- internalReply{err: errors.New("backend unavailable")}
		return
	}
	f, err := wire.ParseFrame(c.raw)
	if err != nil || len(f.Messages) != 1 {
		c.reply <- internalReply{err: errors.New("invalid internal frame")}
		return
	}
	m := f.Messages[0]
	if c.notify {
		if m.Method == "notifications/initialized" {
			r.sendBack(c.raw)
		} else {
			r.sendFront(c.raw)
		}
		c.reply <- internalReply{}
		return
	}
	r.sequence++
	idraw, _ := json.Marshal(fmt.Sprintf("__proxy/internal/%d/%d", r.proc.ID, r.sequence))
	id, _ := wire.ParseID(idraw)
	b, err := wire.Replace(c.raw, idraw, "id")
	if err != nil {
		c.reply <- internalReply{err: err}
		return
	}
	r.controls[id.Key] = controlRequest{c.reply, m.ID}
	r.sendBack(b)
}
