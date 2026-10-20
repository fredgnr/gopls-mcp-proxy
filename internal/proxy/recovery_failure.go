package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

// logInitializationRecovery reports static initialization eligibility as soon
// as the successful handshake is forwarded. This is not a promise that future
// backend state, directory snapshots or protocol settings will remain equal.
func (r *relay) logInitializationRecovery() {
	if r.initializationLogged {
		return
	}
	d, ok := r.recovery.InitializationDiagnostic()
	if !ok {
		return
	}
	r.initializationLogged = true
	args := []any{"requested_version", d.RequestedVersion, "negotiated_version", d.NegotiatedVersion, "recoverable", d.Reason == ""}
	if d.Reason != "" {
		args = append(args, "side", d.Side, "path", d.Path, "reason", d.Reason, "actual_type", d.ActualType)
		r.cfg.Logger.Warn("initialization recovery eligibility", args...)
		return
	}
	r.cfg.Logger.Info("initialization recovery eligibility", args...)
}

// failRecovery never publishes the candidate or resumes the stale process.
// Give queued, never-dispatched requests one bounded chance to receive a useful
// error before Run closes the streams and reaps any remaining candidate.
func (r *relay) failRecovery(err error) {
	if r.phase == "closing" || r.fatal != nil {
		return
	}
	if !errors.Is(err, ErrReconnectRequired) {
		err = fmt.Errorf("%w: %w", ErrReconnectRequired, err)
	}
	r.closingErr = err
	r.closeAt = time.Now().Add(min(r.cfg.CleanupTimeout, r.cfg.RequestTimeout))
	r.phase = "closing"
	r.failControls(err)
	for _, q := range r.queue {
		for _, rec := range q.records {
			if !rec.cancelled && !rec.dispatched {
				r.finish(rec, r.recoveryError(rec), false)
			}
		}
	}
	r.queue = nil
	r.queueCount, r.queueBytes = 0, 0
}

func (r *relay) recoveryError(rec *record) []byte {
	raw := r.proxyError(rec, "protocol_restart_required")
	d, ok := r.recovery.InitializationDiagnostic()
	if !ok || d.Reason == "" {
		return raw
	}
	// This is a proxy-created error, never an upstream message. Include only
	// diagnostic paths/types, not capability values, metadata or whole envelopes.
	dataRaw, _ := wire.Get(raw, "error", "data")
	var data map[string]any
	if json.Unmarshal(dataRaw, &data) != nil {
		return raw
	}
	data["recovery"] = map[string]string{
		"requested_version": d.RequestedVersion, "negotiated_version": d.NegotiatedVersion,
		"side": d.Side, "path": d.Path, "reason": d.Reason, "actual_type": d.ActualType,
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	updated, err := wire.Replace(raw, encoded, "error", "data")
	if err != nil {
		return raw
	}
	return updated
}
