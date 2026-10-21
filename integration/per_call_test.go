package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/testbackend"
)

func perCallSnapshot(t *testing.T, p *rawPeer, id string, modern bool) testbackend.Snapshot {
	t.Helper()
	params := `{"name":"future_tool","arguments":null}`
	if modern {
		params = modernParams(`"name":"future_tool","arguments":null`)
	}
	raw := p.call(id, "tools/call", params)
	var reply struct {
		Result struct {
			Snapshot testbackend.Snapshot `json:"structuredContent"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || len(reply.Error) != 0 || reply.Result.Snapshot.Boot == "" {
		t.Fatalf("tool snapshot failed: %s (%v)", raw, err)
	}
	return reply.Result.Snapshot
}

func perCallExecutions(t *testing.T, f *fixture) []testbackend.Event {
	t.Helper()
	var result []testbackend.Event
	for _, event := range f.events(t) {
		if event.Kind == "execution" && event.Method == "tools/call" {
			result = append(result, event)
		}
	}
	return result
}

func requirePerCallBoot(t *testing.T, before, after testbackend.Snapshot) {
	t.Helper()
	if before.Boot == after.Boot || before.PID == after.PID {
		t.Fatalf("tools/call reused backend: before=%s/%d after=%s/%d", before.Boot, before.PID, after.Boot, after.PID)
	}
}

// Exercise the installed default launcher as well as the unchanged non-tool
// path. Every tool call must get a fresh process even without an input change.
func TestRawPerCallDefaultLauncherFreshBackends(t *testing.T) {
	l := newDefaultLayout(t, false)
	p := l.start(t, "--refresh-mode", "per-call", "--disable-watcher", "--poll-interval", "1h")
	p.initialize("2025-11-25")
	initial := p.snapshot("1", false)
	p.call("2", "tools/list", `{}`)
	if got := p.snapshot("3", false); got.Boot != initial.Boot {
		t.Fatal("non-tool requests caused a per-call restart")
	}
	first := perCallSnapshot(t, p, "4", false)
	requirePerCallBoot(t, initial, first)
	second := perCallSnapshot(t, p, "5", false)
	requirePerCallBoot(t, first, second)
	if got := p.snapshot("6", false); got.Boot != second.Boot {
		t.Fatal("non-tool request did not retain the current backend")
	}
	var firstInit, secondInit struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(first.Initialize, &firstInit) != nil || json.Unmarshal(second.Initialize, &secondInit) != nil || firstInit.Method != "initialize" || secondInit.Method != "initialize" || !bytes.Contains(firstInit.Params, []byte(`"actual-client"`)) || !bytes.Equal(firstInit.Params, secondInit.Params) {
		t.Fatalf("initialization not restored consistently: first=%s second=%s", first.Initialize, second.Initialize)
	}
	if calls := perCallExecutions(t, l.f); len(calls) != 2 || calls[0].Boot == calls[1].Boot {
		t.Fatalf("independent tool execution evidence: %+v", calls)
	}
}

func TestRawPerCallOptInOnly(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.initialize("2025-11-25")
	first := perCallSnapshot(t, p, "1", false)
	second := perCallSnapshot(t, p, "2", false)
	if first.Boot != second.Boot || first.PID != second.PID {
		t.Fatal("default on-change mode unexpectedly restarted unchanged tools/call")
	}
}

// The fixture reads files only at boot, so both modes must prove replacement
// independently of gopls's own file watching. Replayed initialization must retain
// the original client identity, elicitation declarations and auth-change marker.
func TestRawCodexRecoveryAcrossRefreshModes(t *testing.T) {
	testRawInitializationRecoveryAcrossRefreshModes(t, codexInitializeParams)
}

func TestRawEmptyExtensionsRecoveryAcrossRefreshModes(t *testing.T) {
	if omittedExtensionsInitializeParams == emptyExtensionsInitializeParams {
		t.Fatal("report control did not omit extensions; update the fixture transformation")
	}
	for _, tc := range []struct{ name, params string }{
		{"omitted", omittedExtensionsInitializeParams},
		{"empty", emptyExtensionsInitializeParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRawInitializationRecoveryAcrossRefreshModes(t, tc.params)
		})
	}
}

func testRawInitializationRecoveryAcrossRefreshModes(t *testing.T, initializeParams string) {
	t.Helper()
	for _, mode := range []string{"per-call", "on-change"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			p := f.proxyFlags(t, []string{"--refresh-mode", mode})
			initialized := p.call("100", "initialize", initializeParams)
			if bytes.Contains(initialized, []byte(`"error"`)) || !bytes.Contains(initialized, []byte(`"protocolVersion":"2025-06-18"`)) {
				t.Fatalf("fixture initialize: %s", initialized)
			}
			p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			initial := p.snapshot("0", false)
			catalogue := p.call(`"catalogue"`, "tools/list", `{}`)
			first := perCallSnapshot(t, p, "1", false)
			second := perCallSnapshot(t, p, "2", false)
			if mode == "per-call" {
				requirePerCallBoot(t, initial, first)
				requirePerCallBoot(t, first, second)
				requireBackendExited(t, initial.PID)
				requireBackendExited(t, first.PID)
			} else if initial.Boot != first.Boot || first.Boot != second.Boot {
				t.Fatal("unchanged on-change calls replaced the backend")
			}
			writeInput(t, f.inputs[0], "B", true)
			third := perCallSnapshot(t, p, "3", false)
			requirePerCallBoot(t, second, third)
			requireBackendExited(t, second.PID)
			requireInput(t, third, "main.go", "B")
			for _, snapshot := range []testbackend.Snapshot{initial, first, second, third} {
				var request struct {
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal(snapshot.Initialize, &request); err != nil {
					t.Fatal(err)
				}
				if request.Method != "initialize" || !bytes.Equal(request.Params, []byte(initializeParams)) {
					t.Fatalf("initialization changed across refresh: %s", snapshot.Initialize)
				}
			}
			if after := p.call(`"catalogue"`, "tools/list", `{}`); !rawJSONEqual(catalogue, after) {
				t.Fatalf("catalogue changed across recovery: before=%s after=%s", catalogue, after)
			}
			if executions := perCallExecutions(t, f); len(executions) != 3 {
				t.Fatalf("expected exactly three dispatched calls without replay: %+v", executions)
			}
			p.shutdown()
			requireRecoverableInitialization(t, p.log)
			for _, event := range f.events(t) {
				if event.Kind == "boot" {
					requireBackendExited(t, event.PID)
				}
			}
		})
	}
}

// Only Codex's empty client marker has a recovery rule. A future stateful
// declaration must not be silently dropped or accepted by the narrow exception.
func TestRawCodexAuthChangeRejectsStatefulDeclaration(t *testing.T) {
	testRawRejectedInitialization(t,
		`{"experimental":{"codex/auth-change":{"sessionToken":"RECOVERY_TOKEN_MUST_NOT_LEAK"}}}`,
		`params.capabilities.experimental["codex/auth-change"].sessionToken`,
		"nonempty_static_declaration", "string")
}

func TestRawExtensionsRejectsUnsupportedDeclaration(t *testing.T) {
	for _, tc := range []struct{ name, capabilities, path, reason, actualType string }{
		{"unknown_marker", `{"extensions":{"future/extension":{}}}`, `params.capabilities.extensions["future/extension"]`, "nonempty_static_declaration", "object"},
		{"stateful", `{"extensions":{"future/extension":{"token":"RECOVERY_TOKEN_MUST_NOT_LEAK"}}}`, `params.capabilities.extensions["future/extension"]`, "nonempty_static_declaration", "object"},
		{"null", `{"extensions":null}`, "params.capabilities.extensions", "invalid_type", "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRawRejectedInitialization(t, tc.capabilities, tc.path, tc.reason, tc.actualType)
		})
	}
}

func testRawRejectedInitialization(t *testing.T, capabilities, path, reason, actualType string) {
	t.Helper()
	for _, mode := range []string{"per-call", "on-change"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			p := f.proxyFlags(t, []string{"--refresh-mode", mode})
			const secret = "RECOVERY_TOKEN_MUST_NOT_LEAK"
			params := `{"protocolVersion":"2025-06-18","clientInfo":{"name":"future-client","version":"1"},"capabilities":` + capabilities + `}`
			raw := p.call(`"initialize-stateful"`, "initialize", params)
			if mode == "on-change" {
				if bytes.Contains(raw, []byte(`"error"`)) {
					t.Fatalf("on-change initial handshake should preserve upstream result: %s", raw)
				}
				p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
				if snapshot := p.snapshot("1", false); !bytes.Contains(snapshot.Initialize, []byte(`"capabilities":`+capabilities)) {
					t.Fatalf("unsupported initialization was rewritten: %s", snapshot.Initialize)
				}
				writeInput(t, f.inputs[0], "B", true)
				raw = p.call(`"blocked-tool"`, "tools/call", `{"name":"future_tool","arguments":null}`)
			}
			var response struct {
				Result json.RawMessage `json:"result"`
				Error  struct {
					Message string `json:"message"`
					Data    struct {
						RequestDispatched       bool `json:"requestDispatched"`
						BackendResponseReceived bool `json:"backendResponseReceived"`
						AutomaticReplay         bool `json:"automaticReplay"`
						Recovery                struct {
							Path       string `json:"path"`
							Reason     string `json:"reason"`
							ActualType string `json:"actual_type"`
						} `json:"recovery"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			data := response.Error.Data
			if len(response.Result) != 0 || !bytes.Contains([]byte(response.Error.Message), []byte("protocol_restart_required")) ||
				data.Recovery.Path != path || data.Recovery.Reason != reason || data.Recovery.ActualType != actualType {
				t.Fatalf("missing precise initialization rejection: %s", raw)
			}
			dispatched := mode == "per-call" // Only the initialize reached the backend.
			if data.RequestDispatched != dispatched || data.BackendResponseReceived != dispatched || data.AutomaticReplay {
				t.Fatalf("incorrect dispatch accounting: %s", raw)
			}
			select {
			case extra, open := <-p.frames:
				if open {
					t.Fatalf("unexpected response after terminal initialization rejection: %s", extra)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("initialization rejection did not close the frontend within its cleanup budget")
			}
			p.shutdown()
			logs, err := os.ReadFile(p.log)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(secret)) || bytes.Contains(logs, []byte(secret)) {
				t.Fatal("initialization rejection leaked a capability value")
			}
			boots := 0
			for _, event := range f.events(t) {
				if event.Kind == "boot" {
					boots++
					requireBackendExited(t, event.PID)
				}
			}
			if boots != 1 || len(perCallExecutions(t, f)) != 0 {
				t.Fatalf("unsupported declaration started a replacement or dispatched a tool: boots=%d calls=%+v", boots, perCallExecutions(t, f))
			}
		})
	}
}

func TestRawPerCallModernFirstCall(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call"})
	first := perCallSnapshot(t, p, `"same-id"`, true)
	second := perCallSnapshot(t, p, `"same-id"`, true)
	requirePerCallBoot(t, first, second)
	boots := 0
	for _, event := range f.events(t) {
		if event.Kind == "boot" {
			boots++
		}
		if event.Method == "initialize" || event.Method == "server/discover" {
			t.Fatalf("proxy invented a modern lifecycle call: %+v", event)
		}
	}
	if boots != 2 {
		t.Fatalf("two first-use modern tool calls started %d backends", boots)
	}
}

func TestRawPerCallPreservesOpaqueRequestsAndNativeErrors(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call"})
	params := modernParams(`"name":"unknown_fixture_tool","arguments":{"opaque":null,"large":900719925474099312345,"array":[true,{"future":"unchanged"}]}`)
	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":900719925474099312345,"method":"tools/call","params":%s}`, params)
	for i := 0; i < 2; i++ {
		p.send(request)
		got := p.response("900719925474099312345")
		want := `{"jsonrpc":"2.0","id":900719925474099312345,"error":{"code":-32602,"message":"unknown fixture tool"}}`
		if string(got) != want {
			t.Fatalf("native tool error changed: got=%s want=%s", got, want)
		}
	}
	calls := perCallExecutions(t, f)
	if len(calls) != 2 || calls[0].Boot == calls[1].Boot {
		t.Fatalf("failed native calls did not get distinct instances: %+v", calls)
	}
	for _, call := range calls {
		if call.Raw != request {
			t.Fatalf("opaque tool request changed: got=%s want=%s", call.Raw, request)
		}
	}
}

// These files are deliberately outside monitoring or explicitly excluded.
// Therefore changed snapshots prove unconditional tool-call process freshness,
// not a filesystem event or content fingerprint causing the restart.
func TestRawPerCallReloadsUnwatchedAndExcludedFiles(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.root, "outside.go")
	ignored := filepath.Join(f.workspace, "ignored")
	if err := os.Mkdir(ignored, 0700); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(ignored, "new-package", "new.go")
	renamed := filepath.Join(ignored, "new-package", "renamed.go")
	if err := os.WriteFile(outside, []byte("A"), 0600); err != nil {
		t.Fatal(err)
	}
	f.inputs = append(f.inputs, outside, newFile, renamed)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call", "--exclude", "ignored"})
	p.initialize("2025-11-25")
	previous := perCallSnapshot(t, p, "1", false)
	steps := []struct {
		name   string
		change func() error
		check  func(testbackend.Snapshot)
	}{
		{"external equal-size equal-mtime edit", func() error { writeInput(t, outside, "B", true); return nil }, func(s testbackend.Snapshot) { requireInput(t, s, "outside.go", "B") }},
		{"new excluded directory and file", func() error {
			if err := os.MkdirAll(filepath.Dir(newFile), 0700); err != nil {
				return err
			}
			return os.WriteFile(newFile, []byte("N"), 0600)
		}, func(s testbackend.Snapshot) { requireInput(t, s, "new.go", "N") }},
		{"rename excluded file", func() error { return os.Rename(newFile, renamed) }, func(s testbackend.Snapshot) {
			if s.Inputs["new.go"] != nil {
				t.Fatal("renamed source remained present")
			}
			requireInput(t, s, "renamed.go", "N")
		}},
		{"delete excluded file", func() error { return os.Remove(renamed) }, func(s testbackend.Snapshot) {
			if s.Inputs["renamed.go"] != nil {
				t.Fatal("deleted input remained present")
			}
		}},
	}
	for i, step := range steps {
		if err := step.change(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		next := perCallSnapshot(t, p, fmt.Sprint(i+2), false)
		requirePerCallBoot(t, previous, next)
		step.check(next)
		previous = next
	}
	if got := len(perCallExecutions(t, f)); got != len(steps)+1 {
		t.Fatalf("tool executions=%d, want %d", got, len(steps)+1)
	}
}

// A held reverse request is an independent synchronization barrier. Waiting
// longer than drain-timeout detects an incorrect invalidate-on-arrival policy.
func TestRawPerCallSerializesAndPreservesControlRouting(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call"})
	p.initialize("2025-11-25")
	p.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_reverse","_meta":{"progressToken":"client-progress"}}}`)
	var reverseID json.RawMessage
	progressSeen := false
	for len(reverseID) == 0 {
		raw := p.next()
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatal(err)
		}
		switch msg.Method {
		case "notifications/progress":
			if !bytes.Contains(raw, []byte(`"progressToken":"client-progress"`)) {
				t.Fatalf("progress token was not restored: %s", raw)
			}
			progressSeen = true
		case "roots/list":
			reverseID = msg.ID
		default:
			t.Fatalf("unexpected active-call frame: %s", raw)
		}
	}
	if !progressSeen {
		t.Fatal("active tool progress was not forwarded")
	}
	first := perCallExecutions(t, f)
	if len(first) != 1 {
		t.Fatalf("first tool evidence: %+v", first)
	}
	p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"future_tool"}}`)
	p.send(`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`)
	time.Sleep(250 * time.Millisecond)
	if calls := perCallExecutions(t, f); len(calls) != 1 {
		t.Fatalf("queued tool executed while first still awaited its client: %+v", calls)
	}
	select {
	case raw, ok := <-p.frames:
		t.Fatalf("queued tool interrupted active tool: frame=%s open=%v", raw, ok)
	default:
	}
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"roots":[{"uri":"file:///actual-client-root","name":"actual client"}]}}`, reverseID))
	if raw := p.response("1"); !bytes.Contains(raw, []byte(`file:///actual-client-root`)) || bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("reverse response did not complete original tool: %s", raw)
	}
	var second struct {
		Result struct {
			Snapshot testbackend.Snapshot `json:"structuredContent"`
		} `json:"result"`
	}
	if raw := p.response("2"); json.Unmarshal(raw, &second) != nil || second.Result.Snapshot.Boot == "" {
		t.Fatalf("queued tool failed: %s", raw)
	}
	if second.Result.Snapshot.Boot == first[0].Boot {
		t.Fatal("queued tool reused active tool's backend")
	}
	controlSeen := false
	for _, event := range f.events(t) {
		if event.Kind == "receipt" && bytes.Contains([]byte(event.Raw), []byte(`"notifications/roots/list_changed"`)) {
			if event.Boot != first[0].Boot {
				t.Fatalf("control notification crossed backend ownership: %+v", event)
			}
			controlSeen = true
		}
	}
	if !controlSeen {
		t.Fatal("control notification was blocked behind a queued tool")
	}
}

func TestRawPerCallCancellationDoesNotExecuteQueuedTool(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call"})
	p.initialize("2025-11-25")
	p.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_hold"}}`)
	f.waitExecution(t, "tools/call")
	p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"future_tool","cancelledBeforeDispatch":true}}`)
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	perCallSnapshot(t, p, "3", false)
	calls := perCallExecutions(t, f)
	if len(calls) != 2 || string(calls[0].ID) != "1" || string(calls[1].ID) != "3" || calls[0].Boot == calls[1].Boot {
		t.Fatalf("cancellation executed a queued call or reused its predecessor: %+v", calls)
	}
}

func TestRawPerCallNeverReplaysFailedTool(t *testing.T) {
	for _, tool := range []string{"fixture_hold", "fixture_crash", "fixture_half"} {
		t.Run(tool, func(t *testing.T) {
			f := newFixture(t)
			p := f.proxyFlags(t, []string{"--refresh-mode", "per-call", "--request-timeout", "1s"})
			p.initialize("2025-11-25")
			raw := p.call("1", "tools/call", fmt.Sprintf(`{"name":%q}`, tool))
			for _, want := range []string{`"requestDispatched":true`, `"automaticReplay":false`, `"sideEffects":"unknown"`} {
				if !bytes.Contains(raw, []byte(want)) {
					t.Fatalf("failed tool omitted %s: %s", want, raw)
				}
			}
			perCallSnapshot(t, p, "2", false)
			calls := perCallExecutions(t, f)
			if len(calls) != 2 || string(calls[0].ID) != "1" || string(calls[1].ID) != "2" || calls[0].Boot == calls[1].Boot {
				t.Fatalf("failed tool replayed or next call reused its backend: %+v", calls)
			}
		})
	}
}

func TestRawPerCallRejectsToolBatchesWithoutExecution(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call"})
	p.initialize("2025-03-26")
	p.send(`[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"future_tool"}},{"jsonrpc":"2.0","id":"1","method":"fixture/echo","params":{"mustNotExecute":true}}]`)
	raw := p.next()
	var replies []struct {
		ID    json.RawMessage `json:"id"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &replies); err != nil || len(replies) != 2 {
		t.Fatalf("rejected batch lost grouping: %s (%v)", raw, err)
	}
	if string(replies[0].ID) != "1" || string(replies[1].ID) != `"1"` {
		t.Fatalf("rejected batch changed typed IDs: %s", raw)
	}
	for _, reply := range replies {
		if !bytes.Contains(reply.Error, []byte(`per_call_batch_unsupported`)) || !bytes.Contains(reply.Error, []byte(`"requestDispatched":false`)) {
			t.Fatalf("batch rejection diagnostics: %s", raw)
		}
	}
	for _, event := range f.events(t) {
		if event.Kind == "execution" && (event.Method == "tools/call" || event.Method == "fixture/echo") {
			t.Fatalf("rejected batch reached backend: %+v", event)
		}
	}
	perCallSnapshot(t, p, "2", false)
}

func TestRawPerCallRejectsMixedToolBatchBeforeForwarding(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--refresh-mode", "per-call"})
	p.initialize("2025-03-26")
	p.send(`[{"jsonrpc":"2.0","method":"fixture/notice","params":{"mustNotForward":true}},{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"future_tool"}}]`)
	select {
	case raw, ok := <-p.frames:
		if ok {
			t.Fatalf("mixed batch should close before forwarding: %s", raw)
		}
	case <-time.After(p.timeout):
		t.Fatal("unsupported mixed batch did not close the connection")
	}
	p.shutdown()
	for _, event := range f.events(t) {
		if bytes.Contains([]byte(event.Raw), []byte(`mustNotForward`)) || event.Method == "tools/call" {
			t.Fatalf("unsupported mixed batch partly reached backend: %+v", event)
		}
	}
}
