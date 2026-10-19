// Package protocol contains revision-specific MCP associations, not tool or
// application semantics. It never converts one protocol revision into another.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

// These names and message shapes are fixed by the 2026-07-28 specification:
// https://modelcontextprotocol.io/specification/2026-07-28/basic/index
// https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/subscriptions
// https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/cancellation
const (
	ModernVersion            = "2026-07-28"
	VersionKey               = "io.modelcontextprotocol/protocolVersion"
	ClientCapabilitiesKey    = "io.modelcontextprotocol/clientCapabilities"
	ClientInfoKey            = "io.modelcontextprotocol/clientInfo"
	SubscriptionIDKey        = "io.modelcontextprotocol/subscriptionId"
	SubscriptionMethod       = "subscriptions/listen"
	SubscriptionAcknowledged = "notifications/subscriptions/acknowledged"
	CancellationMethod       = "notifications/cancelled"
	// Application codes are outside JSON-RPC's reserved [-32768,-32000]
	// range, including the modern MCP-reserved [-32099,-32020] range.
	ProxyErrorCode = 1000
)

type Profile struct {
	Version            string
	Modern             bool
	Batch              bool
	RequiresInitialize bool
	SessionUniqueIDs   bool
}

func Lookup(version string) (Profile, bool) {
	switch version {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return Profile{Version: version, Batch: version == "2025-03-26", RequiresInitialize: true, SessionUniqueIDs: true}, true
	case ModernVersion:
		return Profile{Version: version, Modern: true}, true
	default:
		return Profile{Version: version}, false
	}
}

// Version reads the revision from this particular message. Modern capabilities
// and client identity must never be inferred from a previous request.
func Version(m wire.Message) string {
	if m.Method == "initialize" {
		return String(m.Raw, "params", "protocolVersion")
	}
	return String(m.Raw, "params", "_meta", VersionKey)
}

func String(raw []byte, path ...string) string {
	v, ok := wire.Get(raw, path...)
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

func ValidateFrame(profile Profile, frame wire.Frame) error {
	if frame.Batch && !profile.Batch {
		return fmt.Errorf("batch is not supported by MCP %s", profile.Version)
	}
	return nil
}

// ValidateModernMetadata is available for local control traffic. Normal peer
// requests should retain their native upstream validation and errors.
func ValidateModernMetadata(m wire.Message) error {
	if Version(m) == "" {
		return errors.New("modern request is missing protocolVersion metadata")
	}
	capabilities, ok := wire.Get(m.Raw, "params", "_meta", ClientCapabilitiesKey)
	if !ok || len(capabilities) == 0 || capabilities[0] != '{' {
		return errors.New("modern request is missing clientCapabilities metadata")
	}
	return nil
}

func CancellationID(m wire.Message) (wire.ID, bool) {
	if m.Kind != wire.Notification || m.Method != CancellationMethod {
		return wire.ID{}, false
	}
	return idAt(m.Raw, "params", "requestId")
}

func ProgressToken(m wire.Message) (wire.ID, bool) {
	if m.Method == "notifications/progress" {
		return idAt(m.Raw, "params", "progressToken")
	}
	if m.Kind == wire.Request {
		return idAt(m.Raw, "params", "_meta", "progressToken")
	}
	return wire.ID{}, false
}

func SubscriptionID(m wire.Message) (wire.ID, bool) {
	if m.Kind == wire.Response {
		return idAt(m.Raw, "result", "_meta", SubscriptionIDKey)
	}
	return idAt(m.Raw, "params", "_meta", SubscriptionIDKey)
}

func idAt(raw []byte, path ...string) (wire.ID, bool) {
	v, ok := wire.Get(raw, path...)
	if !ok {
		return wire.ID{}, false
	}
	id, err := wire.ParseID(v)
	return id, err == nil
}

func IsSubscription(m wire.Message) bool {
	return m.Kind == wire.Request && m.Method == SubscriptionMethod && Version(m) == ModernVersion
}

// An acknowledgment is a notification, never a final response to listen.
func IsSubscriptionAck(m wire.Message) bool {
	_, valid := SubscriptionID(m)
	return m.Kind == wire.Notification && m.Method == SubscriptionAcknowledged && valid
}

func IsInputRequired(m wire.Message) bool {
	return m.Kind == wire.Response && String(m.Raw, "result", "resultType") == "input_required"
}

// Clients stop waiting on cancellation and discard late results. The proxy
// consistently follows the revisions' recommendation not to create a response
// after a request has been cancelled.
func CancelledResponseAllowed(Profile) bool { return false }

func Cancellation(id wire.ID, reason string) []byte {
	return marshal(map[string]any{"jsonrpc": "2.0", "method": CancellationMethod,
		"params": map[string]any{"requestId": id.Raw, "reason": reason}})
}

// SubscriptionCompletion produces the exact successful terminal result for a
// modern listen request. The caller owns at-most-once lifecycle accounting.
func SubscriptionCompletion(id wire.ID) []byte {
	return marshal(map[string]any{"jsonrpc": "2.0", "id": id.Raw, "result": map[string]any{
		"resultType": "complete", "_meta": map[string]any{SubscriptionIDKey: id.Raw}}})
}

type ErrorData struct {
	Source                  string `json:"source"`
	Reason                  string `json:"reason"`
	Generation              uint64 `json:"generation"`
	RequestDispatched       bool   `json:"requestDispatched"`
	BackendResponseReceived bool   `json:"backendResponseReceived"`
	SideEffects             string `json:"sideEffects"`
	AutomaticReplay         bool   `json:"automaticReplay"`
}

// ErrorResponse is exclusively for proxy-originated failures. Upstream errors
// must be forwarded as raw messages instead of being passed through this helper.
func ErrorResponse(id wire.ID, reason string, generation uint64, dispatched, received bool) []byte {
	message := "gopls-mcp-proxy: " + reason + "; request was not dispatched"
	sideEffects := "none"
	if dispatched {
		message = "gopls-mcp-proxy: " + reason + "; request may have executed; side effects are unknown; automatic replay is disabled"
		sideEffects = "unknown"
	}
	data := ErrorData{Source: "gopls-mcp-proxy", Reason: reason, Generation: generation,
		RequestDispatched: dispatched, BackendResponseReceived: received, SideEffects: sideEffects}
	return marshal(map[string]any{"jsonrpc": "2.0", "id": id.Raw, "error": map[string]any{
		"code": ProxyErrorCode, "message": message, "data": data}})
}

func marshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("invalid internal protocol value: " + err.Error())
	}
	return raw
}
