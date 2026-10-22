package proxy

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/backend"
	"github.com/fredgnr/gopls-mcp-proxy/internal/protocol"
	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func lifecycleRelay(t *testing.T) *relay {
	t.Helper()
	r, _ := testRelay(t, protocol.ModernVersion)
	r.cfg.RefreshMode = RefreshPerCall
	r.cfg.BackendLifecycle = BackendOnDemand
	r.poll = false
	return r
}

// Real cleanup tests use a pipe reader with no protocol goroutines; routing is
// driven deterministically, but Process.CloseContext/Wait are the real barrier.
func lifecycleProcess(t *testing.T, r *relay) *backend.Process {
	t.Helper()
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("process cleanup test requires the Unix cat utility")
	}
	factory, err := backend.NewFactory(backend.Config{Executable: cat, Logger: r.cfg.Logger, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	p, err := factory.Start(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	r.factory, r.proc = factory, p
	return p
}

func lifecycleStopped(t *testing.T, r *relay) {
	t.Helper()
	select {
	case event := <-r.events:
		e, ok := event.(stopDone)
		if !ok {
			t.Fatalf("unexpected cleanup event: %#v", event)
		}
		r.stopped(e)
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup barrier did not complete")
	}
}

func TestOnDemandConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.BackendLifecycle != BackendResident || cfg.BackendIdleTimeout != 10*time.Second {
		t.Fatal("resident compatibility defaults changed")
	}
	cfg.BackendLifecycle = BackendOnDemand
	if cfg.Validate() == nil {
		t.Fatal("accepted on-demand with on-change")
	}
	cfg.RefreshMode = RefreshPerCall
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.BackendIdleTimeout = 0
	if cfg.Validate() == nil {
		t.Fatal("accepted an unbounded/zero idle timeout")
	}
	cfg.BackendIdleTimeout = time.Second
	cfg.BackendLifecycle = "automatic"
	if cfg.Validate() == nil {
		t.Fatal("accepted unknown lifecycle")
	}
}

func TestOnDemandToolReservationWaitsForExactWriterAck(t *testing.T) {
	r := lifecycleRelay(t)
	lifecycleProcess(t, r)
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	r.queue[0].freshBoot = r.proc.ID
	r.dispatch()
	back := routerRead(t, r.back)
	r.written(writeDone{boot: r.proc.ID, size: len(back.raw)})
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	routerDeliverFresh(r)
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	if len(r.pending) != 0 || !r.retirementPending() || r.canDispatch(r.queue[0]) {
		t.Fatal("pending removal allowed an ordinary request into the completed tool boot")
	}
	r.advance()
	if r.phase != "ready" || !r.toolWrittenAt.IsZero() {
		t.Fatal("retired before the result frame write succeeded")
	}
	item := routerRead(t, r.front)
	r.written(writeDone{size: len(item.raw)})
	if r.toolWrittenAt.IsZero() {
		t.Fatal("result writer acknowledgement was not recorded")
	}
	r.advance()
	if r.phase != "stopping" || r.stopReason != "call_complete" {
		t.Fatal("safe tool completion waited for an idle timeout or queued ping")
	}
	lifecycleStopped(t, r)
	if r.phase != "idle" || r.proc != nil || len(r.queue) != 1 || r.queue[0].records[0].dispatched {
		t.Fatal("cleanup did not preserve never-dispatched successor demand")
	}
	routerEmpty(t, r.front)
}

func TestOnDemandFailedWriterNeverAuthorizesRelease(t *testing.T) {
	r := lifecycleRelay(t)
	r.retiringCall = r.proc.ID
	r.sendFrontResult([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), r.proc.ID)
	item := routerRead(t, r.front)
	r.written(writeDone{size: len(item.raw), err: io.ErrClosedPipe})
	if r.fatal == nil || !r.toolWrittenAt.IsZero() {
		t.Fatal("failed result write was treated as successful delivery")
	}
}

func TestOnDemandDormancyDoesNotSpendRecoveryBudget(t *testing.T) {
	r := lifecycleRelay(t)
	r.proc = nil
	r.phase = "restarting"
	r.dirtyAt, r.wakeAt, r.startAt = time.Now().Add(-time.Hour), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)
	r.advance()
	r.expire()
	if r.phase != "idle" || r.fatal != nil || !r.refreshDeadline().IsZero() {
		t.Fatal("old wake budget survived dormancy")
	}
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	requestDeadline := r.queue[0].deadline
	r.beginWake()
	if time.Until(r.refreshDeadline()) < r.cfg.RefreshTimeout-time.Second {
		t.Fatal("fresh demand inherited an old recovery deadline")
	}
	if got := r.demandDeadline(time.Now().Add(time.Hour)); !got.Equal(requestDeadline) {
		t.Fatal("wake extended the request's enqueue deadline")
	}
}

func TestOnDemandCanceledScanCannotPublishOrClearNextScan(t *testing.T) {
	r := lifecycleRelay(t)
	r.proc = nil
	r.phase = "idle"
	r.scanning, r.scanSequence = true, 7
	ctx, cancel := context.WithCancel(context.Background())
	r.scanCancel = cancel
	r.wakeAt = time.Now()
	r.advance()
	if ctx.Err() == nil || r.scanning || r.phase != "idle" {
		t.Fatal("unneeded initial scan was not abandoned")
	}
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	r.scanning, r.scanSequence = true, 9
	r.scanned(scanDone{token: 7, purpose: "initial", snap: r.snapshot})
	if !r.scanning || r.phase != "idle" || r.startFinished != nil || r.queue[0].freshBoot != 0 {
		t.Fatal("late old scan cleared the next scan or created a candidate")
	}
}

func TestOnDemandCanceledStartWithSuccessorUsesFreshWake(t *testing.T) {
	r := lifecycleRelay(t)
	p := lifecycleProcess(t, r)
	r.proc = nil
	r.phase = "starting"
	r.startAt, r.wakeAt = time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	r.startCancel = cancel
	r.advance()
	r.expire()
	if !r.abandonedStart || ctx.Err() == nil || r.fatal != nil {
		t.Fatal("canceled startup consumed an obsolete timeout budget")
	}
	// The former Start succeeded just before cancellation was observed; a new
	// demand arrives before its result is processed. Do not donate the process.
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`))
	r.started(startDone{proc: p, snap: r.snapshot})
	if r.phase != "stopping" || !r.stopToIdle {
		t.Fatal("abandoned successful candidate was reused by its successor")
	}
	for r.phase == "stopping" {
		select {
		case ev := <-r.events:
			if done, ok := ev.(stopDone); ok {
				r.stopped(done)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("abandoned candidate did not clean up")
		}
	}
	if r.phase != "idle" || r.abandonedStart || len(r.queue) != 1 || r.queue[0].freshBoot != 0 {
		t.Fatal("abandonment leaked into the successor attempt")
	}
	r.beginWake()
	if r.wakeAt.IsZero() || time.Until(r.refreshDeadline()) < r.cfg.RefreshTimeout-time.Second {
		t.Fatal("successor wake inherited abandoned timing")
	}
}

func TestOnDemandAbandonedStartStillHasCleanupDeadline(t *testing.T) {
	r := lifecycleRelay(t)
	r.proc = nil
	r.phase = "starting"
	r.abandonedStart = true
	r.abandonedAt = time.Now().Add(-r.cfg.CleanupTimeout)
	r.expire()
	if r.fatal == nil {
		t.Fatal("an unresponsive cancelled factory can wait indefinitely")
	}
}

func TestOnDemandCanceledDispatchDoesNotPinPristineCandidate(t *testing.T) {
	r := lifecycleRelay(t)
	lifecycleProcess(t, r)
	r.recovery = &Recovery{}
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	q := r.queue[0]
	r.scanning, r.scanSequence = true, 3
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	r.advance()
	if r.phase != "stopping" || r.stopReason != "request_cancelled" {
		t.Fatal("pristine candidate retained by a handshake that never dispatched")
	}
	r.scanned(scanDone{token: 3, purpose: "dispatch", snap: r.snapshot, dispatch: q})
	lifecycleStopped(t, r)
	if r.phase != "idle" || r.proc != nil || len(r.pending) != 0 || len(r.queue) != 0 {
		t.Fatal("canceled dispatch revived the unused boot")
	}
	routerEmpty(t, r.back)
	routerEmpty(t, r.front)
}

func TestOnDemandFileEventsDuringCleanupDoNotRestartWakeBudget(t *testing.T) {
	r := lifecycleRelay(t)
	lifecycleProcess(t, r)
	r.wakeAt = time.Now().Add(-time.Hour)
	r.stopCurrent("call_complete")
	r.invalidate("watcher_overflow")
	r.expire()
	if r.fatal != nil || !r.refreshDeadline().IsZero() {
		t.Fatal("a file event restarted an obsolete wake deadline during retirement")
	}
	lifecycleStopped(t, r)
	if r.phase != "idle" {
		t.Fatal("file event restarted an otherwise dormant backend")
	}
}

func TestOnDemandOldRecoveryAndStoppedTrafficCannotCrossBoot(t *testing.T) {
	r := lifecycleRelay(t)
	r.phase = "recovering"
	reply := make(chan internalReply, 1)
	r.internal(internalCall{ctx: r.ctx, boot: r.proc.ID - 1, raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), reply: reply})
	if got := <-reply; got.err == nil {
		t.Fatal("old recovery request entered a new backend")
	}
	r.phase = "stopping"
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":7,"method":"roots/list"}`))
	r.fromBack([]byte(`{"jsonrpc":"2.0","method":"notifications/future_state"}`))
	if len(r.reverse) != 0 || r.recovery.NeedsResident() {
		t.Fatal("retired traffic changed the logical session")
	}
	routerEmpty(t, r.front)
	routerEmpty(t, r.back)
}

func TestOnDemandCleanupFailureCannotRepeatDeliveredResult(t *testing.T) {
	r := lifecycleRelay(t)
	r.phase = "stopping"
	r.retiringCall = r.proc.ID
	r.toolWrittenAt = time.Now()
	r.stopToIdle = true
	r.stopped(stopDone{boot: r.proc.ID, err: errors.New("wait not confirmed")})
	if r.fatal == nil || r.proc == nil || r.phase != "stopping" {
		t.Fatal("cleanup failure opened the next process barrier")
	}
	routerEmpty(t, r.front)
}

func TestOnDemandBackgroundWorkDoesNotRenewIdleDeadline(t *testing.T) {
	r := lifecycleRelay(t)
	r.idleSince = time.Now().Add(-time.Hour)
	before := r.idleSince
	r.scanning = true
	if r.advanceLifecycle() || !r.idleSince.Equal(before) {
		t.Fatal("poll scan restarted ordinary idle grace")
	}
	r.scanning = false
	r.fromBack([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"still alive"}}`))
	if r.advanceLifecycle() || !r.idleSince.Equal(before) {
		t.Fatal("backend log restarted ordinary idle grace")
	}
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if !r.idleSince.IsZero() || r.advanceLifecycle() || !r.canDispatch(r.queue[0]) {
		t.Fatal("new demand lost an idle timer race")
	}
}

func TestOnDemandCanceledLongSubscriptionWaitsOnlyForCancellationWrite(t *testing.T) {
	r := lifecycleRelay(t)
	m, _ := wire.ParseMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`))
	r.pending[m.ID.Key] = &record{msg: m, boot: r.proc.ID, long: true}
	if !r.hasDemand() || len(r.suspendBlockers()) == 0 {
		t.Fatal("active long subscription did not retain backend")
	}
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	if len(r.suspendBlockers()) != 0 || r.callBoundaryIdle() {
		t.Fatal("cancel did not remove ongoing demand or bypassed its pending write")
	}
	item := routerRead(t, r.back)
	r.written(writeDone{boot: r.proc.ID, size: len(item.raw)})
	if !r.callBoundaryIdle() || r.hasDemand() {
		t.Fatal("cancelled subscription requires a terminal response that may never arrive")
	}
}
