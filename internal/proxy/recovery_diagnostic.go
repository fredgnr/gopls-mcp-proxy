package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fredgnr/gopls-mcp-proxy/internal/wire"
)

// RecoveryDiagnostic describes initialization recovery eligibility without
// retaining or logging extension values. An empty Reason means it is eligible;
// it does not promise that a replacement backend will have the same contract.
type RecoveryDiagnostic struct {
	RequestedVersion  string
	NegotiatedVersion string
	Side              string
	Path              string
	Reason            string
	ActualType        string
}

func (d RecoveryDiagnostic) String() string {
	return fmt.Sprintf("initialization recovery rejected: requested_version=%q negotiated_version=%q side=%q path=%q reason=%q actual_type=%q",
		d.RequestedVersion, d.NegotiatedVersion, d.Side, d.Path, d.Reason, d.ActualType)
}

// InitializationDiagnostic is available when a successful backend initialize
// response is recorded at the freshness gate. Per-call mode checks it before
// frontend delivery and rejects a known incompatible handshake.
func (r *Recovery) InitializationDiagnostic() (RecoveryDiagnostic, bool) {
	return r.initDiagnostic, len(r.initRequest) != 0
}

// Preflight rejects already-known recovery failures without starting a candidate
// process. The owner calls it only when replacing an initialized backend; an
// initial process launch needs no restoration. Recover repeats the same check.
func (r *Recovery) Preflight() error {
	if r.blocked != "" {
		return fmt.Errorf("%w: %s", ErrReconnectRequired, r.blocked)
	}
	if r.revision == "2026-07-28" && len(r.initRequest) == 0 {
		if len(r.logLevel) != 0 || len(r.subscribed) != 0 {
			return fmt.Errorf("%w: modern observed state requires a new frontend session", ErrReconnectRequired)
		}
		return nil
	}
	if len(r.initRequest) == 0 || !legacyRecoveryRevision(r.revision) || len(r.initialized) == 0 {
		return fmt.Errorf("%w: no completed, supported initialization to restore", ErrReconnectRequired)
	}
	return nil
}

func recoveryInitializeDiagnostic(request, response []byte) RecoveryDiagnostic {
	d := RecoveryDiagnostic{}
	d.RequestedVersion, _ = recoveryString(request, "params", "protocolVersion")
	d.NegotiatedVersion, _ = recoveryString(response, "result", "protocolVersion")
	if !d.onlyFields(request, "client", "", "jsonrpc", "id", "method", "params") ||
		!d.onlyFields(response, "server", "", "jsonrpc", "id", "result") {
		return d
	}
	for _, side := range []struct {
		raw   []byte
		name  string
		field string
		info  string
	}{
		{request, "client", "params", "clientInfo"},
		{response, "server", "result", "serverInfo"},
	} {
		object, exists := wire.Get(side.raw, side.field)
		if !exists {
			d.reject(side.name, side.field, "missing_required_field", "missing")
			return d
		}
		allowed := []string{"protocolVersion", "capabilities", side.info, "_meta"}
		if side.name == "server" {
			allowed = append(allowed, "instructions")
		}
		if !d.onlyFields(object, side.name, side.field, allowed...) {
			return d
		}
		if meta, exists := wire.Get(object, "_meta"); exists && !d.onlyFields(meta, side.name, side.field+"._meta", "progressToken") {
			if d.Reason == "no_recovery_rule" {
				d.Reason = "non_recoverable_metadata"
			}
			return d
		}
		if info, exists := wire.Get(object, side.info); exists && !d.implementationInfo(info, side.name, side.field+"."+side.info) {
			return d
		}
	}
	version, exists := wire.Get(response, "result", "protocolVersion")
	if !exists {
		d.reject("server", "result.protocolVersion", "missing_required_field", "missing")
		return d
	}
	if _, ok := recoveryString(response, "result", "protocolVersion"); !ok {
		d.reject("server", "result.protocolVersion", "invalid_type", recoveryJSONType(version))
		return d
	}
	if !legacyRecoveryRevision(d.NegotiatedVersion) {
		d.reject("server", "result.protocolVersion", "unsupported_protocol_version", "string")
		return d
	}
	for _, side := range []struct {
		raw    []byte
		name   string
		field  string
		server bool
	}{{request, "client", "params", false}, {response, "server", "result", true}} {
		capabilities, exists := wire.Get(side.raw, side.field, "capabilities")
		if exists && !recoveryCapabilityShape(d.NegotiatedVersion, side.server).diagnose(capabilities, side.name, side.field+".capabilities", &d) {
			return d
		}
	}
	return d
}

func (d *RecoveryDiagnostic) reject(side, path, reason, actualType string) bool {
	d.Side, d.Path, d.Reason, d.ActualType = side, path, reason, actualType
	return false
}

// Object traversal is sorted so the first reported path is deterministic even
// when several fields lack recovery rules. Only paths and types are retained.
func (d *RecoveryDiagnostic) onlyFields(raw []byte, side, path string, allowed ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return d.reject(side, path, "invalid_type", recoveryJSONType(raw))
	}
	for _, key := range sortedRecoveryKeys(object) {
		known := false
		for _, field := range allowed {
			if field == key {
				known = true
				break
			}
		}
		if !known {
			return d.reject(side, recoveryFieldPath(path, key), "no_recovery_rule", recoveryJSONType(object[key]))
		}
	}
	return true
}

func (d *RecoveryDiagnostic) implementationInfo(raw []byte, side, path string) bool {
	if !d.onlyFields(raw, side, path, "name", "version", "title", "description", "websiteUrl", "icons") {
		return false
	}
	if iconsRaw, exists := wire.Get(raw, "icons"); exists {
		var icons []json.RawMessage
		if json.Unmarshal(iconsRaw, &icons) != nil || bytes.Equal(bytes.TrimSpace(iconsRaw), []byte("null")) {
			return d.reject(side, path+".icons", "invalid_type", recoveryJSONType(iconsRaw))
		}
		for i, icon := range icons {
			if !d.onlyFields(icon, side, fmt.Sprintf("%s.icons[%d]", path, i), "src", "mimeType", "sizes", "theme") {
				return false
			}
		}
	}
	return true
}

func (shape recoveryCapabilityFields) diagnose(raw []byte, side, path string, d *RecoveryDiagnostic) bool {
	if shape == nil { // The only primitive capability members are booleans.
		trimmed := bytes.TrimSpace(raw)
		if bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false")) {
			return true
		}
		return d.reject(side, path, "invalid_type", recoveryJSONType(raw))
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return d.reject(side, path, "invalid_type", recoveryJSONType(raw))
	}
	for _, key := range sortedRecoveryKeys(object) {
		child, exists := shape[key]
		if !exists {
			reason := "no_recovery_rule"
			if len(shape) == 0 {
				reason = "nonempty_static_declaration"
			}
			return d.reject(side, recoveryFieldPath(path, key), reason, recoveryJSONType(object[key]))
		}
		if !child.diagnose(object[key], side, recoveryFieldPath(path, key), d) {
			return false
		}
	}
	return true
}

func recoveryFieldPath(parent, field string) string {
	// Bracket-quote unusual extension keys so dots, controls and brackets cannot
	// make a diagnostic path ambiguous or inject new log fields.
	if field == "" || strings.IndexFunc(field, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
	}) >= 0 {
		return fmt.Sprintf("%s[%q]", parent, field)
	}
	if parent == "" {
		return field
	}
	return parent + "." + field
}

func recoveryJSONType(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "missing"
	}
	switch raw[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}
