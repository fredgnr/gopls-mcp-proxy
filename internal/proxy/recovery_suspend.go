package proxy

import (
	"encoding/json"
	"strings"

	"github.com/fredgnr/gopls-mcp-proxy/internal/protocol"
	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

// Fixed flags keep unknown method names, task IDs and opaque continuation
// values out of the ledger and logs. They are deliberately sticky for one
// frontend session: there is no protocol-wide proof that all state was freed.
type suspendState uint8

const (
	suspendTasks suspendState = 1 << iota
	suspendContinuation
	suspendUnknownClientNotification
	suspendUnknownServerNotification
	suspendUnknownResult
)

// SuspendBlockers describes why voluntarily retiring a healthy backend would
// lose observed protocol state. It is side-effect free and separate from
// Preflight: a forced per-call refresh still uses the original recovery rules.
// The relay separately checks live requests (including modern subscriptions),
// scans, output acknowledgements and all per-generation buffers.
func (r *Recovery) SuspendBlockers() []string {
	var blockers []string
	if r.blocked != "" {
		blockers = append(blockers, "unrecoverable_protocol_state")
	}
	if r.revision == protocol.ModernVersion && len(r.initRequest) == 0 {
		if len(r.logLevel) != 0 {
			blockers = append(blockers, "modern_session_state")
		}
	} else if len(r.initRequest) == 0 || len(r.initialized) == 0 {
		blockers = append(blockers, "initialization_incomplete")
	} else if !legacyRecoveryRevision(r.revision) {
		blockers = append(blockers, "unsupported_protocol_version")
	}
	if r.revision != "" {
		if _, known := protocol.Lookup(r.revision); !known && !containsSuspendBlocker(blockers, "unsupported_protocol_version") {
			blockers = append(blockers, "unsupported_protocol_version")
		}
	}
	if len(r.subscribed) != 0 {
		blockers = append(blockers, "resource_subscription")
	}
	for _, method := range sortedRecoveryKeys(r.directories) {
		view := r.directories[method]
		if view == nil || view.invalid || !view.complete || view.hasNext {
			blockers = append(blockers, "incomplete_directory:"+method)
		}
	}
	for _, flag := range []struct {
		state  suspendState
		reason string
	}{
		{suspendTasks, "task_state"},
		{suspendContinuation, "continuation_state"},
		{suspendUnknownClientNotification, "unknown_client_notification"},
		{suspendUnknownServerNotification, "unknown_server_notification"},
		{suspendUnknownResult, "unknown_result_type"},
	} {
		if r.suspendState&flag.state != 0 {
			blockers = append(blockers, flag.reason)
		}
	}
	return blockers
}

// NeedsResident distinguishes a pristine connection, which must not wake a
// backend without demand, from a session with continuing protocol obligations.
func (r *Recovery) NeedsResident() bool {
	for _, reason := range r.SuspendBlockers() {
		if reason != "initialization_incomplete" || r.NeedsRecovery() {
			return true
		}
	}
	return false
}

func containsSuspendBlocker(blockers []string, reason string) bool {
	for _, blocker := range blockers {
		if blocker == reason {
			return true
		}
	}
	return false
}

func (r *Recovery) observeSuspendRequest(m wire.Message) {
	if m.Kind != wire.Request && m.Kind != wire.Notification {
		return
	}
	if isTaskMethod(m.Method) || hasProtocolField(m.Raw, "params", "task") || hasProtocolField(m.Raw, "params", "_meta", "io.modelcontextprotocol/related-task") {
		r.suspendState |= suspendTasks
	}
	if m.Method == "initialize" && hasProtocolField(m.Raw, "params", "capabilities", "tasks") {
		r.suspendState |= suspendTasks
	}
	if hasProtocolField(m.Raw, "params", "_meta", protocol.ClientCapabilitiesKey, "tasks") {
		r.suspendState |= suspendTasks
	}
	if protocol.Version(m) == protocol.ModernVersion && (hasProtocolField(m.Raw, "params", "requestState") || hasProtocolField(m.Raw, "params", "inputResponses")) {
		r.suspendState |= suspendContinuation
	}
	if m.Kind == wire.Notification {
		switch m.Method {
		case "notifications/initialized", "notifications/roots/list_changed", "notifications/progress", protocol.CancellationMethod, "notifications/tasks/status":
			// Their obligations are tracked separately or are task-sticky.
		default:
			r.suspendState |= suspendUnknownClientNotification
		}
	}
}

func (r *Recovery) observeSuspendResponse(request, response wire.Message) {
	// Do not infer state from arbitrary content or tool arguments. Only fixed
	// protocol envelope fields are inspected; native bytes remain untouched.
	if hasProtocolField(response.Raw, "result", "task") || hasProtocolField(response.Raw, "result", "_meta", "io.modelcontextprotocol/related-task") {
		r.suspendState |= suspendTasks
	}
	if request.Method == "initialize" {
		if hasProtocolField(request.Raw, "params", "capabilities", "tasks") || hasProtocolField(response.Raw, "result", "capabilities", "tasks") {
			r.suspendState |= suspendTasks
		}
	}
	if request.Method == "tools/list" {
		var tools []json.RawMessage
		if raw, ok := wire.Get(response.Raw, "result", "tools"); ok && json.Unmarshal(raw, &tools) == nil {
			for _, tool := range tools {
				if raw, exists := wire.Get(tool, "execution", "taskSupport"); exists {
					var support string
					if json.Unmarshal(raw, &support) != nil || support != "forbidden" {
						r.suspendState |= suspendTasks
					}
				}
			}
		}
	}
	if protocol.Version(request) == protocol.ModernVersion || r.revision == protocol.ModernVersion {
		if protocol.IsInputRequired(response) || hasProtocolField(response.Raw, "result", "requestState") || hasProtocolField(response.Raw, "result", "inputRequests") {
			r.suspendState |= suspendContinuation
		}
		if raw, exists := wire.Get(response.Raw, "result", "resultType"); exists {
			var kind string
			if json.Unmarshal(raw, &kind) != nil || (kind != "complete" && kind != "input_required") {
				r.suspendState |= suspendUnknownResult
			}
		}
	}
}

// ObserveBackend records only unsolicited protocol obligations from the live
// generation. Call after boot validation; retired or late frames must not enter
// this ledger. Responses use ObserveResponse after the freshness gate instead.
func (r *Recovery) ObserveBackend(m wire.Message) {
	if m.Kind == wire.Response {
		return
	}
	if isTaskMethod(m.Method) || hasProtocolField(m.Raw, "params", "task") || hasProtocolField(m.Raw, "params", "_meta", "io.modelcontextprotocol/related-task") {
		r.suspendState |= suspendTasks
	}
	if m.Kind != wire.Notification {
		return
	}
	switch m.Method {
	case "notifications/message", "notifications/progress", protocol.CancellationMethod,
		"notifications/tools/list_changed", "notifications/resources/list_changed", "notifications/prompts/list_changed",
		"notifications/resources/updated", protocol.SubscriptionAcknowledged, "notifications/tasks/status":
		// Logs and directory invalidations do not themselves require residency.
		// Explicit subscriptions are tracked by the relay and recovery ledger.
	default:
		r.suspendState |= suspendUnknownServerNotification
	}
}

func isTaskMethod(method string) bool {
	return strings.HasPrefix(method, "tasks/") || strings.HasPrefix(method, "notifications/tasks/")
}

func hasProtocolField(raw []byte, path ...string) bool {
	_, exists := wire.Get(raw, path...)
	return exists
}
