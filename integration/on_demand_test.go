package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/testbackend"
)

func onDemandPeer(t *testing.T, f *fixture, extra ...string) *rawPeer {
	t.Helper()
	flags := []string{"--refresh-mode", "per-call", "--backend-lifecycle", "on-demand", "--backend-idle-timeout", "150ms"}
	return f.proxyFlags(t, append(flags, extra...))
}

func onDemandBoots(t *testing.T, f *fixture) []testbackend.Event {
	t.Helper()
	var boots []testbackend.Event
	for _, e := range f.events(t) {
		if e.Kind == "boot" {
			boots = append(boots, e)
		}
	}
	return boots
}

func onDemandAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}

func awaitOnDemandExit(t *testing.T, f *fixture, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !onDemandAlive(pid) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("backend %d did not retire; events=%+v", pid, f.events(t))
}

func requireOnDemandQuiet(t *testing.T, f *fixture, count int, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if got := len(onDemandBoots(t, f)); got != count {
			t.Fatalf("no demand, but backend count changed: got %d boots, want %d", got, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// M-L01/02/07/10/13: dormancy is distinct from initial startup. Long sleep
// cannot spend the next wake's recovery budget, and neither poll nor old
// control messages create demand. The changed input has equal size and mtime.
func TestRawOnDemandDormantWakeAndFreshBudget(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f, "--refresh-timeout", "500ms", "--poll-interval", "25ms")
	requireOnDemandQuiet(t, f, 0, 100*time.Millisecond)
	p.initialize("2025-11-25")
	initial := p.snapshot("1", false)
	p.call("2", "tools/list", `{}`)
	awaitOnDemandExit(t, f, initial.PID)
	count := len(onDemandBoots(t, f))
	writeInput(t, f.inputs[0], "B", true)
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"already-finished"}}`)
	p.send(`{"jsonrpc":"2.0","id":"old-reverse","result":{"roots":[]}}`)
	requireOnDemandQuiet(t, f, count, 650*time.Millisecond)
	after := perCallSnapshot(t, p, "3", false)
	requirePerCallBoot(t, initial, after)
	requireInput(t, after, "main.go", "B")
	if !bytes.Contains(after.Initialize, []byte(`"actual-client"`)) {
		t.Fatalf("logical initialization lost during sleep: %s", after.Initialize)
	}
	awaitOnDemandExit(t, f, after.PID)
	if got := len(onDemandBoots(t, f)); got != count+1 {
		t.Fatalf("tool wake created redundant candidate: boots=%d want=%d", got, count+1)
	}
}

// M-L03: initialize success without initialized is not a complete handshake.
func TestRawOnDemandWaitsForInitialized(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f)
	p.call("100", "initialize", `{"protocolVersion":"2025-11-25","clientInfo":{"name":"actual-client","version":"7"},"capabilities":{}}`)
	boots := onDemandBoots(t, f)
	if len(boots) != 1 {
		t.Fatalf("initialize boots: %+v", boots)
	}
	requireOnDemandQuiet(t, f, 1, 350*time.Millisecond)
	if !onDemandAlive(boots[0].PID) {
		t.Fatal("incomplete handshake backend was retired")
	}
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	awaitOnDemandExit(t, f, boots[0].PID)
}

// M-L04/19: release after both native success and native terminal error must
// not wait for the ordinary 10-second timeout. Keep resident tests unchanged.
func TestRawOnDemandToolRetiresImmediately(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern=%t", modern), func(t *testing.T) {
			f := newFixture(t)
			p := onDemandPeer(t, f, "--backend-idle-timeout", "10s")
			if !modern {
				p.initialize("2025-11-25")
			}
			first := perCallSnapshot(t, p, "1", modern)
			awaitOnDemandExit(t, f, first.PID)
			params := `{"name":"unknown_native_error"}`
			if modern {
				params = modernParams(`"name":"unknown_native_error"`)
			}
			raw := p.call("2", "tools/call", params)
			if !bytes.Contains(raw, []byte(`"code":-32602`)) || bytes.Contains(raw, []byte(`automaticReplay`)) {
				t.Fatalf("native terminal error was changed: %s", raw)
			}
			calls := perCallExecutions(t, f)
			if len(calls) != 2 || calls[0].Boot == calls[1].Boot {
				t.Fatalf("independent calls: %+v", calls)
			}
			awaitOnDemandExit(t, f, calls[1].PID)
			requireOnDemandQuiet(t, f, len(onDemandBoots(t, f)), 200*time.Millisecond)
		})
	}
}

// M-L05/06: the client withholds a reverse response while an ordinary request
// queues behind the tool. The active backend stays alive; after completion the
// ordinary request must run in a successor, even though it was already queued.
func TestRawOnDemandReverseAndQueuedOrdinaryBoundary(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f, "--max-concurrent", "1", "--backend-idle-timeout", "10s")
	p.initialize("2025-11-25")
	p.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_reverse"}}`)
	raw := p.next()
	var reverse struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(raw, &reverse) != nil || reverse.Method != "roots/list" {
		t.Fatalf("reverse frame: %s", raw)
	}
	calls := perCallExecutions(t, f)
	if len(calls) != 1 {
		t.Fatalf("active tool: %+v", calls)
	}
	p.send(`{"jsonrpc":"2.0","id":2,"method":"fixture/snapshot","params":{}}`)
	time.Sleep(250 * time.Millisecond)
	if !onDemandAlive(calls[0].PID) {
		t.Fatal("retired tool before reverse exchange completed")
	}
	for _, event := range f.events(t) {
		if event.Kind == "execution" && event.Method == "fixture/snapshot" {
			t.Fatal("ordinary request bypassed max-concurrent")
		}
	}
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"roots":[]}}`, reverse.ID))
	p.response("1")
	var next struct {
		Result testbackend.Snapshot `json:"result"`
	}
	raw = p.response("2")
	if json.Unmarshal(raw, &next) != nil || next.Result.Boot == "" || next.Result.Boot == calls[0].Boot {
		t.Fatalf("ordinary request extended completed tool backend: %s", raw)
	}
	requireBackendExited(t, calls[0].PID)
}

// M-L11: a cursor belongs to its physical process. An unfinished page pins it;
// completing that page allows the ordinary idle timer to retire the process.
func TestRawOnDemandPaginationPinsBackend(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call", "--backend-lifecycle", "on-demand", "--backend-idle-timeout", "150ms"}, "--lifecycle-paged")
	p.initialize("2025-11-25")
	first := p.snapshot("1", false)
	raw := p.call("2", "resources/list", `{}`)
	var page struct {
		Result struct {
			NextCursor string `json:"nextCursor"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Result.NextCursor == "" {
		t.Fatalf("first page: %s", raw)
	}
	time.Sleep(350 * time.Millisecond)
	if !onDemandAlive(first.PID) {
		t.Fatal("partial directory was retired")
	}
	raw = p.call("3", "resources/list", fmt.Sprintf(`{"cursor":%q}`, page.Result.NextCursor))
	if bytes.Contains(raw, []byte(`"error"`)) || !bytes.Contains(raw, []byte(`fixture:///last`)) {
		t.Fatalf("cursor invalidated by idle release: %s", raw)
	}
	awaitOnDemandExit(t, f, first.PID)
}

func TestRawOnDemandLegacySubscriptionPinsBackend(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f)
	p.initialize("2025-11-25")
	first := p.snapshot("1", false)
	p.call("2", "resources/subscribe", `{"uri":"fixture:///resource"}`)
	time.Sleep(350 * time.Millisecond)
	if !onDemandAlive(first.PID) {
		t.Fatal("active resource subscription was retired")
	}
	if next := p.snapshot("3", false); next.Boot != first.Boot {
		t.Fatal("subscription backend replaced without demand")
	}
	p.call("4", "resources/unsubscribe", `{"uri":"fixture:///resource"}`)
	awaitOnDemandExit(t, f, first.PID)
}

func TestRawOnDemandModernSubscriptionPinsBackend(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f)
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":%s}`, modernParams(`"notifications":{"toolsListChanged":true}`)))
	ack := p.next()
	if !bytes.Contains(ack, []byte(`notifications/subscriptions/acknowledged`)) {
		t.Fatalf("ack: %s", ack)
	}
	boots := onDemandBoots(t, f)
	if len(boots) != 1 {
		t.Fatalf("subscription boots: %+v", boots)
	}
	time.Sleep(350 * time.Millisecond)
	if !onDemandAlive(boots[0].PID) {
		t.Fatal("modern long subscription was excluded from suspension blockers")
	}
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	awaitOnDemandExit(t, f, boots[0].PID)
}

func TestRawOnDemandMRTRPinsBackend(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f)
	raw := p.call("1", "fixture/mrtr", modernParams(`"opaque":true`))
	var first struct {
		Result struct {
			RequestState json.RawMessage `json:"requestState"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &first) != nil || !bytes.Contains(raw, []byte(`input_required`)) {
		t.Fatalf("MRTR first turn: %s", raw)
	}
	boots := onDemandBoots(t, f)
	time.Sleep(350 * time.Millisecond)
	if len(boots) != 1 || !onDemandAlive(boots[0].PID) {
		t.Fatal("input_required state was discarded")
	}
	raw = p.call("2", "fixture/mrtr", modernParams(`"requestState":`+string(first.Result.RequestState)))
	if bytes.Contains(raw, []byte(`"error"`)) || !bytes.Contains(raw, []byte(`"resultType":"complete"`)) {
		t.Fatalf("MRTR token lost its boot: %s", raw)
	}
}

// M-L13: notifications are real backend demand. Unknown notifications are
// forwarded verbatim and conservatively keep the backend; old cancels do not.
func TestRawOnDemandNotificationWake(t *testing.T) {
	for _, method := range []string{"notifications/roots/list_changed", "notifications/future_state"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t)
			p := onDemandPeer(t, f)
			p.initialize("2025-11-25")
			first := p.snapshot("1", false)
			awaitOnDemandExit(t, f, first.PID)
			notice := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":{"opaque":900719925474099312345}}`, method)
			p.send(notice)
			p.call("2", "ping", `{}`)
			boots := onDemandBoots(t, f)
			if len(boots) != 2 {
				t.Fatalf("notification wake boots: %+v", boots)
			}
			received := false
			for _, e := range f.events(t) {
				if e.Kind == "receipt" && e.Raw == notice && e.Boot == boots[1].Boot {
					received = true
				}
			}
			if !received {
				t.Fatal("notification was not forwarded verbatim after wake")
			}
			if method == "notifications/future_state" {
				time.Sleep(350 * time.Millisecond)
				if !onDemandAlive(boots[1].PID) {
					t.Fatal("unknown notification state was discarded")
				}
			} else {
				awaitOnDemandExit(t, f, boots[1].PID)
			}
		})
	}
}

// M-L18: ping is forwarded to the real backend and resets ordinary idle time.
func TestRawOnDemandPingActivity(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f, "--backend-idle-timeout", "300ms")
	p.initialize("2025-11-25")
	first := p.snapshot("1", false)
	for i := 0; i < 5; i++ {
		time.Sleep(80 * time.Millisecond)
		p.call(fmt.Sprint(i+2), "ping", `{}`)
	}
	boots := onDemandBoots(t, f)
	if len(boots) != 1 || !onDemandAlive(first.PID) {
		t.Fatalf("active heartbeat backend retired: %+v", boots)
	}
	pings := 0
	for _, e := range f.events(t) {
		if e.Kind == "execution" && e.Method == "ping" {
			pings++
		}
	}
	if pings != 5 {
		t.Fatalf("ping was synthesized locally: observed %d upstream calls", pings)
	}
	awaitOnDemandExit(t, f, first.PID)
}

// M-L09/15: cancellation and failed dispatch never manufacture replay demand.
func TestRawOnDemandCancelledAndFailedCallsDoNotReplay(t *testing.T) {
	for _, tool := range []string{"fixture_hold", "fixture_crash", "fixture_half"} {
		t.Run(tool, func(t *testing.T) {
			f := newFixture(t)
			p := onDemandPeer(t, f, "--request-timeout", "2s")
			p.initialize("2025-11-25")
			raw := p.call("1", "tools/call", fmt.Sprintf(`{"name":%q}`, tool))
			if !bytes.Contains(raw, []byte(`"requestDispatched":true`)) || !bytes.Contains(raw, []byte(`"automaticReplay":false`)) {
				t.Fatalf("failed dispatch accounting: %s", raw)
			}
			calls := perCallExecutions(t, f)
			if len(calls) != 1 {
				t.Fatalf("failed tool executed more than once: %+v", calls)
			}
			awaitOnDemandExit(t, f, calls[0].PID)
			requireOnDemandQuiet(t, f, len(onDemandBoots(t, f)), 250*time.Millisecond)
			after := perCallSnapshot(t, p, "2", false)
			awaitOnDemandExit(t, f, after.PID)
			if got := len(perCallExecutions(t, f)); got != 2 {
				t.Fatalf("failed call replayed: %d executions", got)
			}
		})
	}
	t.Run("cancelled_queue", func(t *testing.T) {
		f := newFixture(t)
		p := onDemandPeer(t, f)
		p.initialize("2025-11-25")
		p.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_hold"}}`)
		f.waitExecution(t, "tools/call")
		p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"future_tool"}}`)
		p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
		p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
		calls := perCallExecutions(t, f)
		awaitOnDemandExit(t, f, calls[0].PID)
		requireOnDemandQuiet(t, f, len(onDemandBoots(t, f)), 250*time.Millisecond)
		after := perCallSnapshot(t, p, "3", false)
		awaitOnDemandExit(t, f, after.PID)
		calls = perCallExecutions(t, f)
		if len(calls) != 2 || string(calls[1].ID) != "3" {
			t.Fatalf("cancelled queue item executed: %+v", calls)
		}
	})
}

// Repeated finite calls keep the frontend connected but leave no live backend.
// This is process ownership evidence, not a claim about tiny Go heap changes.
func TestRawOnDemandHundredCallsRetireEveryBoot(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f)
	p.initialize("2025-11-25")
	var previous testbackend.Snapshot
	for i := 0; i < 100; i++ {
		current := perCallSnapshot(t, p, fmt.Sprint(i+1000), false)
		if i > 0 {
			requirePerCallBoot(t, previous, current)
		}
		awaitOnDemandExit(t, f, current.PID)
		previous = current
	}
	boots := onDemandBoots(t, f)
	if len(boots) != 101 || len(perCallExecutions(t, f)) != 100 {
		t.Fatalf("unexpected boot/call growth: %d/%d", len(boots), len(perCallExecutions(t, f)))
	}
	for _, boot := range boots {
		requireBackendExited(t, boot.PID)
	}
	requireOnDemandQuiet(t, f, len(boots), 200*time.Millisecond)
}

// Task results are protocol state, even when their enclosing tools/call has
// already sent a terminal JSON-RPC response. Retain the owning backend.
func TestRawOnDemandTaskResultPinsBackend(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f)
	p.initialize("2025-11-25")
	raw := p.call("1", "tools/call", `{"name":"fixture_task"}`)
	if !bytes.Contains(raw, []byte(`"taskId"`)) {
		t.Fatalf("task result: %s", raw)
	}
	calls := perCallExecutions(t, f)
	if len(calls) != 1 {
		t.Fatalf("task executions: %+v", calls)
	}
	time.Sleep(350 * time.Millisecond)
	if !onDemandAlive(calls[0].PID) {
		t.Fatal("task-owning backend was retired after result")
	}
	if after := p.snapshot("2", false); after.Boot != calls[0].Boot {
		t.Fatal("task backend unexpectedly replaced")
	}
}

// M-L20: waking restores the directory invalidation contract before forwarding
// the already-constructed request. The proxy does not rename a tool or invent
// success for a request based on the client's old cached catalogue.
func TestRawOnDemandDirectoryChangePreservesQueuedRequest(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call", "--backend-lifecycle", "on-demand", "--backend-idle-timeout", "150ms"}, "--lifecycle-directory")
	p.initialize("2025-11-25")
	first := p.snapshot("1", false)
	raw := p.call("2", "tools/list", `{}`)
	if !bytes.Contains(raw, []byte(`"name":"future_tool"`)) {
		t.Fatalf("initial catalogue: %s", raw)
	}
	awaitOnDemandExit(t, f, first.PID)
	writeInput(t, f.inputs[0], "B", true)
	raw = p.call("3", "tools/call", `{"name":"future_tool","arguments":null}`)
	if !bytes.Contains(raw, []byte(`"code":-32602`)) || !bytes.Contains(raw, []byte(`tool removed from catalogue`)) {
		t.Fatalf("queued request was rewritten or native rejection lost: %s", raw)
	}
	invalidated := false
	for _, notification := range p.notifications {
		if bytes.Contains(notification, []byte(`notifications/tools/list_changed`)) {
			invalidated = true
		}
	}
	if !invalidated {
		t.Fatal("wake omitted directory invalidation before queued response")
	}
	calls := perCallExecutions(t, f)
	if len(calls) != 1 || !bytes.Contains([]byte(calls[0].Raw), []byte(`"name":"future_tool"`)) {
		t.Fatalf("old request was replayed or changed: %+v", calls)
	}
	awaitOnDemandExit(t, f, calls[0].PID)
}

func TestRawOnDemandSideEffectCrashNeverReplays(t *testing.T) {
	f := newFixture(t)
	p := onDemandPeer(t, f)
	p.initialize("2025-11-25")
	raw := p.call("1", "tools/call", `{"name":"fixture_write_crash"}`)
	if !bytes.Contains(raw, []byte(`"sideEffects":"unknown"`)) || !bytes.Contains(raw, []byte(`"automaticReplay":false`)) {
		t.Fatalf("write/crash accounting: %s", raw)
	}
	calls := perCallExecutions(t, f)
	if len(calls) != 1 {
		t.Fatalf("write executed more than once: %+v", calls)
	}
	awaitOnDemandExit(t, f, calls[0].PID)
	requireOnDemandQuiet(t, f, len(onDemandBoots(t, f)), 200*time.Millisecond)
	after := perCallSnapshot(t, p, "2", false)
	awaitOnDemandExit(t, f, after.PID)
	sideEffects, err := os.ReadFile(f.evidence + ".side-effects")
	if err != nil || string(sideEffects) != "executed\n" {
		t.Fatalf("side effect was lost or replayed: %q (%v)", sideEffects, err)
	}
}

// M-L05: unlike rawPeer, this client deliberately does not drain stdout while
// the 1 MiB terminal response fills the real pipe. A result removed from the
// pending map is not yet delivered, and its backend must remain alive.
func TestRawOnDemandWaitsForFrontendWrite(t *testing.T) {
	f := newFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(proxyBinary(t), "--refresh-mode", "per-call", "--backend-lifecycle", "on-demand", "--backend-idle-timeout", "150ms", "--disable-watcher", "--poll-interval", "1h", "--startup-timeout", "2s", "--shutdown-timeout", "100ms", "--request-timeout", "5s", "--", exe, "-test.run=^TestRawHelperProcess$", "--")
	cmd.Dir, cmd.Env = f.workspace, f.env()
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(f.root, "blocked-output.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); log.Close() }()
	t.Cleanup(func() {
		in.Close()
		out.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
			t.Error("blocked-output proxy did not exit")
		}
	})
	reader := bufio.NewReader(out)
	read := func() []byte {
		t.Helper()
		frame := make(chan []byte, 1)
		failed := make(chan error, 1)
		go func() {
			b, e := reader.ReadBytes('\n')
			if e != nil {
				failed <- e
			} else {
				frame <- b
			}
		}()
		select {
		case b := <-frame:
			return b
		case e := <-failed:
			t.Fatalf("read frontend: %v", e)
		case <-time.After(5 * time.Second):
			t.Fatal("frontend frame timed out")
		}
		return nil
	}
	if _, err := io.WriteString(in, `{"jsonrpc":"2.0","id":100,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"output-test","version":"1"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if response := read(); !bytes.Contains(response, []byte(`"result"`)) {
		t.Fatalf("initialize: %s", response)
	}
	if _, err := io.WriteString(in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_large"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	f.waitExecution(t, "tools/call")
	calls := perCallExecutions(t, f)
	time.Sleep(350 * time.Millisecond)
	if len(calls) != 1 || !onDemandAlive(calls[0].PID) {
		t.Fatal("backend retired while frontend result was blocked")
	}
	response := read()
	var message struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(response, &message) != nil || len(message.Result.Content) != 1 || len(message.Result.Content[0].Text) != 1<<20 {
		t.Fatal("blocked terminal frame was truncated or replaced")
	}
	awaitOnDemandExit(t, f, calls[0].PID)
}
