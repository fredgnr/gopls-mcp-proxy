package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/fredgnr/gopls-mcp-proxy/internal/protocol"
	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

// ErrReconnectRequired means that replacing the backend cannot preserve the
// session the client has already observed. The stale backend must not resume.
var ErrReconnectRequired = errors.New("protocol_restart_required")

const maxRecoveryBytes = 64 << 20
const maxRecoveryPages = 4096
const maxRecoverySubscriptions = 4096

// Recovery contains only confirmed protocol state, never business call history.
// Its owner serializes Observe calls. Recover operates on a Clone while that
// owner continues routing the candidate's legitimate reverse requests.
type Recovery struct {
	revision       string
	initRequest    []byte
	initResult     []byte
	initDiagnostic RecoveryDiagnostic
	initialized    []byte
	logLevel       []byte
	subscribed     map[string][]byte
	directories    map[string]*directoryView
	blocked        string
	sequence       uint64
}

type directoryView struct {
	request  []byte
	pages    [][]byte
	cursors  map[string]bool
	next     string
	hasNext  bool
	complete bool
	invalid  bool
}

// SetRevision is used for the per-request protocol profile. A successful legacy
// initialize records the actual negotiated version through ObserveResponse.
func (r *Recovery) SetRevision(revision string) {
	if len(r.initRequest) == 0 {
		r.revision = revision
	}
}

func (r *Recovery) Revision() string { return r.revision }

func (r *Recovery) NeedsRecovery() bool {
	return r.revision != "" || len(r.initRequest) != 0
}

func (r *Recovery) Clone() *Recovery {
	c := *r
	c.initRequest = bytes.Clone(r.initRequest)
	c.initResult = bytes.Clone(r.initResult)
	c.initialized = bytes.Clone(r.initialized)
	c.logLevel = bytes.Clone(r.logLevel)
	c.subscribed = make(map[string][]byte, len(r.subscribed))
	for uri, request := range r.subscribed {
		c.subscribed[uri] = bytes.Clone(request)
	}
	c.directories = make(map[string]*directoryView, len(r.directories))
	for method, view := range r.directories {
		v := *view
		v.request = bytes.Clone(view.request)
		v.pages = make([][]byte, len(view.pages))
		for i, page := range view.pages {
			v.pages[i] = bytes.Clone(page)
		}
		v.cursors = make(map[string]bool, len(view.cursors))
		for cursor, present := range view.cursors {
			v.cursors[cursor] = present
		}
		c.directories[method] = &v
	}
	return &c
}

// ObserveRequest is called after a client notification has been sent. Requests
// that change state are recorded only when their successful response was
// delivered, so a failed, cancelled or undelivered update cannot be resurrected.
func (r *Recovery) ObserveRequest(m wire.Message) {
	if m.Method != "initialize" {
		if revision := protocol.Version(m); revision != "" {
			r.SetRevision(revision)
		}
	}
	if m.Method == "notifications/initialized" {
		r.initialized = bytes.Clone(m.Raw)
		if !recoveryOnlyFields(m.Raw, "jsonrpc", "method", "params") {
			r.blocked = "initialized notification contains an unknown state extension"
		}
		if params, ok := wire.Get(m.Raw, "params"); ok && (!recoveryOnlyFields(params, "_meta") || !emptyRecoveryMetadata(params)) {
			r.blocked = "initialized notification contains an unknown state extension"
		}
		r.boundState()
	}
}

// MarkUncertain records a possibly dispatched protocol state change whose
// outcome cannot be confirmed to the frontend (for example cancellation,
// timeout, partial write or a stale suppressed success). The caller must not
// invoke it for an unsent queue item or a delivered explicit upstream error.
// Business calls and opaque business handles never enter the recovery ledger.
func (r *Recovery) MarkUncertain(request wire.Message) {
	switch request.Method {
	case "initialize", "notifications/initialized", "logging/setLevel", "resources/subscribe", "resources/unsubscribe":
		if r.blocked == "" {
			r.blocked = "unconfirmed dispatched protocol state change: " + request.Method
		}
	}
}

// ObserveResponse must be called in actual client-delivery order. In particular,
// an obsolete backend response suppressed by the freshness gate is not evidence
// of client-observed state.
func (r *Recovery) ObserveResponse(request, response wire.Message) {
	if r.blocked != "" {
		return
	}
	if _, failed := wire.Get(response.Raw, "error"); failed {
		return
	}
	if _, success := wire.Get(response.Raw, "result"); !success {
		return
	}
	switch request.Method {
	case "initialize":
		if len(r.initRequest) != 0 {
			r.blocked = "multiple initialize exchanges in one frontend session"
			return
		}
		r.initRequest = bytes.Clone(request.Raw)
		r.initResult, _ = recoveryResponseSnapshot(response.Raw, false)
		version, ok := recoveryString(response.Raw, "result", "protocolVersion")
		if !ok {
			r.blocked = "initialize response omitted its negotiated protocol version"
		} else {
			r.revision = version
		}
		r.initDiagnostic = recoveryInitializeDiagnostic(request.Raw, response.Raw)
		if r.initDiagnostic.Reason != "" {
			r.blocked = r.initDiagnostic.String()
		}
	case "logging/setLevel":
		if !replayableSetting(request.Raw, "level") {
			r.blocked = "logging configuration contains an unknown state extension"
		}
		r.logLevel = bytes.Clone(request.Raw)
	case "resources/subscribe", "resources/unsubscribe":
		uri, ok := recoveryString(request.Raw, "params", "uri")
		if !ok || !replayableSetting(request.Raw, "uri") {
			r.blocked = "resource subscription contains an unknown state extension"
			return
		}
		if r.subscribed == nil {
			r.subscribed = make(map[string][]byte)
		}
		if request.Method == "resources/unsubscribe" {
			delete(r.subscribed, uri)
		} else {
			r.subscribed[uri] = bytes.Clone(request.Raw)
		}
	default:
		if _, known := directoryKinds[request.Method]; known {
			r.observeDirectory(request.Raw, response.Raw, request.Method)
		}
	}
	r.boundState()
}

type directoryKind struct{ field, capability, notification string }

var directoryKinds = map[string]directoryKind{
	"tools/list":               {"tools", "tools", "notifications/tools/list_changed"},
	"resources/list":           {"resources", "resources", "notifications/resources/list_changed"},
	"resources/templates/list": {"resourceTemplates", "resources", "notifications/resources/list_changed"},
	"prompts/list":             {"prompts", "prompts", "notifications/prompts/list_changed"},
}

func (r *Recovery) observeDirectory(request, response []byte, method string) {
	if r.blocked != "" {
		return
	}
	if r.directories == nil {
		r.directories = make(map[string]*directoryView)
	}
	cursorRaw, hasCursor := wire.Get(request, "params", "cursor")
	cursor := ""
	if hasCursor && (len(cursorRaw) == 0 || cursorRaw[0] != '"' || json.Unmarshal(cursorRaw, &cursor) != nil) {
		r.directories[method] = &directoryView{invalid: true}
		return
	}
	view := r.directories[method]
	if !hasCursor {
		if view != nil && (!view.complete || view.invalid) {
			// A second traversal cannot prove what happened to the first
			// client-delivered partial view. Do not silently substitute a
			// convenient later complete snapshot for that earlier evidence.
			view.invalid = true
			return
		}
		view = &directoryView{request: bytes.Clone(request), cursors: make(map[string]bool)}
		r.directories[method] = view
	} else if view == nil || view.invalid || !view.hasNext || view.next != cursor || view.cursors[cursor] {
		r.directories[method] = &directoryView{invalid: true}
		return
	} else {
		view.cursors[cursor] = true
		// Pagination may change only its standard cursor and request ID. A new
		// filter, metadata or extension would describe a different client view.
		start, err1 := recoveryRequestSnapshot(view.request)
		current, err2 := recoveryRequestSnapshot(request)
		if err1 != nil || err2 != nil || !bytes.Equal(start, current) {
			view.invalid = true
			return
		}
	}
	page, next, hasNext, err := directoryPage(response, directoryKinds[method].field)
	if err != nil || len(view.pages) >= maxRecoveryPages || (hasNext && (view.cursors[next] || (hasCursor && next == cursor))) {
		view.invalid = true
		return
	}
	view.pages = append(view.pages, page)
	view.next, view.hasNext, view.complete = next, hasNext, !hasNext
}

// Recover performs only the explicit protocol recovery whitelist. exchange must
// correlate private IDs with this physical candidate and suppress its responses
// and progress; it must still route legitimate reverse requests to the client.
// send transmits initialized to the candidate. notify delivers legal invalidation
// notifications to the client. All callbacks must obey ctx and close blocked I/O.
func (r *Recovery) Recover(ctx context.Context, exchange func(context.Context, []byte) ([]byte, error), send func(context.Context, []byte) error, notify func([]byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Preflight(); err != nil {
		return err
	}
	if r.revision == "2026-07-28" && len(r.initRequest) == 0 {
		// Modern directory notifications require a live notification subscription,
		// which is ended at the process boundary by the router. No old metadata or
		// subscription may be synthesized here. Directory comparison retains the
		// metadata of each actual delivered enumeration; it is not a default
		// identity or capability snapshot for future client requests.
		for _, method := range sortedRecoveryKeys(r.directories) {
			if err := r.compareDirectory(ctx, method, exchange); err != nil {
				return err
			}
		}
		return nil
	}
	request, err := wire.Replace(r.initRequest, mustRecoveryJSON(r.revision), "params", "protocolVersion")
	if err != nil {
		return fmt.Errorf("%w: invalid saved initialization: %v", ErrReconnectRequired, err)
	}
	response, err := r.exchange(ctx, request, exchange)
	if err != nil {
		return err
	}
	snapshot, err := recoveryResponseSnapshot(response, false)
	if err != nil || !bytes.Equal(snapshot, r.initResult) {
		return fmt.Errorf("%w: initialization version, capabilities, identity or information changed", ErrReconnectRequired)
	}
	if err := send(ctx, r.initialized); err != nil {
		return fmt.Errorf("%w: initialized delivery: %v", ErrReconnectRequired, err)
	}
	if len(r.logLevel) != 0 {
		if _, ok := wire.Get(r.initResult, "result", "capabilities", "logging"); !ok {
			return fmt.Errorf("%w: logging configuration lacks an advertised capability", ErrReconnectRequired)
		}
		if _, err := r.exchange(ctx, r.logLevel, exchange); err != nil {
			return err
		}
	}
	for _, uri := range sortedRecoveryKeys(r.subscribed) {
		allowed, ok := wire.Get(r.initResult, "result", "capabilities", "resources", "subscribe")
		if !ok || !bytes.Equal(allowed, []byte("true")) {
			return fmt.Errorf("%w: resource invalidation is not supported", ErrReconnectRequired)
		}
		if _, err := r.exchange(ctx, r.subscribed[uri], exchange); err != nil {
			return err
		}
	}
	// Validate every unnotifiable directory before releasing any invalidations.
	notifications := make(map[string]bool)
	for _, method := range sortedRecoveryKeys(r.directories) {
		kind := directoryKinds[method]
		if r.listChanged(kind.capability) {
			notifications[kind.notification] = true
			continue
		}
		if err := r.compareDirectory(ctx, method, exchange); err != nil {
			return err
		}
	}
	for _, method := range sortedRecoveryKeys(notifications) {
		if err := notify(mustRecoveryJSON(map[string]any{"jsonrpc": "2.0", "method": method})); err != nil {
			return err
		}
	}
	for _, uri := range sortedRecoveryKeys(r.subscribed) {
		if err := notify(mustRecoveryJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/resources/updated", "params": map[string]any{"uri": uri}})); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recovery) exchange(ctx context.Context, request []byte, exchange func(context.Context, []byte) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.sequence++
	id := mustRecoveryJSON(fmt.Sprintf("__proxy_recovery_%d", r.sequence))
	request, err := wire.Replace(request, id, "id")
	if err != nil {
		return nil, fmt.Errorf("%w: malformed recovery request: %v", ErrReconnectRequired, err)
	}
	response, err := exchange(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("%w: recovery exchange: %v", ErrReconnectRequired, err)
	}
	message, err := wire.ParseMessage(response)
	if err != nil || message.Kind != wire.Response {
		return nil, fmt.Errorf("%w: invalid recovery response", ErrReconnectRequired)
	}
	if _, failed := wire.Get(response, "error"); failed {
		return nil, fmt.Errorf("%w: upstream rejected protocol state restoration", ErrReconnectRequired)
	}
	return response, nil
}

func (r *Recovery) compareDirectory(ctx context.Context, method string, exchange func(context.Context, []byte) ([]byte, error)) error {
	view := r.directories[method]
	if view.invalid || !view.complete || len(view.pages) == 0 {
		return fmt.Errorf("%w: %s lacks a complete client-delivered directory view", ErrReconnectRequired, method)
	}
	request := bytes.Clone(view.request)
	seen := make(map[string]bool)
	for i := 0; i < maxRecoveryPages; i++ {
		response, err := r.exchange(ctx, request, exchange)
		if err != nil {
			return err
		}
		page, next, hasNext, err := directoryPage(response, directoryKinds[method].field)
		if err != nil || i >= len(view.pages) || !bytes.Equal(page, view.pages[i]) {
			return fmt.Errorf("%w: %s directory changed or cannot be compared", ErrReconnectRequired, method)
		}
		if !hasNext {
			if i+1 != len(view.pages) {
				return fmt.Errorf("%w: %s directory lost pages", ErrReconnectRequired, method)
			}
			return nil
		}
		if seen[next] {
			return fmt.Errorf("%w: %s repeated pagination cursor", ErrReconnectRequired, method)
		}
		seen[next] = true
		request, err = recoveryCursor(view.request, next)
		if err != nil {
			return fmt.Errorf("%w: %s pagination: %v", ErrReconnectRequired, method, err)
		}
	}
	return fmt.Errorf("%w: %s pagination budget exceeded", ErrReconnectRequired, method)
}

func (r *Recovery) listChanged(capability string) bool {
	v, ok := wire.Get(r.initResult, "result", "capabilities", capability, "listChanged")
	return ok && bytes.Equal(v, []byte("true"))
}

func legacyRecoveryRevision(revision string) bool {
	switch revision {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return true
	}
	return false
}

// These are recovery allowlists, not validators of normal traffic. Unknown
// capabilities remain byte-preserving on the initial exchange, but cannot be
// presumed safe to replay as session state. Known opaque capability objects
// have no defined nested fields and therefore require an empty object here.
// Shapes follow the versioned official schema.ts Client/ServerCapabilities.
type recoveryCapabilityFields map[string]recoveryCapabilityFields

func recoveryCapabilityShape(revision string, server bool) recoveryCapabilityFields {
	empty := recoveryCapabilityFields{}
	shape := recoveryCapabilityFields{"experimental": empty}
	if server {
		shape["logging"] = empty
		shape["prompts"] = recoveryCapabilityFields{"listChanged": nil}
		shape["resources"] = recoveryCapabilityFields{"listChanged": nil, "subscribe": nil}
		shape["tools"] = recoveryCapabilityFields{"listChanged": nil}
		if revision != "2024-11-05" {
			shape["completions"] = empty
		}
	} else {
		shape["roots"] = recoveryCapabilityFields{"listChanged": nil}
		shape["sampling"] = empty
		if revision == "2025-06-18" || revision == "2025-11-25" {
			// Known static form/url declarations are also sent with June
			// negotiation (for example by Codex). Replaying them verbatim
			// does not upgrade the session or restore an elicitation call.
			shape["elicitation"] = recoveryCapabilityFields{"form": empty, "url": empty}
		}
	}
	if revision == "2025-11-25" {
		var requests recoveryCapabilityFields
		if server {
			requests = recoveryCapabilityFields{"tools": {"call": empty}}
		} else {
			shape["sampling"] = recoveryCapabilityFields{"context": empty, "tools": empty}
			requests = recoveryCapabilityFields{"sampling": {"createMessage": empty}, "elicitation": {"create": empty}}
		}
		shape["tasks"] = recoveryCapabilityFields{"list": empty, "cancel": empty, "requests": requests}
	}
	return shape
}

func replayableSetting(request []byte, field string) bool {
	params, ok := wire.Get(request, "params")
	if !ok || !recoveryOnlyFields(params, field, "_meta") || !emptyRecoveryMetadata(params) {
		return false
	}
	_, ok = recoveryString(request, "params", field)
	return ok && recoveryOnlyFields(request, "jsonrpc", "id", "method", "params")
}

func emptyRecoveryMetadata(raw []byte) bool {
	meta, ok := wire.Get(raw, "_meta")
	if !ok {
		return true
	}
	// A progress token is a standard association, isolated by the exchange
	// callback. Unspecified extension state is not safe to replay.
	return recoveryOnlyFields(meta, "progressToken")
}

func recoveryOnlyFields(raw []byte, allowed ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for _, key := range allowed {
		delete(fields, key)
	}
	return len(fields) == 0
}

func recoveryString(raw []byte, path ...string) (string, bool) {
	v, ok := wire.Get(raw, path...)
	var s string
	if !ok || json.Unmarshal(v, &s) != nil || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return "", false
	}
	return s, true
}

func canonicalRecoveryJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func recoveryResponseSnapshot(raw []byte, directory bool) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return nil, errors.New("invalid response object")
	}
	delete(envelope, "id")
	if directory {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(envelope["result"], &result); err != nil || result == nil {
			return nil, errors.New("invalid directory result")
		}
		delete(result, "nextCursor")
		envelope["result"] = mustRecoveryJSON(result)
	}
	return canonicalRecoveryJSON(mustRecoveryJSON(envelope))
}

func recoveryRequestSnapshot(raw []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	delete(envelope, "id")
	if paramsRaw, ok := envelope["params"]; ok {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(paramsRaw, &params); err != nil || params == nil {
			return nil, errors.New("invalid pagination params")
		}
		delete(params, "cursor")
		if len(params) == 0 {
			delete(envelope, "params")
		} else {
			envelope["params"] = mustRecoveryJSON(params)
		}
	}
	return canonicalRecoveryJSON(mustRecoveryJSON(envelope))
}

func directoryPage(raw []byte, field string) ([]byte, string, bool, error) {
	items, ok := wire.Get(raw, "result", field)
	if !ok || len(bytes.TrimSpace(items)) == 0 || bytes.TrimSpace(items)[0] != '[' {
		return nil, "", false, errors.New("directory result omitted its item array")
	}
	nextRaw, hasNext := wire.Get(raw, "result", "nextCursor")
	var next string
	if hasNext && (len(nextRaw) == 0 || nextRaw[0] != '"' || json.Unmarshal(nextRaw, &next) != nil) {
		return nil, "", false, errors.New("invalid directory cursor")
	}
	snapshot, err := recoveryResponseSnapshot(raw, true)
	return snapshot, next, hasNext, err
}

func recoveryCursor(raw []byte, cursor string) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	params := make(map[string]json.RawMessage)
	if existing, ok := envelope["params"]; ok {
		if err := json.Unmarshal(existing, &params); err != nil || params == nil {
			return nil, errors.New("invalid directory params")
		}
	}
	params["cursor"] = mustRecoveryJSON(cursor)
	envelope["params"] = mustRecoveryJSON(params)
	return json.Marshal(envelope)
}

func (r *Recovery) boundState() {
	bytesUsed := len(r.initRequest) + len(r.initResult) + len(r.initialized) + len(r.logLevel)
	for _, request := range r.subscribed {
		bytesUsed += len(request)
	}
	for _, view := range r.directories {
		bytesUsed += len(view.request)
		for _, page := range view.pages {
			bytesUsed += len(page)
		}
	}
	if bytesUsed > maxRecoveryBytes || len(r.subscribed) > maxRecoverySubscriptions {
		r.blocked = "confirmed protocol state exceeds recovery memory budget"
		r.subscribed = nil
		r.directories = nil
		r.logLevel = nil
	}
}

func mustRecoveryJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func sortedRecoveryKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
