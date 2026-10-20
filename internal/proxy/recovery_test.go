package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fredgnr/gopls-mcp-proxy/internal/protocol"
	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func recoveryMessage(t *testing.T, raw string) wire.Message {
	t.Helper()
	m, err := wire.ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func recoverySession(t *testing.T, revision, capabilities string) (*Recovery, []byte) {
	t.Helper()
	r := &Recovery{}
	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":100,"method":"initialize","params":{"protocolVersion":%q,"clientInfo":{"name":"actual client","version":"1"},"capabilities":{"roots":{"listChanged":true},"sampling":{}}}}`, revision)
	response := fmt.Sprintf(`{"jsonrpc":"2.0","id":100,"result":{"protocolVersion":%q,"serverInfo":{"name":"actual server","version":"1"},"capabilities":%s,"instructions":"original instructions"}}`, revision, capabilities)
	r.ObserveResponse(recoveryMessage(t, request), recoveryMessage(t, response))
	r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	return r, []byte(response)
}

func recoverySuccess(raw []byte) []byte {
	id, _ := wire.Get(raw, "id")
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, id))
}

func recoveryNoSend(context.Context, []byte) error { return nil }
func recoveryNoNotify([]byte) error                { return nil }

func TestRecoveryNegotiatedInitialization(t *testing.T) {
	for _, revision := range []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(revision, func(t *testing.T) {
			r, initial := recoverySession(t, revision, `{"tools":{"listChanged":true}}`)
			// The offered version may differ from the version accepted by the
			// client. Only that necessary field and private ID may change.
			r.initRequest = bytes.Replace(r.initRequest, []byte(revision), []byte("2099-01-01"), 1)
			var calls []string
			err := r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
				m := recoveryMessage(t, string(raw))
				calls = append(calls, m.Method)
				if got, _ := recoveryString(raw, "params", "protocolVersion"); got != revision {
					t.Fatalf("offered %q, want negotiated %q", got, revision)
				}
				if got, _ := recoveryString(raw, "params", "clientInfo", "name"); got != "actual client" {
					t.Fatalf("client identity changed: %q", got)
				}
				if got, _ := wire.Get(raw, "params", "capabilities"); string(got) != `{"roots":{"listChanged":true},"sampling":{}}` {
					t.Fatalf("client capabilities changed: %s", got)
				}
				if m.ID.Key == "n:100" {
					t.Fatal("hidden initialization reused the client's ID")
				}
				return initial, nil
			}, func(_ context.Context, raw []byte) error {
				calls = append(calls, recoveryMessage(t, string(raw)).Method)
				return nil
			}, func([]byte) error { t.Fatal("hidden initialization leaked to frontend"); return nil })
			if err != nil || !reflect.DeepEqual(calls, []string{"initialize", "notifications/initialized"}) {
				t.Fatalf("calls %v, error %v", calls, err)
			}
		})
	}
}

func TestRecoveryRejectsChangedSessionContract(t *testing.T) {
	for _, change := range []struct{ old, new string }{
		{`"original instructions"`, `"new instructions"`},
		{`"version":"1"`, `"version":"2"`},
		{`"listChanged":true`, `"listChanged":false`},
		{`"2025-11-25"`, `"2025-06-18"`},
	} {
		t.Run(change.old, func(t *testing.T) {
			r, initial := recoverySession(t, "2025-11-25", `{"tools":{"listChanged":true}}`)
			err := r.Recover(context.Background(), func(context.Context, []byte) ([]byte, error) {
				return bytes.Replace(initial, []byte(change.old), []byte(change.new), 1), nil
			}, func(context.Context, []byte) error { t.Fatal("initialized sent after incompatible result"); return nil }, recoveryNoNotify)
			if !errors.Is(err, ErrReconnectRequired) {
				t.Fatalf("want reconnect, got %v", err)
			}
		})
	}
}

func TestRecoveryUnknownInitializationExtensionsAreNotReplayed(t *testing.T) {
	for _, params := range []string{
		`{"protocolVersion":"2025-11-25","clientInfo":{"name":"x","version":"1"},"capabilities":{},"oneTimeToken":"opaque"}`,
		`{"protocolVersion":"2025-11-25","clientInfo":{"name":"x","version":"1"},"capabilities":{"newState":{}}}`,
		`{"protocolVersion":"2025-11-25","clientInfo":{"name":"x","version":"1"},"capabilities":{"roots":{"futureState":true}}}`,
		`{"protocolVersion":"2025-11-25","clientInfo":{"name":"x","version":"1"},"capabilities":{},"_meta":{"nonce":"once"}}`,
		`{"protocolVersion":"2025-11-25","clientInfo":{"name":"x","version":"1","vendorSessionState":"once"},"capabilities":{}}`,
	} {
		r, initial := recoverySession(t, "2025-11-25", `{}`)
		r = &Recovery{}
		r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+params+`}`), recoveryMessage(t, string(initial)))
		r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
		err := r.Recover(context.Background(), func(context.Context, []byte) ([]byte, error) {
			t.Fatal("unknown one-time state replayed")
			return nil, nil
		}, recoveryNoSend, recoveryNoNotify)
		if !errors.Is(err, ErrReconnectRequired) {
			t.Fatalf("params %s: got %v", params, err)
		}
	}
}

func TestRecoveryConfirmedSettingsAndSubscriptions(t *testing.T) {
	r, initial := recoverySession(t, "2025-11-25", `{"logging":{},"resources":{"subscribe":true}}`)
	observe := func(request, response string) {
		r.ObserveResponse(recoveryMessage(t, request), recoveryMessage(t, response))
	}
	ok := `{"jsonrpc":"2.0","id":1,"result":{}}`
	observe(`{"jsonrpc":"2.0","id":1,"method":"logging/setLevel","params":{"level":"debug"}}`, ok)
	observe(`{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"error"}}`, `{"jsonrpc":"2.0","id":2,"error":{"code":1001,"message":"no"}}`)
	observe(`{"jsonrpc":"2.0","id":3,"method":"logging/setLevel","params":{"level":"info"}}`, ok)
	observe(`{"jsonrpc":"2.0","id":4,"method":"resources/subscribe","params":{"uri":"file:///keep"}}`, ok)
	observe(`{"jsonrpc":"2.0","id":5,"method":"resources/subscribe","params":{"uri":"file:///cancel"}}`, ok)
	observe(`{"jsonrpc":"2.0","id":6,"method":"resources/unsubscribe","params":{"uri":"file:///cancel"}}`, ok)
	// Explicit upstream failures above do not replace the last confirmed level.
	// Possibly-dispatched, uncertain outcomes are covered separately below.
	var methods, notifications []string
	err := r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
		m := recoveryMessage(t, string(raw))
		methods = append(methods, m.Method)
		if m.Method == "initialize" {
			return initial, nil
		}
		if m.Method == "logging/setLevel" {
			if level, _ := recoveryString(raw, "params", "level"); level != "info" {
				t.Fatalf("recovered unconfirmed level %q", level)
			}
		}
		if m.Method == "resources/subscribe" {
			if uri, _ := recoveryString(raw, "params", "uri"); uri != "file:///keep" {
				t.Fatalf("revived cancelled subscription %q", uri)
			}
		}
		return recoverySuccess(raw), nil
	}, recoveryNoSend, func(raw []byte) error { notifications = append(notifications, string(raw)); return nil })
	if err != nil || !reflect.DeepEqual(methods, []string{"initialize", "logging/setLevel", "resources/subscribe"}) {
		t.Fatalf("methods %v error %v", methods, err)
	}
	if len(notifications) != 1 || !strings.Contains(notifications[0], "notifications/resources/updated") || !strings.Contains(notifications[0], "file:///keep") {
		t.Fatalf("resource invalidation missing: %v", notifications)
	}
}

func TestRecoveryDirectoryUsesDeliveredPagesAndNewCursors(t *testing.T) {
	r, initial := recoverySession(t, "2025-11-25", `{"tools":{}}`)
	firstRequest := `{"jsonrpc":"2.0","id":11,"method":"tools/list"}`
	firstResponse := `{"jsonrpc":"2.0","id":11,"result":{"tools":[{"name":"first","future":{"n":9007199254740993}}],"nextCursor":"OLD","extension":null},"unknownEnvelope":"kept"}`
	secondRequest := `{"jsonrpc":"2.0","id":12,"method":"tools/list","params":{"cursor":"OLD"}}`
	secondResponse := `{"jsonrpc":"2.0","id":12,"result":{"tools":[{"name":"second","inputSchema":{"anyOf":[{},null]}}],"extension":null},"unknownEnvelope":"kept"}`
	r.ObserveResponse(recoveryMessage(t, firstRequest), recoveryMessage(t, firstResponse))
	r.ObserveResponse(recoveryMessage(t, secondRequest), recoveryMessage(t, secondResponse))
	var enumerated int
	err := r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
		m := recoveryMessage(t, string(raw))
		if m.Method == "initialize" {
			return initial, nil
		}
		enumerated++
		if enumerated == 1 {
			if _, exists := wire.Get(raw, "params", "cursor"); exists {
				t.Fatal("first page replayed an old cursor")
			}
			return []byte(strings.Replace(firstResponse, `"OLD"`, `"NEW"`, 1)), nil
		}
		if cursor, _ := recoveryString(raw, "params", "cursor"); cursor != "NEW" {
			t.Fatalf("replayed old pagination cursor %q", cursor)
		}
		return []byte(secondResponse), nil
	}, recoveryNoSend, recoveryNoNotify)
	if err != nil || enumerated != 2 {
		t.Fatalf("pages=%d error=%v", enumerated, err)
	}
	// A change that old tool-name digests would miss must invalidate recovery.
	err = r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
		if recoveryMessage(t, string(raw)).Method == "initialize" {
			return initial, nil
		}
		return []byte(strings.Replace(firstResponse, "9007199254740993", "9007199254740992", 1)), nil
	}, recoveryNoSend, recoveryNoNotify)
	if !errors.Is(err, ErrReconnectRequired) {
		t.Fatalf("unknown large-number field change accepted: %v", err)
	}
}

func TestRecoveryDirectoryIncompleteAndConflictingViewsReconnect(t *testing.T) {
	for _, scenario := range []string{"partial", "cursorless-continuation", "duplicate-cursor", "null-cursor", "extension-changed"} {
		t.Run(scenario, func(t *testing.T) {
			r, initial := recoverySession(t, "2025-11-25", `{"tools":{}}`)
			request := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
			response := `{"jsonrpc":"2.0","id":1,"result":{"tools":[],"nextCursor":"A"}}`
			r.ObserveResponse(recoveryMessage(t, request), recoveryMessage(t, response))
			if scenario != "partial" {
				params := `{"cursor":"A"}`
				result := `{"tools":[]}`
				switch scenario {
				case "cursorless-continuation":
					params = `{"cursor":"unknown"}`
				case "duplicate-cursor":
					result = `{"tools":[],"nextCursor":"A"}`
				case "null-cursor":
					params = `{"cursor":null}`
				case "extension-changed":
					params = `{"cursor":"A","filter":"new"}`
				}
				r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":`+params+`}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":2,"result":`+result+`}`))
			}
			err := r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
				if recoveryMessage(t, string(raw)).Method != "initialize" {
					t.Fatal("attempted to manufacture a historical directory snapshot")
				}
				return initial, nil
			}, recoveryNoSend, recoveryNoNotify)
			if !errors.Is(err, ErrReconnectRequired) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRecoveryAdvertisedDirectoryNotification(t *testing.T) {
	r, initial := recoverySession(t, "2025-11-25", `{"tools":{"listChanged":true}}`)
	r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"result":{"tools":[],"nextCursor":"partial"}}`))
	var notifications []string
	err := r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
		if recoveryMessage(t, string(raw)).Method != "initialize" {
			t.Fatal("notification-capable directory was enumerated as startup prerequisite")
		}
		return initial, nil
	}, recoveryNoSend, func(raw []byte) error {
		notifications = append(notifications, recoveryMessage(t, string(raw)).Method)
		return nil
	})
	if err != nil || !reflect.DeepEqual(notifications, []string{"notifications/tools/list_changed"}) {
		t.Fatalf("notifications %v error %v", notifications, err)
	}
}

func TestRecoveryModernNoHandshakeAndUnknownRevision(t *testing.T) {
	r := &Recovery{}
	r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"unknown/business","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"unrecognized":"preserved"}}}`))
	unexpected := func(context.Context, []byte) ([]byte, error) {
		t.Fatal("modern startup synthesized a request")
		return nil, nil
	}
	if err := r.Recover(context.Background(), unexpected, recoveryNoSend, recoveryNoNotify); err != nil {
		t.Fatal(err)
	}
	r.SetRevision("2099-01-01")
	if err := r.Recover(context.Background(), unexpected, recoveryNoSend, recoveryNoNotify); !errors.Is(err, ErrReconnectRequired) {
		t.Fatalf("unknown profile recovery accepted: %v", err)
	}
}

func TestRecoveryModernDirectoryMetadataIsNotAClientDefault(t *testing.T) {
	r := &Recovery{}
	request := recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"opaque":"page context"}}}`)
	response := recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"result":{"tools":[],"unknown":false}}`)
	r.ObserveRequest(request)
	r.ObserveResponse(request, response)
	if err := r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
		m := recoveryMessage(t, string(raw))
		if m.Method != "tools/list" || protocol.Version(m) != protocol.ModernVersion {
			t.Fatalf("unexpected modern request: %s", raw)
		}
		meta, _ := wire.Get(raw, "params", "_meta")
		original, _ := wire.Get(request.Raw, "params", "_meta")
		if !bytes.Equal(meta, original) {
			t.Fatalf("metadata changed: %s", raw)
		}
		return response.Raw, nil
	}, recoveryNoSend, func([]byte) error { t.Fatal("notified modern client without a subscription"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryCloneAndCancellation(t *testing.T) {
	r, _ := recoverySession(t, "2025-11-25", `{"tools":{}}`)
	r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	clone := r.Clone()
	r.directories["tools/list"].pages[0][0] = 'X'
	r.initRequest[0] = 'X'
	if !json.Valid(clone.directories["tools/list"].pages[0]) || !json.Valid(clone.initRequest) {
		t.Fatal("recovery clone shares mutable snapshots with the event loop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clone.Recover(ctx, nil, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation did not stop recovery: %v", err)
	}
}

func TestRecoveryCapabilityShapesAreRevisionSpecificAndRecursive(t *testing.T) {
	for _, test := range []struct {
		name, revision, client, server string
		accepted                       bool
	}{
		{"initial-basic", "2024-11-05", `{"roots":{"listChanged":true},"sampling":{}}`, `{"logging":{},"tools":{"listChanged":true},"resources":{"subscribe":true,"listChanged":false},"prompts":{}}`, true},
		{"initial-unknown-completion", "2024-11-05", `{}`, `{"completions":{}}`, false},
		{"march-completion", "2025-03-26", `{"sampling":{}}`, `{"completions":{}}`, true},
		{"march-unknown-elicitation", "2025-03-26", `{"elicitation":{}}`, `{}`, false},
		{"june-elicitation", "2025-06-18", `{"elicitation":{}}`, `{"completions":{}}`, true},
		{"june-form", "2025-06-18", `{"elicitation":{"form":{}}}`, `{}`, true},
		{"june-url", "2025-06-18", `{"elicitation":{"url":{}}}`, `{}`, true},
		{"june-form-url", "2025-06-18", `{"elicitation":{"form":{},"url":{}}}`, `{}`, true},
		{"june-unknown-mode", "2025-06-18", `{"elicitation":{"unknownMode":{}}}`, `{}`, false},
		{"june-nonempty-form", "2025-06-18", `{"elicitation":{"form":{"unknown":true}}}`, `{}`, false},
		{"june-nonempty-url", "2025-06-18", `{"elicitation":{"url":{"sessionToken":"opaque"}}}`, `{}`, false},
		{"june-null-form", "2025-06-18", `{"elicitation":{"form":null}}`, `{}`, false},
		{"june-boolean-url", "2025-06-18", `{"elicitation":{"url":true}}`, `{}`, false},
		{"june-array-url", "2025-06-18", `{"elicitation":{"url":[]}}`, `{}`, false},
		{"june-unknown-tasks", "2025-06-18", `{}`, `{"tasks":{}}`, false},
		{"november-full-known", "2025-11-25", `{"roots":{"listChanged":false},"sampling":{"context":{},"tools":{}},"elicitation":{"form":{},"url":{}},"tasks":{"list":{},"cancel":{},"requests":{"sampling":{"createMessage":{}},"elicitation":{"create":{}}}}}`, `{"logging":{},"completions":{},"tools":{"listChanged":true},"tasks":{"list":{},"cancel":{},"requests":{"tools":{"call":{}}}}}`, true},
		{"unknown-tool-session", "2025-11-25", `{}`, `{"tools":{"vendorSessionState":"once"}}`, false},
		{"unknown-root-handshake", "2025-11-25", `{"roots":{"vendorHandshake":"once"}}`, `{}`, false},
		{"deep-sampling-state", "2025-11-25", `{"sampling":{"tools":{"vendorState":"once"}}}`, `{}`, false},
		{"deep-task-state", "2025-11-25", `{}`, `{"tasks":{"requests":{"tools":{"call":{"vendorState":"once"}}}}}`, false},
		{"unknown-task-request", "2025-11-25", `{"tasks":{"requests":{"vendorRequest":{}}}}`, `{}`, false},
		{"mistyped-list-change", "2025-11-25", `{}`, `{"tools":{"listChanged":{"vendorState":true}}}`, false},
		{"null-capability", "2025-11-25", `{"sampling":null}`, `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"clientInfo":{"name":"client","version":"1"},"capabilities":%s}}`, test.revision, test.client))
			response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q,"serverInfo":{"name":"server","version":"1"},"capabilities":%s}}`, test.revision, test.server))
			originalRequest, originalResponse := bytes.Clone(request), bytes.Clone(response)
			if got := recoveryInitializeDiagnostic(request, response).Reason == ""; got != test.accepted {
				t.Fatalf("replayable=%v, want %v", got, test.accepted)
			}
			if !bytes.Equal(request, originalRequest) || !bytes.Equal(response, originalResponse) {
				t.Fatal("recovery classification mutated the initial raw exchange")
			}
		})
	}
}

func TestRecoveryInitializedExtensionsReconnect(t *testing.T) {
	for _, notification := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized","oneTimeState":"opaque"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{"oneTimeState":"opaque"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_meta":{"oneTimeState":"opaque"}}}`,
	} {
		r, _ := recoverySession(t, "2025-11-25", `{}`)
		r.ObserveRequest(recoveryMessage(t, notification))
		if !bytes.Equal(r.initialized, []byte(notification)) {
			t.Fatal("initial notification extension was normalized")
		}
		if err := r.Recover(context.Background(), nil, nil, nil); !errors.Is(err, ErrReconnectRequired) {
			t.Fatalf("unknown initialized extension replay allowed: %v", err)
		}
	}
}

func TestRecoveryUnconfirmedProtocolStateRequiresReconnect(t *testing.T) {
	for _, method := range []string{"initialize", "notifications/initialized", "logging/setLevel", "resources/subscribe", "resources/unsubscribe"} {
		t.Run(method, func(t *testing.T) {
			r, _ := recoverySession(t, "2025-11-25", `{"logging":{},"resources":{"subscribe":true}}`)
			r.ObserveResponse(recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"method":"logging/setLevel","params":{"level":"debug"}}`), recoveryMessage(t, `{"jsonrpc":"2.0","id":1,"result":{}}`))
			r.MarkUncertain(recoveryMessage(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":%q,"params":{}}`, method)))
			if err := r.Recover(context.Background(), nil, nil, nil); !errors.Is(err, ErrReconnectRequired) {
				t.Fatalf("uncertain %s restored older state: %v", method, err)
			}
		})
	}
	r, initial := recoverySession(t, "2025-11-25", `{}`)
	r.MarkUncertain(recoveryMessage(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"logging/setLevel","arguments":{"level":"debug"}}}`))
	if err := r.Recover(context.Background(), func(context.Context, []byte) ([]byte, error) { return initial, nil }, recoveryNoSend, recoveryNoNotify); err != nil {
		t.Fatalf("business content was interpreted as protocol state: %v", err)
	}
}
