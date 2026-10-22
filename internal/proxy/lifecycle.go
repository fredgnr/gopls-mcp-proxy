package proxy

import (
	"strings"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func (r *relay) onDemand() bool { return r.cfg.BackendLifecycle == BackendOnDemand }

// A sleeping frontend retains its protocol ledger, but no elapsed recovery
// budget. Filesystem dirtiness is independently checked by the next full scan.
func (r *relay) enterDormant() {
	r.cancelScan()
	r.phase = "idle"
	r.startAt, r.dirtyAt, r.wakeAt, r.retryAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	r.idleSince, r.toolWrittenAt = time.Time{}, time.Time{}
	r.retryDelay = r.cfg.RetryMin
	r.retiringCall = 0
	r.startingCall = nil
	r.abandonedStart, r.stopToIdle = false, false
	r.abandonedAt = time.Time{}
	r.suspendDeferred = ""
	r.poll = false
}

func (r *relay) cancelScan() {
	if r.scanCancel != nil {
		r.scanCancel()
		r.scanCancel = nil
	}
	// Invalidate even a result that has finished scanning but is still queued
	// in events; it cannot publish a stopped candidate into a later wake.
	r.scanSequence++
	r.scanning = false
}

func (r *relay) noteActivity() { r.idleSince = time.Time{} }

func (r *relay) beginWake() {
	if !r.onDemand() || !r.wakeAt.IsZero() {
		return
	}
	r.wakeAt = time.Now()
	r.dirtyAt = time.Time{}
	r.retryAt = time.Time{}
	reason := "protocol_obligation"
	if len(r.queue) > 0 && len(r.queue[0].msgs) > 0 {
		m := r.queue[0].msgs[0]
		switch {
		case m.Method == "initialize":
			reason = "initialize"
		case r.queue[0].isToolCall():
			reason = "tools_call"
		case m.Kind == wire.Notification:
			reason = "notification"
		default:
			reason = "ordinary_request"
		}
	}
	r.cfg.Logger.Info("backend wake", "reason", reason, "queued", r.queueCount, "queue_bytes", r.queueBytes, "recovery_state_bytes", r.recovery.StateBytes())
}

// The queue's existing enqueue deadlines bound a wake; cancellation may remove
// a demand but retries may never extend a remaining request's total lifetime.
func (r *relay) demandDeadline(deadline time.Time) time.Time {
	for _, q := range r.queue {
		if !q.deadline.IsZero() && q.deadline.Before(deadline) {
			deadline = q.deadline
		}
	}
	return deadline
}

func (r *relay) refreshDeadline() time.Time {
	start := r.dirtyAt
	if r.onDemand() {
		start = r.wakeAt
	}
	if start.IsZero() {
		return time.Time{}
	}
	return start.Add(r.cfg.RefreshTimeout)
}

func (r *relay) hasDemand() bool {
	if len(r.queue) > 0 || r.recovery.NeedsResident() {
		return true
	}
	for _, rec := range r.pending {
		if !rec.cancelled {
			return true
		}
	}
	// A candidate's reverse handshake belongs to its abandoned wake. It is
	// cancelled with recovery, rather than keeping that recovery alive forever.
	return r.phase != "recovering" && len(r.reverse) > 0
}

func (r *relay) abandonUnneededWake() bool {
	if !r.onDemand() || r.hasDemand() {
		return false
	}
	switch r.phase {
	case "idle", "restarting":
		if r.scanning || r.phase == "restarting" || !r.wakeAt.IsZero() {
			r.enterDormant()
		}
		return true
	case "starting":
		if !r.abandonedStart {
			r.abandonedAt = time.Now()
		}
		r.abandonedStart = true
		r.wakeAt, r.startAt = time.Time{}, time.Time{}
		if r.startCancel != nil {
			r.startCancel()
		}
		// Wait for Start's result (and its own cleanup). There must never be
		// two concurrent factory attempts after a cancellation/new-request race.
		return true
	case "recovering", "publishing", "draining":
		r.stopCurrent("request_cancelled")
		return true
	case "ready":
		if !r.bootDemandServed {
			// Cancellation may win after publication but before the dispatch
			// checkpoint. A pristine ledger's incomplete handshake must not pin
			// a candidate that never received any original client demand.
			r.cancelScan()
			if r.callBoundaryIdle() {
				r.stopCurrent("request_cancelled")
			}
			return true
		}
	}
	return false
}

func (r *relay) suspendBlockers() []string {
	blockers := r.recovery.SuspendBlockers()
	for _, rec := range r.pending {
		if rec.long && !rec.cancelled {
			blockers = append(blockers, "active_modern_subscription")
			break
		}
	}
	return blockers
}

// Reservation begins before the output write, so an ordinary queued request
// cannot enter this boot in the gap between pending removal and writer ACK.
func (r *relay) retirementPending() bool {
	return r.onDemand() && r.proc != nil && r.retiringCall == r.proc.ID && len(r.suspendBlockers()) == 0
}

// advanceLifecycle returns true when this generation is reserved for retirement
// (even while waiting for writer ACKs), or has begun cleanup. Protocol blockers
// keep it serviceable; self-imposed memory policy must not break a live session.
func (r *relay) advanceLifecycle() bool {
	if !r.onDemand() || r.proc == nil || r.phase != "ready" {
		return false
	}
	blockers := r.suspendBlockers()
	if len(blockers) > 0 {
		reason := strings.Join(blockers, ",")
		if reason != r.suspendDeferred {
			r.cfg.Logger.Info("backend suspension deferred", "reason", reason, "boot_id", r.proc.ID, "pending", len(r.pending), "queued", r.queueCount, "recovery_state_bytes", r.recovery.StateBytes())
			r.suspendDeferred = reason
		}
		return false
	}
	r.suspendDeferred = ""
	if r.retiringCall == r.proc.ID {
		if !r.toolWrittenAt.IsZero() && r.callBoundaryIdle() {
			r.stopCurrent("call_complete")
		}
		return true
	}
	if len(r.queue) != 0 {
		return false
	}
	// Logging output and periodic scans may temporarily defer cleanup, but do
	// not restart an already running idle grace period. Only real interaction
	// calls noteActivity; initial timing starts after complete delivery.
	if r.callBoundaryIdle() {
		if r.idleSince.IsZero() {
			r.idleSince = time.Now()
		}
		if time.Since(r.idleSince) >= r.cfg.BackendIdleTimeout {
			r.stopCurrent("idle_timeout")
			return true
		}
	}
	return false
}
