package protocol

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func message(t *testing.T, s string) wire.Message {
	t.Helper()
	m, err := wire.ParseMessage([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRevisionProfiles(t *testing.T) {
	for _, version := range []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", ModernVersion} {
		p, ok := Lookup(version)
		if !ok || p.Modern != (version == ModernVersion) || p.RequiresInitialize == (version == ModernVersion) || p.Batch != (version == "2025-03-26") {
			t.Fatalf("%+v %v", p, ok)
		}
		if CancelledResponseAllowed(p) {
			t.Fatal("cancelled request produces extra response")
		}
		batch, _ := wire.ParseFrame([]byte(`[{"jsonrpc":"2.0","method":"notice"}]`))
		if (ValidateFrame(p, batch) == nil) != p.Batch {
			t.Fatalf("batch policy %s", version)
		}
	}
	if _, ok := Lookup("2099-01-01"); ok {
		t.Fatal("unknown revision claimed")
	}
}

func TestModernPerRequestMetadataAndMRTR(t *testing.T) {
	first := message(t, `{"jsonrpc":"2.0","id":1,"method":"vendor/business","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"first","version":"1"},"unknown":null},"requestState":"opaque-signed-token"}}`)
	second := message(t, `{"jsonrpc":"2.0","id":2,"method":"vendor/business","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"sampling":{}},"io.modelcontextprotocol/clientInfo":{"name":"second","version":"2"}},"requestState":"different-token"}}`)
	for _, m := range []wire.Message{first, second} {
		if Version(m) != ModernVersion || ValidateModernMetadata(m) != nil {
			t.Fatal("metadata rejected")
		}
	}
	if Version(message(t, `{"jsonrpc":"2.0","id":3,"method":"vendor/business"}`)) != "" {
		t.Fatal("inferred metadata from previous request")
	}
	result := message(t, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"opaque-signed-token","requests":[{"method":"sampling/createMessage","params":{}}],"extension":null}}`)
	if !IsInputRequired(result) {
		t.Fatal("MRTR not recognized")
	}
	if String(result.Raw, "result", "requestState") != "opaque-signed-token" {
		t.Fatal("requestState changed")
	}
}

func TestSubscriptionLifecycleShapes(t *testing.T) {
	listen := message(t, `{"jsonrpc":"2.0","id":9007199254740993,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"notifications":{"toolsListChanged":true}}}`)
	ack := message(t, `{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":9007199254740993},"notifications":{"toolsListChanged":true}}}`)
	if !IsSubscription(listen) || !IsSubscriptionAck(ack) || ack.Kind != wire.Notification {
		t.Fatal("listen/ack classification")
	}
	id, ok := SubscriptionID(ack)
	if !ok || id.Key != listen.ID.Key {
		t.Fatal("subscription did not correlate to typed request ID")
	}
	completion, err := wire.ParseMessage(SubscriptionCompletion(listen.ID))
	if err != nil {
		t.Fatal(err)
	}
	closedID, ok := SubscriptionID(completion)
	if !ok || closedID.Key != listen.ID.Key || completion.ID.Key != listen.ID.Key || String(completion.Raw, "result", "resultType") != "complete" {
		t.Fatal("invalid completion")
	}
	cancel, err := wire.ParseMessage(Cancellation(listen.ID, "backend refreshed"))
	if err != nil {
		t.Fatal(err)
	}
	cancelID, ok := CancellationID(cancel)
	if !ok || cancelID.Key != listen.ID.Key {
		t.Fatal("invalid cancellation")
	}
}

func TestIndependentProgressAndCancellationAssociations(t *testing.T) {
	m := message(t, `{"jsonrpc":"2.0","id":1,"method":"unknown","params":{"_meta":{"progressToken":"separate-token"},"arguments":{"requestId":123,"progressToken":"business"}}}`)
	token, ok := ProgressToken(m)
	if !ok || token.Key == m.ID.Key || token.Key != "s:separate-token" {
		t.Fatal("progress token conflated with ID")
	}
	if _, ok := CancellationID(m); ok {
		t.Fatal("business request mistaken for cancellation")
	}
	n := message(t, `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"separate-token","progress":1,"unknown":null}}`)
	got, ok := ProgressToken(n)
	if !ok || got.Key != token.Key {
		t.Fatal("notification association")
	}
}

func TestProxyErrorEvidenceAndReservedCodes(t *testing.T) {
	if ProxyErrorCode >= -32768 && ProxyErrorCode <= -32000 {
		t.Fatal("proxy code is reserved")
	}
	for _, dispatched := range []bool{false, true} {
		raw := ErrorResponse(wire.StringID("request"), "workspace_changed", 17, dispatched, dispatched)
		m, err := wire.ParseMessage(raw)
		if err != nil || m.Kind != wire.Response {
			t.Fatalf("%s %v", raw, err)
		}
		dataRaw, _ := wire.Get(raw, "error", "data")
		var data ErrorData
		if err := json.Unmarshal(dataRaw, &data); err != nil {
			t.Fatal(err)
		}
		if data.Source != "gopls-mcp-proxy" || data.RequestDispatched != dispatched || data.BackendResponseReceived != dispatched || data.AutomaticReplay {
			t.Fatalf("%+v", data)
		}
		if dispatched && !bytes.Contains(raw, []byte("side effects are unknown")) {
			t.Fatal("message omits uncertainty")
		}
	}
}
