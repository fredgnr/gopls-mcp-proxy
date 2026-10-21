package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func TestRecoveryEmptyClientExtensionsPreservesReportedCapabilities(t *testing.T) {
	for _, versions := range []struct{ requested, negotiated string }{
		{"2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-06-18"},
		{"2025-11-25", "2025-11-25"},
	} {
		for _, extensions := range []struct{ name, field string }{
			{"omitted", ""},
			{"empty", `, "extensions" : { }`},
		} {
			t.Run(versions.requested+"/"+versions.negotiated+"/"+extensions.name, func(t *testing.T) {
				// These are the report's complete capabilities, including the
				// auth-change marker fixed in v0.5.0. Keep the omitted control.
				request := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"clientInfo":{"name":"gopls-readonly-verification","version":"1.0"},"capabilities":{"elicitation":{"form":{},"url":{}},"experimental":{"codex/auth-change":{}}%s}}}`, versions.requested, extensions.field))
				response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q,"serverInfo":{"name":"gopls","version":"v0.24.0"},"capabilities":{"tools":{}}}}`, versions.negotiated))
				initialized := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
				r := &Recovery{}
				r.ObserveResponse(recoveryMessage(t, string(request)), recoveryMessage(t, string(response)))
				if d, exists := r.InitializationDiagnostic(); !exists || d.Reason != "" {
					t.Fatalf("reported initialization is not recoverable: %+v, exists=%v", d, exists)
				}
				r.ObserveRequest(recoveryMessage(t, string(initialized)))
				expected, err := wire.Replace(request, mustRecoveryJSON(versions.negotiated), "params", "protocolVersion")
				if err != nil {
					t.Fatal(err)
				}
				expectedParams, _ := wire.Get(expected, "params")
				for generation := 2; generation <= 4; generation++ {
					// Replacements use a clone of the confirmed session ledger.
					candidate := r.Clone()
					exchanges, sends := 0, 0
					err := candidate.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
						exchanges++
						params, _ := wire.Get(raw, "params")
						if recoveryMessage(t, string(raw)).Method != "initialize" || !bytes.Equal(params, expectedParams) {
							t.Fatalf("generation %d changed initialization bytes: %s", generation, raw)
						}
						return response, nil
					}, func(_ context.Context, raw []byte) error {
						sends++
						if !bytes.Equal(raw, initialized) {
							t.Fatalf("generation %d changed initialized notification: %s", generation, raw)
						}
						return nil
					}, func([]byte) error {
						t.Fatal("initialization recovery leaked a frontend notification")
						return nil
					})
					if err != nil || exchanges != 1 || sends != 1 {
						t.Fatalf("generation %d: exchanges=%d sends=%d error=%v", generation, exchanges, sends, err)
					}
					if !bytes.Equal(r.initRequest, request) || !bytes.Equal(candidate.initRequest, request) {
						t.Fatal("recovery rewrote the saved original initialization")
					}
				}
			})
		}
	}
}

func TestRecoveryClientExtensionsRemainScopedAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, negotiated, side, value, path, reason, actualType string
	}{
		{"unknown-extension", "2025-06-18", "client", `{"vendor/state":{}}`, `params.capabilities.extensions["vendor/state"]`, "nonempty_static_declaration", "object"},
		{"experimental-marker-is-not-an-extension-rule", "2025-06-18", "client", `{"codex/auth-change":{}}`, `params.capabilities.extensions["codex/auth-change"]`, "nonempty_static_declaration", "object"},
		{"november-nonempty", "2025-11-25", "client", `{"vendor/state":{"token":"opaque"}}`, `params.capabilities.extensions["vendor/state"]`, "nonempty_static_declaration", "object"},
		{"null", "2025-06-18", "client", `null`, "params.capabilities.extensions", "invalid_type", "null"},
		{"array", "2025-06-18", "client", `[]`, "params.capabilities.extensions", "invalid_type", "array"},
		{"boolean", "2025-06-18", "client", `false`, "params.capabilities.extensions", "invalid_type", "boolean"},
		{"string", "2025-06-18", "client", `"opaque"`, "params.capabilities.extensions", "invalid_type", "string"},
		{"number", "2025-06-18", "client", `0`, "params.capabilities.extensions", "invalid_type", "number"},
		{"initial-profile", "2024-11-05", "client", `{}`, "params.capabilities.extensions", "no_recovery_rule", "object"},
		{"march-profile", "2025-03-26", "client", `{}`, "params.capabilities.extensions", "no_recovery_rule", "object"},
		{"june-server", "2025-06-18", "server", `{}`, "result.capabilities.extensions", "no_recovery_rule", "object"},
		{"november-server", "2025-11-25", "server", `{}`, "result.capabilities.extensions", "no_recovery_rule", "object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientCaps, serverCaps := `{}`, `{}`
			if tc.side == "client" {
				clientCaps = `{"extensions":` + tc.value + `}`
			} else {
				serverCaps = `{"extensions":` + tc.value + `}`
			}
			request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":%s}}`, clientCaps)
			response := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q,"capabilities":%s}}`, tc.negotiated, serverCaps)
			r := &Recovery{}
			r.ObserveResponse(recoveryMessage(t, request), recoveryMessage(t, response))
			r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
			d, exists := r.InitializationDiagnostic()
			if !exists || d.Side != tc.side || d.Path != tc.path || d.Reason != tc.reason || d.ActualType != tc.actualType || d.NegotiatedVersion != tc.negotiated {
				t.Fatalf("unexpected diagnostic: %+v, exists=%v", d, exists)
			}
			// Rejected declarations must never reach a replacement backend.
			if err := r.Recover(context.Background(), nil, nil, nil); !errors.Is(err, ErrReconnectRequired) {
				t.Fatalf("unsupported extension recovered: %v", err)
			}
		})
	}
}
