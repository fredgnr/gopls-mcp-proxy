package proxy

import (
	"bytes"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func TestPerCallRefreshModeValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RefreshMode != RefreshOnChange || cfg.Validate() != nil {
		t.Fatal("default refresh behavior changed")
	}
	cfg.RefreshMode = RefreshPerCall
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "always", "tools/call"} {
		cfg.RefreshMode = invalid
		if cfg.Validate() == nil {
			t.Fatalf("accepted refresh mode %q", invalid)
		}
	}
}

func TestPerCallSchedulerWaitsForRequestAndResponseDelivery(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.cfg.RefreshMode = RefreshPerCall
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"fixture/hold"}`))
	r.dispatch()
	sent := routerRead(t, r.back)
	r.written(writeDone{boot: r.proc.ID, size: len(sent.raw)})
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"opaque"}}`))
	r.poll = false
	r.advance()
	if r.phase != "ready" || !r.dirtyAt.IsZero() || !r.drainAt.IsZero() {
		t.Fatal("queued tool call started the drain timer on an active request")
	}
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"result":{"complete":true}}`))
	routerDeliverFresh(r)
	if len(r.pending) != 0 || len(r.frontPending) != 1 {
		t.Fatal("test must reach the buffered frontend write boundary")
	}
	r.advance()
	if r.phase != "ready" || !r.dirtyAt.IsZero() {
		t.Fatal("queued tool replaced backend before previous result was written")
	}
	reply := routerRead(t, r.front)
	r.written(writeDone{size: len(reply.raw)})
	r.advance()
	if r.phase != "draining" || r.dirtyAt.IsZero() || len(r.queue) != 1 || r.queue[0].records[0].dispatched {
		t.Fatal("idle call boundary did not force a fresh process before dispatch")
	}
	routerEmpty(t, r.back)
}

func TestPerCallWaitingRetainsPolling(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.cfg.RefreshMode = RefreshPerCall
	r.cfg.DisableWatcher = true
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"fixture/hold"}`))
	r.dispatch()
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`))
	r.advance()
	select {
	case event := <-r.events:
		result, ok := event.(scanDone)
		if !ok || result.purpose != "poll" {
			t.Fatalf("waiting call blocked periodic scan: %#v", event)
		}
		r.scanned(result)
	case <-time.After(time.Second):
		t.Fatal("waiting call prevented polling fallback")
	}
	if r.phase != "ready" || !r.dirtyAt.IsZero() {
		t.Fatal("unchanged periodic scan refreshed an active operation")
	}
}

func TestPerCallCancelledHeadCannotDonateFreshBoot(t *testing.T) {
	r, _ := testRelay(t, "2026-07-28")
	r.cfg.RefreshMode = RefreshPerCall
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	old := r.queue[0]
	old.freshBoot = r.proc.ID // This process was started for the first call.
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`))
	r.scanned(scanDone{purpose: "dispatch", snap: r.snapshot, dispatch: old})
	r.scanned(scanDone{purpose: "dispatch", snap: r.snapshot, dispatch: r.queue[0]})
	if len(r.pending) != 0 || r.canDispatch(r.queue[0]) {
		t.Fatal("replacement queue head borrowed the cancelled request's process")
	}
	r.poll = false
	r.advance()
	if r.phase != "draining" {
		t.Fatal("replacement call did not require another process")
	}
	routerEmpty(t, r.back)
	routerEmpty(t, r.front)
}

func TestPerCallToolOwnsFiniteDispatchAndControls(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.cfg.RefreshMode = RefreshPerCall
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"opaque"}}`))
	r.queue[0].freshBoot = r.proc.ID
	if !r.canDispatch(r.queue[0]) {
		t.Fatal("fresh tool could not dispatch")
	}
	r.dispatch()
	sent := routerRead(t, r.back)
	r.written(writeDone{boot: r.proc.ID, size: len(sent.raw)})
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"fixture/echo"}`))
	if r.canDispatch(r.queue[0]) {
		t.Fatal("ordinary request overlapped active tools/call")
	}
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":"roots","method":"roots/list"}`))
	reverse := routerFrame(t, routerRead(t, r.front).raw).Messages[0]
	r.fromFront(append(append([]byte(`{"jsonrpc":"2.0","id":`), reverse.ID.Raw...), []byte(`,"result":{"roots":[]}}`)...))
	forwarded := routerFrame(t, routerRead(t, r.back).raw).Messages[0]
	if forwarded.Kind != wire.Response || forwarded.ID.Key != "s:roots" || len(r.reverse) != 0 {
		t.Fatal("reverse response could not reach the active tool's process")
	}
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`))
	if got := routerFrame(t, routerRead(t, r.back).raw).Messages[0]; got.Method != "notifications/roots/list_changed" {
		t.Fatal("control notification did not reach active process")
	}
	if r.phase != "ready" || !r.dirtyAt.IsZero() {
		t.Fatal("protocol control traffic triggered a per-call restart")
	}
}

func TestPerCallBatchRejectedBeforeDispatch(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.cfg.RefreshMode = RefreshPerCall
	r.fromFront([]byte(`[{"jsonrpc":"2.0","id":7,"method":"tools/call"},{"jsonrpc":"2.0","id":"7","method":"fixture/echo"}]`))
	response := routerFrame(t, routerRead(t, r.front).raw)
	if !response.Batch || len(response.Messages) != 2 || response.Messages[0].ID.Key != "n:7" || response.Messages[1].ID.Key != "s:7" {
		t.Fatal("batch rejection lost grouping or typed IDs")
	}
	for _, m := range response.Messages {
		if !bytes.Contains(m.Raw, []byte(`"reason":"per_call_batch_unsupported"`)) || !bytes.Contains(m.Raw, []byte(`"requestDispatched":false`)) {
			t.Fatalf("ambiguous batch rejection: %s", m.Raw)
		}
	}
	if r.fatal != nil || len(r.queue) != 0 || len(r.pending) != 0 {
		t.Fatal("rejected pure-request batch entered dispatch state")
	}
	routerEmpty(t, r.back)
}

func TestPerCallMixedBatchRejectedBeforeControlSideEffects(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.cfg.RefreshMode = RefreshPerCall
	r.fromFront([]byte(`[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":1,"method":"tools/call"}]`))
	if r.fatal == nil || len(r.recovery.initialized) != 0 || len(r.queue) != 0 {
		t.Fatal("mixed per-call batch partially changed protocol state")
	}
	routerEmpty(t, r.back)
	routerEmpty(t, r.front)
}

func TestPerCallRejectedBatchCannotReuseOutstandingIDs(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		r, _ := testRelay(t, "2025-03-26")
		r.cfg.RefreshMode = RefreshPerCall
		r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"fixture/hold"}`))
		if dispatched {
			r.dispatch()
			routerRead(t, r.back)
		}
		r.fromFront([]byte(`[{"jsonrpc":"2.0","id":1,"method":"tools/call"}]`))
		if r.fatal == nil {
			t.Fatalf("rejected batch reused outstanding ID (dispatched=%v)", dispatched)
		}
		routerEmpty(t, r.front)
		routerEmpty(t, r.back)
	}
}

func TestPerCallToolNotificationDoesNotForceRestart(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.cfg.RefreshMode = RefreshPerCall
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"invalid-notification"}}`))
	q := r.queue[0]
	if q.isToolCall() || !r.canDispatch(q) {
		t.Fatal("notification was treated as a finite tools/call request")
	}
	r.scanned(scanDone{purpose: "dispatch", snap: r.snapshot, dispatch: q})
	if r.phase != "ready" || !r.dirtyAt.IsZero() {
		t.Fatal("tool notification caused a process refresh")
	}
	routerRead(t, r.back)
}
