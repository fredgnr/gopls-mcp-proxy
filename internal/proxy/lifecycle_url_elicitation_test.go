package proxy

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func TestOnDemandURLElicitationErrorPinsAfterWriterAck(t *testing.T) {
	r := lifecycleRelay(t)
	r.recovery = urlElicitationSession(t)
	p := lifecycleProcess(t, r)
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"native"}}`))
	r.queue[0].freshBoot = p.ID
	r.dispatch()
	back := routerRead(t, r.back)
	r.written(writeDone{boot: p.ID, size: len(back.raw)})
	native := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32042,"message":"Authorization required","data":{"elicitations":[{"mode":"url","elicitationId":"flow-1","url":"https://example.invalid/authorize","message":"Continue in browser"}]}}}`)
	r.fromBack(native)
	routerDeliverFresh(r)
	front := routerRead(t, r.front)
	if !bytes.Equal(front.raw, native) {
		t.Fatalf("native URL-required error changed: %s", front.raw)
	}
	r.written(writeDone{size: len(front.raw)})
	if r.toolWrittenAt.IsZero() || !r.callBoundaryIdle() {
		t.Fatal("regression did not reach the delivered, otherwise idle tool boundary")
	}
	r.advance()
	if r.phase != "ready" || r.proc != p || r.fatal != nil {
		t.Fatalf("retired unresolved URL flow after result ACK: phase=%s reason=%s blockers=%v", r.phase, r.stopReason, r.suspendBlockers())
	}
	requireSuspendBlocker(t, r.recovery, "url_elicitation_state")
	r.idleSince = time.Now().Add(-2 * r.cfg.BackendIdleTimeout)
	r.advance()
	if r.phase != "ready" {
		t.Fatal("ordinary idle timeout retired the unresolved URL flow")
	}
}

func TestOnDemandReverseURLElicitationAcceptPinsAfterRPCCompletes(t *testing.T) {
	r := lifecycleRelay(t)
	r.recovery = urlElicitationSession(t)
	p := lifecycleProcess(t, r)
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"native"}}`))
	r.queue[0].freshBoot = p.ID
	r.dispatch()
	back := routerRead(t, r.back)
	r.written(writeDone{boot: p.ID, size: len(back.raw)})
	native := []byte(`{"jsonrpc":"2.0","id":"server-url","method":"elicitation/create","params":{"mode":"url","elicitationId":"flow-1","url":"https://example.invalid/authorize","message":"Continue in browser"}}`)
	r.fromBack(native)
	front := routerRead(t, r.front)
	forwarded := routerFrame(t, front.raw).Messages[0]
	nativeParams, _ := wire.Get(native, "params")
	forwardedParams, _ := wire.Get(front.raw, "params")
	if !bytes.Equal(nativeParams, forwardedParams) {
		t.Fatal("URL elicitation parameters changed")
	}
	r.written(writeDone{size: len(front.raw)})
	r.fromFront([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"action":"accept"}}`, forwarded.ID.Raw)))
	back = routerRead(t, r.back)
	if !bytes.Equal(back.raw, []byte(`{"jsonrpc":"2.0","id":"server-url","result":{"action":"accept"}}`)) {
		t.Fatalf("URL accept reply changed beyond ID routing: %s", back.raw)
	}
	r.written(writeDone{boot: p.ID, size: len(back.raw)})
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	routerDeliverFresh(r)
	front = routerRead(t, r.front)
	r.written(writeDone{size: len(front.raw)})
	if len(r.reverse) != 0 || !r.callBoundaryIdle() || r.toolWrittenAt.IsZero() {
		t.Fatal("regression did not finish reverse RPC and terminal response delivery")
	}
	r.advance()
	if r.phase != "ready" || r.proc != p || r.fatal != nil {
		t.Fatalf("URL accept was mistaken for browser completion: phase=%s reason=%s blockers=%v", r.phase, r.stopReason, r.suspendBlockers())
	}
	requireSuspendBlocker(t, r.recovery, "url_elicitation_state")
}
