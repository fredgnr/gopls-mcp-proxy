package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/protocol"
	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

func (r *relay) fromFront(raw []byte) {
	if r.phase == "closing" {
		return
	}
	f, err := wire.ParseFrame(raw)
	if err != nil {
		r.fatal = err
		return
	}
	if p, known := protocol.Lookup(r.recovery.Revision()); known {
		if err := protocol.ValidateFrame(p, f); err != nil {
			r.fatal = err
			return
		}
	}
	for _, m := range f.Messages {
		if p, known := protocol.Lookup(protocol.Version(m)); known {
			if err := protocol.ValidateFrame(p, f); err != nil {
				r.fatal = err
				return
			}
		}
	}
	if r.cfg.RefreshMode == RefreshPerCall && r.rejectPerCallBatch(f) {
		return
	}
	q := &queued{batch: f.Batch, bytes: len(raw), deadline: time.Now().Add(r.cfg.RequestTimeout)}
	group := &batchGroup{batch: f.Batch}
	for _, m := range f.Messages {
		if m.Kind == wire.Response {
			r.frontResponse(m)
			continue
		}
		if id, ok := protocol.CancellationID(m); ok {
			r.cancelRequest(m, id)
			continue
		}
		if m.Method == "notifications/progress" {
			if r.proc == nil || r.phase == "stopping" {
				continue
			}
			if token, ok := protocol.ProgressToken(m); ok {
				for _, rev := range r.reverse {
					if rev.boot == r.proc.ID && rev.mappedProgress.Key == token.Key {
						b, err := wire.Replace(m.Raw, rev.progress.Raw, "params", "progressToken")
						if err == nil {
							r.sendBack(b)
						}
						break
					}
				}
			}
			continue
		}
		if m.Kind == wire.Notification && isControlNotification(m) && r.proc != nil && (r.phase == "ready" || r.phase == "draining") {
			r.recovery.ObserveRequest(m)
			r.sendBack(m.Raw)
			continue
		}
		q.msgs = append(q.msgs, m)
		if m.Kind == wire.Request {
			for _, rec := range q.records {
				if rec.msg.ID.Key == m.ID.Key {
					r.fatal = errors.New("duplicate request ID in batch")
					return
				}
			}
			if token, ok := protocol.ProgressToken(m); ok {
				for _, rec := range r.pending {
					if rec.progress.Key == token.Key {
						r.invalidate("ambiguous_progress_reuse")
					}
				}
				for _, prior := range q.msgs[:len(q.msgs)-1] {
					if tok, ok := protocol.ProgressToken(prior); ok && tok.Key == token.Key {
						r.fatal = errors.New("duplicate progress token in batch")
						return
					}
				}
				for _, old := range r.queue {
					for _, prior := range old.msgs {
						if tok, ok := protocol.ProgressToken(prior); ok && tok.Key == token.Key {
							r.fatal = errors.New("duplicate queued progress token")
							return
						}
					}
				}
			}
			if prev := r.pending[m.ID.Key]; prev != nil {
				if prev.cancelled {
					r.invalidate("ambiguous_id_reuse")
				} else {
					r.fatal = errors.New("duplicate outstanding request ID")
					return
				}
			}
			for _, old := range r.queue {
				for _, rec := range old.records {
					if !rec.cancelled && rec.msg.ID.Key == m.ID.Key {
						r.fatal = errors.New("duplicate queued request ID")
						return
					}
				}
			}
			rec := &record{msg: m, group: group, deadline: q.deadline}
			q.records = append(q.records, rec)
			group.remaining++
		}
	}
	if len(q.msgs) == 0 {
		return
	}
	if len(q.records) > r.cfg.MaxConcurrent {
		if len(q.msgs) > len(q.records) {
			r.fatal = errors.New("mixed batch notification cannot be admitted")
			return
		}
		for _, rec := range q.records {
			r.finish(rec, r.proxyError(rec, "batch_concurrency_limit"), false)
		}
		if len(q.records) == 0 {
			r.fatal = errors.New("notification capacity exceeded")
		}
		return
	}
	if r.queueCount+len(q.records) > r.cfg.QueueLimit || r.queueBytes+q.bytes > r.cfg.QueueBytes {
		if len(q.msgs) > len(q.records) {
			r.fatal = errors.New("notification queue full")
			return
		}
		for _, rec := range q.records {
			r.finish(rec, r.proxyError(rec, "queue_full"), false)
		}
		return
	}
	r.poll = true
	r.queue = append(r.queue, q)
	r.queueCount += len(q.records)
	r.queueBytes += q.bytes
}
func (r *relay) frontResponse(m wire.Message) {
	rev, ok := r.reverse[m.ID.Key]
	if !ok {
		return
	}
	delete(r.reverse, m.ID.Key)
	if r.proc == nil || rev.boot != r.proc.ID {
		return
	}
	b, err := wire.Replace(m.Raw, rev.original.Raw, "id")
	if err != nil {
		r.fatal = err
		return
	}
	if rev.group != nil {
		r.reverseBytes += len(b)
		if r.reverseBytes > r.cfg.QueueBytes {
			r.fatal = errors.New("reverse response byte budget exceeded")
			return
		}
		rev.group.responses = append(rev.group.responses, b)
		rev.group.remaining--
		if rev.group.remaining == 0 && len(rev.group.responses) > 0 {
			for _, raw := range rev.group.responses {
				r.reverseBytes -= len(raw)
			}
			r.sendBack(joinFrame(rev.group.responses, rev.group.batch))
			rev.group.responses = nil
		}
	} else {
		r.sendBack(b)
	}
}
func (r *relay) cancelRequest(m wire.Message, id wire.ID) {
	for _, q := range r.queue {
		for _, rec := range q.records {
			if !rec.cancelled && rec.msg.ID.Key == id.Key {
				rec.cancelled = true
				r.cancelGroup(rec)
			}
		}
	}
	r.pruneQueue()
	if rec := r.pending[id.Key]; rec != nil && !rec.cancelled {
		rec.cancelled = true
		r.recovery.MarkUncertain(rec.msg)
		r.cancelGroup(rec)
		r.sendBack(m.Raw)
		if !protocol.IsSubscription(rec.msg) {
			r.invalidate("request_cancelled")
		} else {
			rec.long = true
		}
	}
}
func (r *relay) cancelGroup(rec *record) {
	if rec.group == nil {
		return
	}
	g := rec.group
	rec.group = nil
	if rec.buffered {
		for i, v := range g.records {
			if v == rec {
				r.resultBytes -= len(g.responses[i])
				g.records = append(g.records[:i], g.records[i+1:]...)
				g.responses = append(g.responses[:i], g.responses[i+1:]...)
				g.upstream = append(g.upstream[:i], g.upstream[i+1:]...)
				break
			}
		}
	} else {
		g.remaining--
	}
	r.flushGroup(g)
}
func (r *relay) pruneQueue() {
	out := r.queue[:0]
	for _, q := range r.queue {
		live := map[string]bool{}
		records := q.records[:0]
		for _, rec := range q.records {
			if !rec.cancelled {
				records = append(records, rec)
				live[rec.msg.ID.Key] = true
			} else {
				r.queueCount--
			}
		}
		q.records = records
		msgs := q.msgs[:0]
		for _, m := range q.msgs {
			if m.Kind != wire.Request || live[m.ID.Key] {
				msgs = append(msgs, m)
			}
		}
		q.msgs = msgs
		if len(msgs) == 0 {
			r.queueBytes -= q.bytes
		} else {
			newBytes := len(joinFrame(q.msgsRaw(), q.batch))
			r.queueBytes += newBytes - q.bytes
			q.bytes = newBytes
			out = append(out, q)
		}
	}
	r.queue = out
}
func (r *relay) canDispatch(q *queued) bool {
	if r.phase != "ready" || r.proc == nil {
		return false
	}
	if r.cfg.RefreshMode == RefreshPerCall {
		if q.isToolCall() && (q.freshBoot != r.proc.ID || !r.callBoundaryIdle()) {
			return false
		}
		for _, rec := range r.pending {
			if rec.msg.Method == "tools/call" {
				return false
			}
		}
	}
	if len(r.pending)+len(q.records) > r.cfg.QueueLimit+r.cfg.MaxConcurrent {
		return false
	}
	pendingBytes := 0
	for _, rec := range r.pending {
		pendingBytes += len(rec.msg.Raw)
	}
	if pendingBytes+q.bytes > r.cfg.QueueBytes {
		return false
	}
	if r.active()+len(q.records) > r.cfg.MaxConcurrent {
		return false
	}
	for _, rec := range q.records {
		if r.pending[rec.msg.ID.Key] != nil {
			return false
		}
	}
	return true
}
func (r *relay) dispatch() {
	q := r.queue[0]
	r.queue = r.queue[1:]
	r.queueCount -= len(q.records)
	r.queueBytes -= q.bytes
	records := map[string]*record{}
	for _, rec := range q.records {
		rec.boot = r.proc.ID
		rec.generation = r.generation
		rec.dispatched = true
		r.pending[rec.msg.ID.Key] = rec
		records[rec.msg.ID.Key] = rec
	}
	raws := make([][]byte, 0, len(q.msgs))
	for _, m := range q.msgs {
		b := m.Raw
		if rec := records[m.ID.Key]; rec != nil {
			if token, ok := protocol.ProgressToken(m); ok {
				r.sequence++
				mapped := wire.StringID(fmt.Sprintf("__proxy/progress/%d/%d", r.proc.ID, r.sequence))
				rec.progress = token
				rec.mappedProgress = mapped
				r.progress[mapped.Key] = rec
				var err error
				b, err = wire.Replace(b, mapped.Raw, "params", "_meta", "progressToken")
				if err != nil {
					r.fatal = err
					return
				}
			}
		}
		if v := protocol.Version(m); v != "" && m.Method != "initialize" {
			r.recovery.SetRevision(v)
		}
		r.recovery.ObserveRequest(m)
		raws = append(raws, b)
	}
	// Ownership is registered before the first potentially partial write. Nothing
	// after this point can put a request back into the admission queue.
	r.sendBack(joinFrame(raws, q.batch))
}
func joinFrame(raws [][]byte, batch bool) []byte {
	if len(raws) == 1 && !batch {
		return raws[0]
	}
	return append(append([]byte{'['}, bytes.Join(raws, []byte{','})...), ']')
}
func (r *relay) fromBack(raw []byte) {
	if r.phase == "closing" {
		return
	}
	f, err := wire.ParseFrame(raw)
	if err != nil {
		r.fatal = fmt.Errorf("backend protocol frame: %w", err)
		return
	}
	if p, known := protocol.Lookup(r.recovery.Revision()); known {
		if err := protocol.ValidateFrame(p, f); err != nil {
			r.fatal = err
			return
		}
	}
	var results []wire.Message
	var reverseOut [][]byte
	revGroup := &reverseGroup{batch: f.Batch}
	for _, m := range f.Messages {
		if m.Kind == wire.Request {
			revGroup.remaining++
		}
	}
	for _, m := range f.Messages {
		if m.Kind == wire.Response {
			if c, ok := r.controls[m.ID.Key]; ok {
				delete(r.controls, m.ID.Key)
				b, err := wire.Replace(m.Raw, c.id.Raw, "id")
				c.reply <- internalReply{b, err}
				continue
			}
			rec := r.pending[m.ID.Key]
			if rec == nil {
				continue
			}
			if rec.cancelled {
				delete(r.pending, m.ID.Key)
				delete(r.progress, rec.mappedProgress.Key)
				continue
			}
			if rec.received {
				r.fatal = errors.New("duplicate backend terminal response")
				return
			}
			rec.received = true
			if rec.long {
				r.finish(rec, m.Raw, false)
				delete(r.pending, m.ID.Key)
				delete(r.progress, rec.mappedProgress.Key)
				continue
			}
			results = append(results, m)
			continue
		}
		if m.Kind == wire.Request {
			p, _ := protocol.Lookup(r.recovery.Revision())
			if p.Modern {
				r.fatal = errors.New("modern backend sent forbidden reverse request")
				return
			}
			for _, rev := range r.reverse {
				if rev.original.Key == m.ID.Key {
					r.fatal = errors.New("duplicate backend reverse request ID")
					return
				}
			}
			if len(r.reverse) >= r.cfg.QueueLimit {
				r.fatal = errors.New("reverse request capacity exceeded")
				return
			}
			r.sequence++
			id := wire.StringID(fmt.Sprintf("__proxy/server/%d/%d", r.proc.ID, r.sequence))
			rev := reverseRecord{original: m.ID, boot: r.proc.ID, deadline: time.Now().Add(r.cfg.RequestTimeout), group: revGroup}
			if tok, ok := protocol.ProgressToken(m); ok {
				for _, old := range r.reverse {
					if old.progress.Key == tok.Key {
						r.fatal = errors.New("duplicate reverse progress token")
						return
					}
				}
				rev.progress = tok
				rev.mappedProgress = wire.StringID(fmt.Sprintf("__proxy/serverprogress/%d/%d", r.proc.ID, r.sequence))
			}
			r.reverse[id.Key] = rev
			b, err := wire.Replace(m.Raw, id.Raw, "id")
			if err != nil {
				r.fatal = err
				return
			}
			if rev.mappedProgress.Key != "" {
				b, err = wire.Replace(b, rev.mappedProgress.Raw, "params", "_meta", "progressToken")
				if err != nil {
					r.fatal = err
					return
				}
			}
			reverseOut = append(reverseOut, b)
			continue
		}
		if id, ok := protocol.CancellationID(m); ok {
			if rec := r.pending[id.Key]; rec != nil && protocol.IsSubscription(rec.msg) {
				if !rec.cancelled {
					rec.cancelled = true
					r.cancelGroup(rec)
					r.sendFront(m.Raw)
				}
				delete(r.pending, id.Key)
				delete(r.progress, rec.mappedProgress.Key)
				continue
			}
			for mapped, rev := range r.reverse {
				if rev.original.Key == id.Key {
					mid := wire.StringID(mapped[2:])
					b, err := wire.Replace(m.Raw, mid.Raw, "params", "requestId")
					if err == nil {
						r.sendFront(b)
					}
					delete(r.reverse, mapped)
					if rev.group != nil {
						rev.group.remaining--
						if rev.group.remaining == 0 && len(rev.group.responses) > 0 {
							for _, raw := range rev.group.responses {
								r.reverseBytes -= len(raw)
							}
							r.sendBack(joinFrame(rev.group.responses, rev.group.batch))
							rev.group.responses = nil
						}
					}
					break
				}
			}
			continue
		}
		if token, ok := protocol.ProgressToken(m); ok {
			rec := r.progress[token.Key]
			if rec == nil || rec.cancelled {
				continue
			}
			b, err := wire.Replace(m.Raw, rec.progress.Raw, "params", "progressToken")
			if err != nil {
				r.fatal = err
				return
			}
			r.sendFront(b)
			continue
		}
		if id, ok := protocol.SubscriptionID(m); ok {
			rec := r.pending[id.Key]
			if rec == nil || rec.cancelled || !protocol.IsSubscription(rec.msg) {
				continue
			}
			if protocol.IsSubscriptionAck(m) {
				rec.long = true
			}
			r.sendFront(m.Raw)
			continue
		}
		if r.phase != "stopping" {
			r.sendFront(m.Raw)
		}
	}
	if len(reverseOut) > 0 {
		r.sendFront(joinFrame(reverseOut, f.Batch))
	}
	if len(results) > 0 {
		if r.resultBytes+len(raw) > r.cfg.QueueBytes {
			r.fatal = errors.New("pending result byte budget exceeded")
			return
		}
		r.results = append(r.results, resultFrame{results, r.proc.ID, len(raw)})
		r.resultBytes += len(raw)
	}
}
func (r *relay) deliverResults(frames []resultFrame, fresh bool, reason string) {
	for _, f := range frames {
		r.resultBytes -= f.bytes
	}
	if r.resultBytes < 0 {
		r.resultBytes = 0
	}
	for _, f := range frames {
		for _, m := range f.msgs {
			rec := r.pending[m.ID.Key]
			if rec == nil || rec.boot != f.boot {
				continue
			}
			if rec.cancelled {
				delete(r.pending, m.ID.Key)
				delete(r.progress, rec.mappedProgress.Key)
				continue
			}
			if fresh {
				r.finish(rec, m.Raw, true)
			} else {
				r.finish(rec, r.proxyError(rec, reason), false)
			}
		}
	}
}
func (r *relay) finish(rec *record, raw []byte, upstream bool) {
	if rec.cancelled || rec.group == nil {
		return
	}
	g := rec.group
	if rec.buffered {
		for i, v := range g.records {
			if v == rec {
				r.resultBytes += len(raw) - len(g.responses[i])
				g.responses[i] = raw
				g.upstream[i] = upstream
				break
			}
		}
	} else {
		rec.buffered = true
		g.records = append(g.records, rec)
		g.responses = append(g.responses, raw)
		g.upstream = append(g.upstream, upstream)
		g.remaining--
		r.resultBytes += len(raw)
	}
	if r.resultBytes > r.cfg.QueueBytes {
		r.fatal = errors.New("buffered response byte budget exceeded")
		return
	}
	r.flushGroup(g)
}
func (r *relay) flushGroup(g *batchGroup) {
	if g.done || g.remaining != 0 {
		return
	}
	hasSuccess := false
	for _, v := range g.upstream {
		hasSuccess = hasSuccess || v
	}
	if hasSuccess && r.phase == "ready" {
		if !g.queued {
			g.queued = true
			r.readyBatches = append(r.readyBatches, g)
		}
		return
	}
	r.deliverBatch(g, false)
}
func (r *relay) deliverBatch(g *batchGroup, fresh bool) {
	if g.done {
		return
	}
	g.done = true
	var initializationErr error
	for i, rec := range g.records {
		r.resultBytes -= len(g.responses[i])
		if g.upstream[i] {
			if r.phase == "closing" {
				// An earlier initialize in this same scan may have rejected
				// per-call mode. Do not publish another successful session result.
				g.responses[i] = r.recoveryError(rec)
			} else if !fresh || rec.generation != r.generation || r.proc == nil || rec.boot != r.proc.ID || r.tracker.Epoch() != r.snapshot.Epoch {
				g.responses[i] = r.proxyError(rec, "workspace_changed")
			} else {
				m, err := wire.ParseMessage(g.responses[i])
				if err == nil {
					r.recovery.ObserveResponse(rec.msg, m)
					if rec.msg.Method == "initialize" {
						r.logInitializationRecovery()
						if _, success := wire.Get(m.Raw, "result"); success && r.cfg.RefreshMode == RefreshPerCall {
							if d, known := r.recovery.InitializationDiagnostic(); known && d.Reason != "" {
								// Every tool call must replace the backend in this
								// mode. Reject a known incompatibility now instead
								// of reporting a usable session until its first call.
								// Preflight is intentionally not used: initialized
								// has not arrived yet, even for a valid handshake.
								initializationErr = fmt.Errorf("%w: %s", ErrReconnectRequired, d.String())
								g.responses[i] = r.recoveryError(rec)
							}
						}
					}
				}
			}
		}
		rec.group = nil
		if r.pending[rec.msg.ID.Key] == rec {
			delete(r.pending, rec.msg.ID.Key)
			delete(r.progress, rec.mappedProgress.Key)
		}
	}
	if r.resultBytes < 0 {
		r.resultBytes = 0
	}
	if len(g.responses) > 0 {
		r.sendFront(joinFrame(g.responses, g.batch))
	}
	g.responses = nil
	g.records = nil
	if initializationErr != nil {
		// Enqueue the initialize error before failing queued requests. The
		// bounded closing path waits for writer acknowledgement and reaps the
		// backend; this initialize was dispatched and its response was received.
		r.failRecovery(initializationErr)
	}
}
func (r *relay) proxyError(rec *record, reason string) []byte {
	if rec.dispatched {
		r.recovery.MarkUncertain(rec.msg)
	}
	return protocol.ErrorResponse(rec.msg.ID, reason, rec.generation, rec.dispatched, rec.received)
}
func (r *relay) endSubscriptions() {
	for _, rec := range r.pending {
		if protocol.IsSubscription(rec.msg) && !rec.cancelled {
			r.sendFront(protocol.Cancellation(rec.msg.ID, "backend refreshed; establish a new subscription"))
			r.sendBack(protocol.Cancellation(rec.msg.ID, "backend refresh"))
			rec.cancelled = true
			r.cancelGroup(rec)
			rec.long = true
		}
	}
}

// Only protocol control messages may bypass an earlier queued ordinary message.
func isControlNotification(m wire.Message) bool {
	switch m.Method {
	case "notifications/initialized", "notifications/roots/list_changed":
		return true
	}
	return false
}

func (q *queued) msgsRaw() [][]byte {
	out := make([][]byte, len(q.msgs))
	for i, m := range q.msgs {
		out[i] = m.Raw
	}
	return out
}

func (q *queued) isToolCall() bool {
	for _, rec := range q.records {
		if rec.msg.Method == "tools/call" && !rec.cancelled {
			return true
		}
	}
	return false
}

// Per-call isolation cannot share a process between batch members. Reject such
// a batch before handling even its control messages, rather than execute some
// members or silently weaken the per-call guarantee. Ordinary batches keep the
// normal revision-specific behavior.
func (r *relay) rejectPerCallBatch(f wire.Frame) bool {
	if !f.Batch {
		return false
	}
	hasCall := false
	for _, m := range f.Messages {
		hasCall = hasCall || (m.Kind == wire.Request && m.Method == "tools/call")
	}
	if !hasCall {
		return false
	}
	ids := make(map[string]bool)
	for _, m := range f.Messages {
		if m.Kind != wire.Request {
			r.fatal = errors.New("per_call_batch_unsupported: tools/call must be sent as an individual request")
			return true
		}
		if ids[m.ID.Key] {
			r.fatal = errors.New("duplicate request ID in batch")
			return true
		}
		ids[m.ID.Key] = true
		if rec := r.pending[m.ID.Key]; rec != nil && !rec.cancelled {
			r.fatal = errors.New("duplicate outstanding request ID")
			return true
		}
		for _, q := range r.queue {
			for _, rec := range q.records {
				if rec.msg.ID.Key == m.ID.Key && !rec.cancelled {
					r.fatal = errors.New("duplicate queued request ID")
					return true
				}
			}
		}
	}
	responses := make([][]byte, 0, len(f.Messages))
	for _, m := range f.Messages {
		responses = append(responses, protocol.ErrorResponse(m.ID, "per_call_batch_unsupported", 0, false, false))
	}
	r.sendFront(joinFrame(responses, true))
	return true
}
