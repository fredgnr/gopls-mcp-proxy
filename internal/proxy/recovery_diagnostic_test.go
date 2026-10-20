package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func TestRecoveryCodexInitializationPreservesKnownCapabilities(t *testing.T) {
	for _, versions := range []struct{ requested, negotiated string }{
		{"2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-06-18"},
		{"2025-11-25", "2025-11-25"},
	} {
		for _, elicitation := range []string{`{}`, `{"form":{}}`, `{"url":{}}`, `{"form":{},"url":{}}`} {
			t.Run(versions.requested+"/"+versions.negotiated+"/"+elicitation, func(t *testing.T) {
				r := &Recovery{}
				request := fmt.Sprintf(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":%q,"clientInfo":{"name":"codex-mcp-client","title":"Codex","version":"0.163.0"},"capabilities":{"elicitation":%s}}}`, versions.requested, elicitation)
				response := fmt.Sprintf(`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":%q,"serverInfo":{"name":"gopls","version":"v0.23.0"},"capabilities":{"tools":{}}}}`, versions.negotiated)
				r.ObserveResponse(recoveryMessage(t, request), recoveryMessage(t, response))
				diagnostic, exists := r.InitializationDiagnostic()
				if !exists || diagnostic.Reason != "" || diagnostic.RequestedVersion != versions.requested || diagnostic.NegotiatedVersion != versions.negotiated {
					t.Fatalf("unexpected eligibility: %+v, exists=%v", diagnostic, exists)
				}
				if err := r.Preflight(); !errors.Is(err, ErrReconnectRequired) {
					t.Fatalf("incomplete handshake recovered: %v", err)
				}
				r.ObserveRequest(recoveryMessage(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
				if err := r.Preflight(); err != nil {
					t.Fatalf("known static capabilities blocked: %v", err)
				}
				expected, err := wire.Replace([]byte(request), mustRecoveryJSON(versions.negotiated), "params", "protocolVersion")
				if err != nil {
					t.Fatal(err)
				}
				expectedParams, _ := wire.Get(expected, "params")
				for i := 0; i < 2; i++ {
					calls := 0
					err := r.Recover(context.Background(), func(_ context.Context, raw []byte) ([]byte, error) {
						calls++
						params, _ := wire.Get(raw, "params")
						if recoveryMessage(t, string(raw)).Method != "initialize" || !bytes.Equal(params, expectedParams) {
							t.Fatalf("recovery changed the client identity or capabilities: %s", raw)
						}
						return []byte(response), nil
					}, recoveryNoSend, recoveryNoNotify)
					if err != nil || calls != 1 {
						t.Fatalf("recovery %d: calls=%d error=%v", i, calls, err)
					}
				}
				if !bytes.Equal(r.initRequest, []byte(request)) {
					t.Fatal("recovery rewrote the saved original initialize request")
				}
			})
		}
	}
}

func TestRecoveryInitializationDiagnosticPathsAndReasons(t *testing.T) {
	const request = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"client","version":"1"},"capabilities":{}}}`
	const response = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"server","version":"1"},"capabilities":{}}}`
	for _, tc := range []struct {
		name, side, path, value, reason, actualType string
		parts                                       []string
	}{
		{"unknown-mode", "client", "params.capabilities.elicitation.unknownMode", `{"elicitation":{"unknownMode":{}}}`, "no_recovery_rule", "object", []string{"params", "capabilities"}},
		{"nonempty-form", "client", "params.capabilities.elicitation.form.unknown", `{"elicitation":{"form":{"unknown":true}}}`, "nonempty_static_declaration", "boolean", []string{"params", "capabilities"}},
		{"nonempty-url", "client", "params.capabilities.elicitation.url.sessionToken", `{"elicitation":{"url":{"sessionToken":"TOP_SECRET_VALUE"}}}`, "nonempty_static_declaration", "string", []string{"params", "capabilities"}},
		{"null-form", "client", "params.capabilities.elicitation.form", `{"elicitation":{"form":null}}`, "invalid_type", "null", []string{"params", "capabilities"}},
		{"boolean-url", "client", "params.capabilities.elicitation.url", `{"elicitation":{"url":true}}`, "invalid_type", "boolean", []string{"params", "capabilities"}},
		{"array-url", "client", "params.capabilities.elicitation.url", `{"elicitation":{"url":[]}}`, "invalid_type", "array", []string{"params", "capabilities"}},
		{"unknown-server-capability", "server", "result.capabilities.tools.session", `{"tools":{"session":"TOP_SECRET_VALUE"}}`, "no_recovery_rule", "string", []string{"result", "capabilities"}},
		{"unknown-identity", "client", "params.clientInfo.session", `{"name":"client","session":"TOP_SECRET_VALUE"}`, "no_recovery_rule", "string", []string{"params", "clientInfo"}},
		{"unknown-icon", "server", "result.serverInfo.icons[0].session", `{"name":"server","icons":[{"src":"x","session":"TOP_SECRET_VALUE"}]}`, "no_recovery_rule", "string", []string{"result", "serverInfo"}},
		{"mistyped-icons", "server", "result.serverInfo.icons", `{"name":"server","icons":null}`, "invalid_type", "null", []string{"result", "serverInfo"}},
		{"metadata", "client", "params._meta.nonce", `{"protocolVersion":"2025-11-25","_meta":{"nonce":"TOP_SECRET_VALUE"}}`, "non_recoverable_metadata", "string", []string{"params"}},
		{"missing-negotiated-version", "server", "result.protocolVersion", `{"capabilities":{}}`, "missing_required_field", "missing", []string{"result"}},
		{"null-negotiated-version", "server", "result.protocolVersion", `null`, "invalid_type", "null", []string{"result", "protocolVersion"}},
		{"unsupported-negotiated-version", "server", "result.protocolVersion", `"2099-01-01"`, "unsupported_protocol_version", "string", []string{"result", "protocolVersion"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, resp := []byte(request), []byte(response)
			var err error
			if tc.side == "client" {
				req, err = wire.Replace(req, []byte(tc.value), tc.parts...)
			} else {
				resp, err = wire.Replace(resp, []byte(tc.value), tc.parts...)
			}
			if err != nil {
				t.Fatal(err)
			}
			r := &Recovery{}
			r.ObserveResponse(recoveryMessage(t, string(req)), recoveryMessage(t, string(resp)))
			d, exists := r.InitializationDiagnostic()
			if !exists || d.Side != tc.side || d.Path != tc.path || d.Reason != tc.reason || d.ActualType != tc.actualType || d.RequestedVersion != "2025-11-25" {
				t.Fatalf("unexpected diagnostic: %+v, exists=%v", d, exists)
			}
			if err := r.Preflight(); !errors.Is(err, ErrReconnectRequired) || !strings.Contains(err.Error(), d.Path) || strings.Contains(err.Error(), "TOP_SECRET_VALUE") {
				t.Fatalf("unsafe or missing failure detail: %v", err)
			}
			// The same known failure must stop Recover before any I/O. Calling
			// nil callbacks would panic if a rejected declaration were replayed.
			if err := r.Recover(context.Background(), nil, nil, nil); !errors.Is(err, ErrReconnectRequired) {
				t.Fatalf("failed preflight was not preserved: %v", err)
			}
		})
	}
}

func TestRecoveryInitializationDiagnosticIsStableAndCloneSafe(t *testing.T) {
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"zUnknown":{},"elicitation":{"url":{"z":"secret","a":"secret"},"form":{"z":"secret","a":"secret"}}}}}`)
	response := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
	r := &Recovery{}
	if _, exists := r.InitializationDiagnostic(); exists {
		t.Fatal("unobserved handshake has a diagnostic")
	}
	for i := 0; i < 100; i++ {
		d := recoveryInitializeDiagnostic(request, response)
		if d.Path != "params.capabilities.elicitation.form.a" || strings.Contains(d.String(), "secret") {
			t.Fatalf("unstable or unsafe diagnostic: %s", d.String())
		}
	}
	r.ObserveResponse(recoveryMessage(t, string(request)), recoveryMessage(t, string(response)))
	clone := r.Clone()
	r.initDiagnostic.Path = "changed"
	d, _ := clone.InitializationDiagnostic()
	if d.Path != "params.capabilities.elicitation.form.a" {
		t.Fatal("clone shares its diagnostic with the owner")
	}
	if got := recoveryFieldPath("params.capabilities", "bad.key\n"); got != `params.capabilities["bad.key\n"]` {
		t.Fatalf("ambiguous extension path: %s", got)
	}
}
