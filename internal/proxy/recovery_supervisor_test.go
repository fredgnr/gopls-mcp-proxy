package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func TestRecoveryPreflightRejectsBeforeCandidateStart(t *testing.T) {
	r, _ := testRelay(t, "2025-06-18")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":"not-dispatched","method":"tools/call","params":{"name":"sideEffect"}}`))
	r.recovery.blocked = "known unrecoverable state"
	r.proc, r.back = nil, nil
	r.phase = "restarting"
	// A factory is deliberately absent: a known rejection must not reach Start.
	r.scanned(scanDone{purpose: "initial", snap: r.snapshot})
	if r.phase != "closing" || !errors.Is(r.closingErr, ErrReconnectRequired) || r.startFinished != nil {
		t.Fatalf("preflight started a candidate or lost its failure: phase=%s error=%v", r.phase, r.closingErr)
	}
	response := routerFrame(t, routerRead(t, r.front).raw).Messages[0]
	if response.ID.Key != "s:not-dispatched" {
		t.Fatalf("lost queued request ID: %s", response.Raw)
	}
	assertRecoveryNotDispatched(t, response.Raw)
	if len(r.pending) != 0 || r.fatal != nil {
		t.Fatal("preflight dispatched the tool or terminated before writing its error")
	}
}

func TestRecoveryFailurePreservesBatchIDsCancellationAndNoReplay(t *testing.T) {
	r, _ := testRelay(t, "2025-03-26")
	r.fromFront([]byte(`{"jsonrpc":"2.0","id":99,"method":"sideEffect"}`))
	r.dispatch()
	dispatched := r.pending["n:99"]
	original := routerRead(t, r.back)
	r.written(writeDone{boot: r.proc.ID, size: len(original.raw)})
	r.fromFront([]byte(`[{"jsonrpc":"2.0","id":1,"method":"queuedOne"},{"jsonrpc":"2.0","id":"1","method":"queuedTwo"},{"jsonrpc":"2.0","id":2,"method":"cancelled"}]`))
	r.fromFront([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`))
	r.failRecovery(fmt.Errorf("%w: candidate contract changed", ErrReconnectRequired))
	if r.fatal != nil || r.phase != "closing" {
		t.Fatal("failure did not enter bounded frontend delivery")
	}
	item := routerRead(t, r.front)
	frame := routerFrame(t, item.raw)
	if !frame.Batch || len(frame.Messages) != 2 || frame.Messages[0].ID.Key != "n:1" || frame.Messages[1].ID.Key != "s:1" {
		t.Fatalf("queued failure lost batch grouping, typed IDs or cancellation: %s", item.raw)
	}
	for _, m := range frame.Messages {
		assertRecoveryNotDispatched(t, m.Raw)
	}
	if r.pending["n:99"] != dispatched || !dispatched.dispatched {
		t.Fatal("failure changed ownership of an already dispatched request")
	}
	routerEmpty(t, r.back)
	r.advance()
	if r.fatal != nil {
		t.Fatal("closed after enqueueing the error, before writer acknowledgement")
	}
	r.written(writeDone{size: len(item.raw)})
	r.advance()
	if !errors.Is(r.fatal, ErrReconnectRequired) {
		t.Fatalf("did not close after the queued errors were written: %v", r.fatal)
	}
	if strings.Count(r.fatal.Error(), "protocol_restart_required") != 1 {
		t.Fatalf("duplicated restart prefix: %v", r.fatal)
	}
	routerEmpty(t, r.back)
	routerEmpty(t, r.front)
}

func TestRecoveryFailureBoundsBlockedFrontendAndIgnoresNewTraffic(t *testing.T) {
	for _, budget := range []struct {
		name             string
		cleanup, request time.Duration
	}{
		{"cleanup", time.Second, 3 * time.Second},
		{"request", 3 * time.Second, time.Second},
	} {
		t.Run(budget.name, func(t *testing.T) {
			r, _ := testRelay(t, "2025-06-18")
			r.cfg.CleanupTimeout, r.cfg.RequestTimeout = budget.cleanup, budget.request
			r.fromFront([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
			before := time.Now()
			r.failRecovery(errors.New("candidate initialization failed"))
			after := time.Now()
			bound := min(budget.cleanup, budget.request)
			if r.closeAt.Before(before.Add(bound)) || r.closeAt.After(after.Add(bound)) {
				t.Fatalf("close deadline exceeds the shorter budget: %v", r.closeAt)
			}
			if !errors.Is(r.closingErr, ErrReconnectRequired) || strings.Count(r.closingErr.Error(), "protocol_restart_required") != 1 {
				t.Fatalf("non-sentinel recovery failure was not normalized: %v", r.closingErr)
			}
			// The writer never acknowledges the queued response. No new frontend
			// traffic or late backend result may reopen dispatch while it drains.
			item := routerRead(t, r.front)
			assertRecoveryNotDispatched(t, item.raw)
			r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`))
			r.fromBack([]byte(`{"jsonrpc":"2.0","id":"reverse","method":"roots/list"}`))
			r.scanned(scanDone{purpose: "initial", snap: r.snapshot})
			r.dirtyAt = time.Now().Add(-2 * r.cfg.RefreshTimeout)
			r.expire()
			r.advance()
			if r.phase != "closing" || r.fatal != nil || r.startFinished != nil || len(r.reverse) != 0 || len(r.pending) != 0 {
				t.Fatalf("late traffic changed closing state: phase=%s error=%v", r.phase, r.fatal)
			}
			routerEmpty(t, r.back)
			routerEmpty(t, r.front)
			r.closeAt = time.Now().Add(-time.Second)
			r.advance()
			if !errors.Is(r.fatal, ErrReconnectRequired) {
				t.Fatalf("blocked output prevented bounded termination: %v", r.fatal)
			}
		})
	}
}

func TestRecoveryInitializationDiagnosticIsEarlyAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		allowed     bool
	}{
		{"codex", "", true},
		{"unknown-state", `,"oneTimeToken":"SECRET-NONCE"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := testRelay(t, "2025-06-18")
			var logs bytes.Buffer
			r.cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"PRIVATE-CLIENT-NAME","title":"Codex","version":"0.163.0"},"capabilities":{"elicitation":{"form":{},"url":{}}}` + tc.extra + `}}`
			r.fromFront([]byte(request))
			r.dispatch()
			routerRead(t, r.back)
			r.fromBack([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"PRIVATE-SERVER-NAME","version":"1"},"capabilities":{}}}`))
			routerDeliverFresh(r)
			response := routerRead(t, r.front)
			if _, ok := wire.Get(response.raw, "result"); !ok || r.fatal != nil || r.phase != "ready" {
				t.Fatal("recovery diagnostic interrupted the original successful handshake")
			}
			// Rechecking the ledger should not repeatedly emit the same diagnosis.
			r.logInitializationRecovery()
			lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
			if len(lines) != 1 {
				t.Fatalf("want one early initialization diagnostic, got %s", logs.Bytes())
			}
			var entry map[string]any
			if err := json.Unmarshal(lines[0], &entry); err != nil {
				t.Fatal(err)
			}
			if entry["requested_version"] != "2025-11-25" || entry["negotiated_version"] != "2025-06-18" || entry["recoverable"] != tc.allowed {
				t.Fatalf("missing negotiation/eligibility diagnosis: %s", logs.Bytes())
			}
			if !tc.allowed && (entry["side"] != "client" || entry["path"] != "params.oneTimeToken" || entry["reason"] == "" || entry["reason"] == nil || entry["actual_type"] != "string") {
				t.Fatalf("missing structured rejection location: %s", logs.Bytes())
			}
			for _, secret := range []string{"SECRET-NONCE", "PRIVATE-CLIENT-NAME", "PRIVATE-SERVER-NAME"} {
				if bytes.Contains(logs.Bytes(), []byte(secret)) {
					t.Fatalf("diagnostic exposed initialization content %q", secret)
				}
			}
			if !tc.allowed {
				r.fromFront([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`))
				r.failRecovery(r.recovery.Preflight())
				failure := routerRead(t, r.front)
				assertRecoveryNotDispatched(t, failure.raw)
				if path, _ := recoveryString(failure.raw, "error", "data", "recovery", "path"); path != "params.oneTimeToken" {
					t.Fatalf("client-facing error lacks the rejection location: %s", failure.raw)
				}
				if bytes.Contains(failure.raw, []byte("SECRET-NONCE")) {
					t.Fatal("client-facing recovery error exposed initialization content")
				}
			}
		})
	}
}

func assertRecoveryNotDispatched(t *testing.T, raw []byte) {
	t.Helper()
	if reason, _ := recoveryString(raw, "error", "data", "reason"); reason != "protocol_restart_required" {
		t.Fatalf("missing explicit restart reason: %s", raw)
	}
	for _, name := range []string{"requestDispatched", "automaticReplay"} {
		if value, _ := wire.Get(raw, "error", "data", name); string(value) != "false" {
			t.Fatalf("incorrect %s marker in recovery failure: %s", name, raw)
		}
	}
}
