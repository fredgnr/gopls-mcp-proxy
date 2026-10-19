package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/backend"
	"github.com/fredgnr/gopls-mcp-proxy/internal/protocol"
	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
	"github.com/fredgnr/gopls-mcp-proxy/internal/workspace"
)

type routerTracker struct{ epoch atomic.Uint64 }

func (t *routerTracker) Scan(context.Context) (workspace.Snapshot, error) {
	return workspace.Snapshot{Epoch: t.epoch.Load()}, nil
}
func (*routerTracker) Watch(context.Context, func(string)) {}
func (t *routerTracker) Epoch() uint64                     { return t.epoch.Load() }

func testRelay(t *testing.T, version string) (*relay, *routerTracker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := DefaultConfig()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	tracker := &routerTracker{}
	r := &relay{ctx: ctx, cancel: cancel, cfg: cfg, tracker: tracker, events: make(chan any, 16), front: make(chan writeItem, 16), back: make(chan writeItem, 16), proc: &backend.Process{ID: 101}, phase: "ready", generation: 1, pending: map[string]*record{}, reverse: map[string]reverseRecord{}, controls: map[string]controlRequest{}, progress: map[string]*record{}, recovery: &Recovery{}}
	r.recovery.SetRevision(version)
	return r, tracker
}

func routerFrame(t *testing.T, raw []byte) wire.Frame {
	t.Helper()
	f, err := wire.ParseFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func routerRead(t *testing.T, ch <-chan writeItem) writeItem {
	t.Helper()
	select {
	case item := <-ch:
		return item
	default:
		t.Fatal("expected a queued wire frame")
		return writeItem{}
	}
}
func routerEmpty(t *testing.T, ch <-chan writeItem) {
	t.Helper()
	select {
	case item := <-ch:
		t.Fatalf("unexpected wire frame: %s", item.raw)
	default:
	}
}

func TestRouterQueuedCancellationReleasesBatch(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.fromFront([]byte(`[{"jsonrpc":"2.0","id":1,"method":"one"},{"jsonrpc":"2.0","id":"1","method":"two"}]`))
	if r.queueCount != 2 || r.queueBytes == 0 {
		t.Fatalf("queue accounting %d/%d", r.queueCount, r.queueBytes)
	}
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	if r.queueCount != 1 || len(r.queue) != 1 || len(r.queue[0].msgs) != 1 || r.queue[0].msgs[0].ID.Key != "s:1" {
		t.Fatalf("wrong batch member cancelled: %+v", r.queue)
	}
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"1"}}`))
	if r.queueCount != 0 || r.queueBytes != 0 || len(r.queue) != 0 {
		t.Fatalf("cancelled batch retains capacity: %d/%d", r.queueCount, r.queueBytes)
	}
	routerEmpty(t, r.back)
	routerEmpty(t, r.front)
}

func TestRouterBatchResponsesKeepGroupingAndTypedIDs(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.fromFront([]byte(`[{"jsonrpc":"2.0","id":1,"method":"one"},{"jsonrpc":"2.0","id":"1","method":"two"},{"jsonrpc":"2.0","id":9007199254740993,"method":"three"}]`))
	r.dispatch()
	if len(r.pending) != 3 {
		t.Fatalf("typed ID collision: %d", len(r.pending))
	}
	sent := routerFrame(t, routerRead(t, r.back).raw)
	if !sent.Batch || len(sent.Messages) != 3 {
		t.Fatal("request batch changed")
	}
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":"1","result":{"opaque":null}}`))
	routerDeliverFresh(r)
	routerEmpty(t, r.front)
	r.fromBack([]byte(`[{"jsonrpc":"2.0","id":9007199254740993,"result":{"big":9007199254740993}},{"jsonrpc":"2.0","id":1,"error":{"code":12345,"message":"native","data":null}}]`))
	routerDeliverFresh(r)
	returned := routerFrame(t, routerRead(t, r.front).raw)
	if !returned.Batch || len(returned.Messages) != 3 || len(r.pending) != 0 {
		t.Fatalf("response grouping: %+v pending=%d", returned, len(r.pending))
	}
	routerEmpty(t, r.front)
}

func TestRouterDuplicateIDWithinBatchRejected(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.fromFront([]byte(`[{"jsonrpc":"2.0","id":100,"method":"one"},{"jsonrpc":"2.0","id":1e2,"method":"two"}]`))
	if r.fatal == nil {
		t.Fatal("same numeric ID appeared twice in one outstanding batch")
	}
	routerEmpty(t, r.back)
}

func TestRouterMixedBatchMaintainsRequestNotificationOrder(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.fromFront([]byte(`[{"jsonrpc":"2.0","id":1,"method":"configure"},{"jsonrpc":"2.0","method":"vendor/after-configure","params":{"unknown":null}}]`))
	routerEmpty(t, r.back)
	r.dispatch()
	f := routerFrame(t, routerRead(t, r.back).raw)
	if !f.Batch || len(f.Messages) != 2 || f.Messages[0].Method != "configure" || f.Messages[1].Method != "vendor/after-configure" {
		t.Fatalf("causal order changed: %s", f.Raw)
	}
}

type routerPartialWriter struct{ bytes.Buffer }

func (w *routerPartialWriter) Write(p []byte) (int, error) {
	n, _ := w.Buffer.Write(p[:min(7, len(p))])
	return n, io.ErrClosedPipe
}

func TestRouterPartialWriteNeverReturnsBusinessToQueue(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":7,"method":"sideEffect","params":{"amount":1}}`))
	r.dispatch()
	rec := r.pending["n:7"]
	if rec == nil || !rec.dispatched || rec.boot != 101 || len(r.queue) != 0 {
		t.Fatal("dispatch ownership was not recorded before writing")
	}
	out := &routerPartialWriter{}
	go r.write(out, r.back, 101)
	select {
	case event := <-r.events:
		done, ok := event.(writeDone)
		if !ok || !errors.Is(done.err, io.ErrClosedPipe) {
			t.Fatalf("unexpected event %+v", event)
		}
		r.written(done)
	case <-time.After(time.Second):
		t.Fatal("partial writer did not terminate")
	}
	if out.Len() != 7 || len(r.queue) != 0 || !rec.dispatched || r.pending[rec.msg.ID.Key] != rec || r.phase != "draining" {
		t.Fatalf("uncertain write lost ownership or was replayed: queue=%d bytes=%d phase=%s", len(r.queue), out.Len(), r.phase)
	}
	errFrame := routerFrame(t, r.proxyError(rec, "backend_write_failed"))
	if raw, _ := wire.Get(errFrame.Raw, "error", "data", "requestDispatched"); string(raw) != "true" {
		t.Fatal("uncertain partial write marked undispatched")
	}
	routerEmpty(t, r.back)
}

func TestRouterDirtyEpochBetweenScanAndDispatchBlocksSend(t *testing.T) {
	r, tracker := testRelay(t, "2025-11-25")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"read"}`))
	checkpoint := workspace.Snapshot{Epoch: 0}
	tracker.epoch.Store(1)
	r.scanned(scanDone{purpose: "dispatch", snap: checkpoint})
	if len(r.pending) != 0 || len(r.queue) != 1 {
		t.Fatal("stale scan dispatched queued business")
	}
	routerEmpty(t, r.back)
	// A stale publish checkpoint cannot publish a candidate either.
	r.phase = "publishing"
	r.candidate = checkpoint
	r.scanned(scanDone{purpose: "publish", snap: checkpoint})
	if r.phase == "ready" || r.generation != 1 {
		t.Fatal("dirty candidate was published")
	}
}

func TestRouterDirtyEpochBetweenScanAndResultKeepsSuccessHidden(t *testing.T) {
	r, tracker := testRelay(t, "2025-11-25")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"read"}`))
	r.dispatch()
	routerRead(t, r.back)
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"result":{"value":"old"}}`))
	tracker.epoch.Store(1)
	frames := r.results
	r.results = nil
	r.scanned(scanDone{purpose: "results", snap: workspace.Snapshot{Epoch: 0}, results: frames})
	routerEmpty(t, r.front)
	if len(r.results) != 1 {
		t.Fatal("stale scan consumed the awaiting result")
	}
	frames = r.results
	r.results = nil
	r.scanned(scanDone{purpose: "results", snap: workspace.Snapshot{Epoch: 1}, results: frames})
	returned := routerRead(t, r.front).raw
	if _, ok := wire.Get(returned, "error"); !ok {
		t.Fatalf("old success escaped freshness check: %s", returned)
	}
	if raw, _ := wire.Get(returned, "error", "data", "backendResponseReceived"); string(raw) != "true" {
		t.Fatal("lost actual-response evidence")
	}
}

func TestRouterProgressMappingDoesNotTouchBusinessPayload(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"_meta":{"progressToken":"original"},"arguments":{"progressToken":"business","requestId":1}}}`))
	r.dispatch()
	sent := routerFrame(t, routerRead(t, r.back).raw).Messages[0]
	token, ok := protocol.ProgressToken(sent)
	if !ok || token.Key == "s:original" {
		t.Fatal("progress association was not isolated")
	}
	if string(vGet(sent.Raw, "params", "arguments", "progressToken")) != `"business"` {
		t.Fatal("opaque arguments rewritten")
	}
	back := append([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":`), token.Raw...)
	back = append(back, []byte(`,"progress":1,"future":null}}`)...)
	r.fromBack(back)
	got := routerRead(t, r.front).raw
	if string(vGet(got, "params", "progressToken")) != `"original"` || string(vGet(got, "params", "future")) != "null" {
		t.Fatalf("progress round trip changed metadata: %s", got)
	}
}

func vGet(raw []byte, path ...string) []byte { value, _ := wire.Get(raw, path...); return value }

func TestRouterDuplicateActiveProgressTokenIsolated(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"_meta":{"progressToken":"same"}}}`))
	r.dispatch()
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"y","params":{"_meta":{"progressToken":"same"}}}`))
	if r.fatal == nil && (r.phase != "draining" || len(r.queue) != 1 || r.canDispatch(r.queue[0])) {
		t.Fatal("ambiguous active client progress token can reach the same backend connection")
	}
}

func TestRouterSaturatedNotificationOutputTerminates(t *testing.T) {
	for _, direction := range []string{"frontend", "backend"} {
		t.Run(direction, func(t *testing.T) {
			r, _ := testRelay(t, "2025-11-25")
			r.cfg.QueueBytes = 1
			raw := []byte(`{"jsonrpc":"2.0","method":"notice"}`)
			if direction == "frontend" {
				r.sendFront(raw)
			} else {
				r.sendBack(raw)
			}
			if r.fatal == nil {
				t.Fatal("necessary notification silently dropped at byte capacity")
			}
			routerEmpty(t, r.front)
			routerEmpty(t, r.back)
		})
	}
}

func TestRouterRejectsModernBackendBatch(t *testing.T) {
	r, _ := testRelay(t, protocol.ModernVersion)
	r.fromBack([]byte(`[{"jsonrpc":"2.0","method":"vendor/notification"}]`))
	if r.fatal == nil {
		t.Fatal("modern backend emitted forbidden batch")
	}
	routerEmpty(t, r.front)
}

func TestRouterDuplicateReverseIDRejected(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"method":"roots/list"}`))
	routerRead(t, r.front)
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"method":"roots/list"}`))
	if r.fatal == nil {
		t.Fatal("same backend reverse ID has two ambiguous outstanding mappings")
	}
}

func routerDeliverFresh(r *relay) {
	frames := r.results
	r.results = nil
	r.deliverResults(frames, true, "")
	batches := r.readyBatches
	r.readyBatches = nil
	r.scanned(scanDone{purpose: "results", snap: r.snapshot, batches: batches})
}

func TestRouterScanCertifiesOnlyCapturedResponses(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"one"}`))
	r.dispatch()
	routerRead(t, r.back)
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"two"}`))
	r.dispatch()
	routerRead(t, r.back)
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"result":{"first":true}}`))
	captured := r.results
	r.results = nil
	// A second response arrives after the scan's observation boundary. It needs
	// a later scan even when the first checkpoint reports identical contents.
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":2,"result":{"second":true}}`))
	r.scanned(scanDone{purpose: "results", snap: workspace.Snapshot{}, results: captured})
	batches := r.readyBatches
	r.readyBatches = nil
	r.scanned(scanDone{purpose: "results", snap: r.snapshot, batches: batches})
	first := routerFrame(t, routerRead(t, r.front).raw)
	if first.Messages[0].ID.Key != "n:1" || len(r.results) != 1 || r.pending["n:2"] == nil {
		t.Fatal("scan certified a response that arrived after its boundary")
	}
	routerEmpty(t, r.front)
}

func TestRouterCancelledScanHeadCannotAuthorizeReplacementRequest(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"one"}`))
	captured := r.queue[0]
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"two"}`))
	r.scanned(scanDone{purpose: "dispatch", snap: workspace.Snapshot{}, dispatch: captured})
	if len(r.pending) != 0 || len(r.queue) != 1 {
		t.Fatal("old scan dispatched replacement queue head")
	}
	routerEmpty(t, r.back)
}

func TestRouterRejectedMixedBatchCannotSilentlyDropNotification(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.cfg.QueueLimit = 1
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"one"}`))
	r.fromFront([]byte(`[{"jsonrpc":"2.0","id":2,"method":"two"},{"jsonrpc":"2.0","method":"vendor/essential-notice"}]`))
	if r.fatal == nil {
		t.Fatal("mixed batch admission dropped a necessary notification without terminating its connection")
	}
	routerEmpty(t, r.back)
}

func TestRouterReverseBatchResponsesRemainGrouped(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.fromBack([]byte(`[{"jsonrpc":"2.0","id":1,"method":"roots/list"},{"jsonrpc":"2.0","id":"1","method":"sampling/createMessage","params":{}}]`))
	var reverse []wire.Message
	for len(reverse) < 2 {
		frame := routerFrame(t, routerRead(t, r.front).raw)
		reverse = append(reverse, frame.Messages...)
	}
	response := func(id wire.ID) []byte {
		raw := append([]byte(`{"jsonrpc":"2.0","id":`), id.Raw...)
		return append(raw, []byte(`,"result":{"opaque":null}}`)...)
	}
	r.fromFront(response(reverse[0].ID))
	routerEmpty(t, r.back)
	r.fromFront(response(reverse[1].ID))
	returned := routerFrame(t, routerRead(t, r.back).raw)
	if !returned.Batch || len(returned.Messages) != 2 {
		t.Fatalf("reverse batch response lost grouping: %s", returned.Raw)
	}
	if returned.Messages[0].ID.Key != "n:1" || returned.Messages[1].ID.Key != "s:1" {
		t.Fatalf("reverse batch original typed IDs lost: %s", returned.Raw)
	}
}

func TestRouterReverseProgressIsMappedAndOldBootCannotReceiveIt(t *testing.T) {
	r, _ := testRelay(t, "2025-11-25")
	r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage","params":{"_meta":{"progressToken":"backend-progress"},"messages":[]}}`))
	forwarded := routerFrame(t, routerRead(t, r.front).raw).Messages[0]
	token, ok := protocol.ProgressToken(forwarded)
	if !ok || token.Key == "s:backend-progress" {
		t.Fatal("reverse progress token was not isolated across boots")
	}
	progress := append([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":`), token.Raw...)
	progress = append(progress, []byte(`,"progress":1}}`)...)
	r.fromFront(progress)
	returned := routerRead(t, r.back).raw
	if string(vGet(returned, "params", "progressToken")) != `"backend-progress"` {
		t.Fatalf("reverse progress association failed: %s", returned)
	}
	// Retain the old association deliberately: boot ownership must still prevent
	// an old frontend message from reaching a replacement process.
	r.proc = &backend.Process{ID: 102}
	r.fromFront(progress)
	routerEmpty(t, r.back)
}

func TestRouterReverseBatchResponsesShareGlobalByteBudget(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.cfg.QueueBytes = 4096
	var firstIDs []wire.ID
	for batch := 0; batch < 3; batch++ {
		r.fromBack([]byte(fmt.Sprintf(`[{"jsonrpc":"2.0","id":%d,"method":"roots/list"},{"jsonrpc":"2.0","id":%d,"method":"roots/list"}]`, batch*2+1, batch*2+2)))
		var requests []wire.Message
		for len(requests) < 2 {
			requests = append(requests, routerFrame(t, routerRead(t, r.front).raw).Messages...)
		}
		firstIDs = append(firstIDs, requests[0].ID)
	}
	for _, id := range firstIDs {
		raw := append([]byte(`{"jsonrpc":"2.0","id":`), id.Raw...)
		raw = append(raw, []byte(`,"result":{"payload":"`+strings.Repeat("x", 2048)+`"}}`)...)
		r.fromFront(raw)
		if r.fatal != nil {
			break
		}
	}
	if r.fatal == nil {
		t.Fatal("partial reverse batches retained responses beyond global byte budget")
	}
}
