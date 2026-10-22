package proxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fredgnr/gopls-mcp-proxy/internal/protocol"
)

func requireSuspendBlocker(t *testing.T, r *Recovery, reason string) {
	t.Helper()
	if !containsSuspendBlocker(r.SuspendBlockers(), reason) {
		t.Fatalf("blockers %v do not contain %q", r.SuspendBlockers(), reason)
	}
}

func requireSuspendAllowed(t *testing.T, r *Recovery) {
	t.Helper()
	if got := r.SuspendBlockers(); len(got) != 0 {
		t.Fatalf("unexpected suspend blockers: %v", got)
	}
}

func TestSuspendRequiresCompletedHandshake(t *testing.T) {
	r := &Recovery{}
	requireSuspendBlocker(t, r, "initialization_incomplete")
	if r.NeedsResident() {
		t.Fatal("pristine connection requires a resident process")
	}
	r.ObserveResponse(
		recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`),
		recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true}}}}`),
	)
	requireSuspendBlocker(t, r, "initialization_incomplete")
	if !r.NeedsResident() {
		t.Fatal("confirmed initialize awaiting initialized lost its obligation")
	}
	r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	requireSuspendAllowed(t, r)
	r.SetRevision("2099-01-01") // Negotiated legacy revision cannot be overwritten.
	requireSuspendAllowed(t, r)
	unknown := &Recovery{}
	unknown.SetRevision("2099-01-01")
	requireSuspendBlocker(t, unknown, "unsupported_protocol_version")
}

func TestSuspendDirectoryRequiresClosedCursor(t *testing.T) {
	for _, method := range []string{"tools/list", "resources/list", "resources/templates/list", "prompts/list"} {
		t.Run(method, func(t *testing.T) {
			r, _ := recoverySession(t, "2025-11-25", `{"tools":{"listChanged":true},"resources":{"listChanged":true},"prompts":{"listChanged":true}}`)
			field := directoryKinds[method].field
			r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"`+method+`"}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"result":{"`+field+`":[],"nextCursor":""}}`))
			requireSuspendBlocker(t, r, "incomplete_directory:"+method)
			// Empty string is a real opaque cursor, not end-of-pagination.
			r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"method":"`+method+`","params":{"cursor":""}}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"result":{"`+field+`":[]}}`))
			requireSuspendAllowed(t, r)
		})
	}
	for _, nextRequest := range []string{
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"cursor":"wrong"}}`,
	} {
		r, _ := recoverySession(t, "2025-11-25", `{}`)
		r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"result":{"tools":[],"nextCursor":"next"}}`))
		r.ObserveResponse(recoveryMessage(t, nextRequest), recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"result":{"tools":[]}}`))
		requireSuspendBlocker(t, r, "incomplete_directory:tools/list")
	}
}

func TestSuspendResourceSubscriptionUntilConfirmedUnsubscribe(t *testing.T) {
	r, _ := recoverySession(t, "2025-11-25", `{"resources":{"subscribe":true}}`)
	subscribe := recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"resources/subscribe","params":{"uri":"file:///source.go"}}`)
	r.ObserveResponse(subscribe, recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"result":{}}`))
	requireSuspendBlocker(t, r, "resource_subscription")
	unsubscribe := recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"method":"resources/unsubscribe","params":{"uri":"file:///source.go"}}`)
	r.ObserveRequest(unsubscribe)
	requireSuspendBlocker(t, r, "resource_subscription")
	r.ObserveResponse(unsubscribe, recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"error":{"code":-1,"message":"no"}}`))
	requireSuspendBlocker(t, r, "resource_subscription")
	r.ObserveResponse(unsubscribe, recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"result":{}}`))
	requireSuspendAllowed(t, r)
}

func TestSuspendTaskObservationDoesNotChangeForcedRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, request, response, backend, capabilities string
	}{
		{name: "server capability", capabilities: `{"tasks":{}}`},
		{name: "client capability", request: `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"capabilities":{"tasks":{}}}}`},
		{name: "modern client capability", request: `{"jsonrpc":"2.0","id":2,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"tasks":{}}}}}`},
		{name: "task augmented call", request: `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"opaque","task":{"ttl":100}}}`},
		{name: "task method", request: `{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"taskId":"TOP_SECRET"}}`},
		{name: "task result", response: `{"jsonrpc":"2.0","id":2,"result":{"task":{"taskId":"TOP_SECRET","status":"working"}}}`},
		{name: "related task result", response: `{"jsonrpc":"2.0","id":2,"result":{"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"TOP_SECRET"}}}}`},
		{name: "task notification", backend: `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"TOP_SECRET","status":"completed"}}`},
		{name: "reverse task", backend: `{"jsonrpc":"2.0","id":3,"method":"sampling/createMessage","params":{"task":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := tc.capabilities
			if caps == "" {
				caps = `{}`
			}
			r, _ := recoverySession(t, "2025-11-25", caps)
			request := recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"opaque"}}`)
			if tc.request != "" {
				request = recoveryMessage(t, tc.request)
				r.ObserveRequest(request)
			}
			if tc.response != "" {
				r.ObserveResponse(request, recoveryMessage(t, tc.response))
			}
			if tc.backend != "" {
				r.ObserveBackend(recoveryMessage(t, tc.backend))
			}
			requireSuspendBlocker(t, r, "task_state")
			if strings.Contains(strings.Join(r.SuspendBlockers(), ","), "TOP_SECRET") {
				t.Fatal("diagnostic leaked task state")
			}
			if err := r.Preflight(); err != nil {
				t.Fatalf("voluntary blocker changed forced refresh: %v", err)
			}
			clone := r.Clone()
			if !reflect.DeepEqual(clone.SuspendBlockers(), r.SuspendBlockers()) {
				t.Fatal("clone lost blockers")
			}
		})
	}
}

func TestSuspendModernContinuationAndUnknownResult(t *testing.T) {
	for _, tc := range []struct{ result, reason string }{
		{`{"resultType":"input_required"}`, "continuation_state"},
		{`{"resultType":"complete","requestState":{"opaque":"TOP_SECRET"}}`, "continuation_state"},
		{`{"resultType":"future"}`, "unknown_result_type"},
		{`{"resultType":null}`, "unknown_result_type"},
	} {
		r := &Recovery{}
		r.SetRevision(protocol.ModernVersion)
		r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"result":`+tc.result+`}`))
		requireSuspendBlocker(t, r, tc.reason)
		if err := r.Preflight(); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"requestState", "inputResponses"} {
		r := &Recovery{}
		r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"`+field+`":{}}}`))
		requireSuspendBlocker(t, r, "continuation_state")
	}
}

func TestSuspendOpaqueBusinessPayloadAndKnownNotifications(t *testing.T) {
	r, _ := recoverySession(t, "2025-11-25", `{"tools":{"listChanged":true},"resources":{"listChanged":true}}`)
	request := recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"fixture/snapshot","params":{"arguments":{"task":{},"requestState":{},"capabilities":{"tasks":{}}}}}`)
	r.ObserveRequest(request)
	r.ObserveResponse(request, recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"input_required"}],"structuredContent":{"task":{},"requestState":{}}}}`))
	for _, method := range []string{"notifications/message", "notifications/tools/list_changed", "notifications/resources/list_changed", "notifications/prompts/list_changed", "notifications/resources/updated", "notifications/progress", protocol.CancellationMethod, protocol.SubscriptionAcknowledged} {
		r.ObserveBackend(recoveryMessage(t, `{"jsonrpc":"2.0","method":"`+method+`"}`))
	}
	r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`))
	requireSuspendAllowed(t, r)
	r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/vendor/TOP_SECRET"}`))
	requireSuspendBlocker(t, r, "unknown_client_notification")
	r.ObserveBackend(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/vendor/TOP_SECRET"}`))
	requireSuspendBlocker(t, r, "unknown_server_notification")
	if err := r.Preflight(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r.SuspendBlockers(), ","), "TOP_SECRET") {
		t.Fatal("unknown method leaked")
	}
}

func TestSuspendUnknownStateWithoutRevisionRequiresResidency(t *testing.T) {
	r := &Recovery{}
	r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/vendor/change"}`))
	if !r.NeedsResident() {
		t.Fatal("unknown state before initialization lost")
	}
	before := r.Clone()
	r.SuspendBlockers()
	if !reflect.DeepEqual(before, r.Clone()) {
		t.Fatal("SuspendBlockers mutated the ledger")
	}
}

func TestSuspendToolTaskDeclaration(t *testing.T) {
	for _, support := range []string{"required", "optional", "forbidden"} {
		r, _ := recoverySession(t, "2025-11-25", `{}`)
		r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"example","execution":{"taskSupport":"`+support+`"}}]}}`))
		if support == "forbidden" {
			requireSuspendAllowed(t, r)
		} else {
			requireSuspendBlocker(t, r, "task_state")
		}
	}
}

func TestSuspendUnknownAndUncertainRecoveryState(t *testing.T) {
	r, _ := recoverySession(t, "2025-11-25", `{"experimental":{"unknown":{}}}`)
	requireSuspendBlocker(t, r, "unrecoverable_protocol_state")
	r, _ = recoverySession(t, "2025-11-25", `{}`)
	r.MarkUncertain(recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"resources/subscribe","params":{"uri":"file:///source.go"}}`))
	requireSuspendBlocker(t, r, "unrecoverable_protocol_state")
}

func urlElicitationSession(t *testing.T) *Recovery {
	t.Helper()
	r := &Recovery{}
	r.ObserveResponse(
		recoveryMessage(t, `{"jsonrpc":"2.0","id":100,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{"form":{},"url":{}}}}}`),
		recoveryMessage(t, `{"jsonrpc":"2.0","id":100,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}}}}`),
	)
	r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	requireSuspendAllowed(t, r) // A static URL capability is not an active flow.
	return r
}

func TestSuspendURLElicitationIsStickyAndDoesNotChangeForcedRefresh(t *testing.T) {
	for _, tc := range []struct{ name, response, backend string }{
		{"required error", `{"jsonrpc":"2.0","id":2,"error":{"code":-32042,"message":"URL required","data":{"elicitations":[{"mode":"url","elicitationId":"TOP_SECRET","url":"https://example.invalid/TOP_SECRET","message":"Continue in browser"}]}}}`, ""},
		// The reserved code itself asserts an unresolved URL obligation. A
		// malformed/missing list cannot safely prove that there is no state.
		{"required error missing data", `{"jsonrpc":"2.0","id":2,"error":{"code":-32042,"message":"URL required"}}`, ""},
		{"reverse URL request", "", `{"jsonrpc":"2.0","id":3,"method":"elicitation/create","params":{"mode":"url","elicitationId":"TOP_SECRET","url":"https://example.invalid/TOP_SECRET","message":"Continue in browser"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := urlElicitationSession(t)
			if tc.response != "" {
				r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`), recoveryMessage(t, tc.response))
			} else {
				r.ObserveBackend(recoveryMessage(t, tc.backend))
			}
			requireSuspendBlocker(t, r, "url_elicitation_state")
			if !r.NeedsResident() || strings.Contains(strings.Join(r.SuspendBlockers(), ","), "TOP_SECRET") {
				t.Fatal("URL obligation lost or diagnostic leaked native state")
			}
			if err := r.Preflight(); err != nil {
				t.Fatalf("URL blocker changed forced refresh semantics: %v", err)
			}
			clone := r.Clone()
			requireSuspendBlocker(t, clone, "url_elicitation_state")
			clone.ObserveBackend(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/elicitation/complete","params":{"elicitationId":"TOP_SECRET"}}`))
			requireSuspendBlocker(t, clone, "url_elicitation_state")
		})
	}
}

func TestSuspendURLElicitationIgnoresFormAndOpaquePayloads(t *testing.T) {
	r := urlElicitationSession(t)
	r.ObserveBackend(recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"method":"elicitation/create","params":{"mode":"form","message":"Enter name","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}}`))
	request := recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"arguments":{"mode":"url","error":{"code":-32042}}}}`)
	r.ObserveRequest(request)
	for _, response := range []string{
		`{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"error":{"code":-32042,"data":{"elicitations":[]}},"mode":"url"}}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-1,"message":"business error","data":{"elicitations":[{"mode":"url"}]}}}`,
	} {
		r.ObserveResponse(request, recoveryMessage(t, response))
	}
	requireSuspendAllowed(t, r)
}
