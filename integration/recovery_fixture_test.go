package integration

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
)

// Reconstructed from Codex rust-v0.163.0 (83086188ae0614bd882818ff1aa23f3194962012),
// codex-rs/codex-mcp/src/rmcp_client.rs:mcp_initialize_request_params with
// AuthElicitation enabled. The experimental map is nonempty; codex/auth-change
// itself is the empty capability marker emitted by Codex. This is not a capture
// of the original failing connection or a live Codex test.
//
//go:embed testdata/codex-0.163.0-initialize.json
var codexInitializeFixture string

var codexInitializeParams = strings.TrimSpace(codexInitializeFixture)

func expectedGoplsVersion(t *testing.T) string {
	t.Helper()
	version := os.Getenv("GOPLS_EXPECTED_VERSION")
	if version == "" {
		version = "v0.24.0"
	}
	if version != "v0.23.0" && version != "v0.24.0" {
		t.Fatalf("unsupported GOPLS_EXPECTED_VERSION %q; use a pinned v0.23.0 or v0.24.0", version)
	}
	return "golang.org/x/tools/gopls " + version
}

func requireGoplsVersion(t *testing.T, output []byte) {
	t.Helper()
	if got, want := strings.TrimSpace(string(output)), expectedGoplsVersion(t); got != want {
		t.Fatalf("real gopls acceptance requires %s, got %s", want, got)
	}
}

func requireBackendExited(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err == nil && process.Signal(syscall.Signal(0)) == nil {
		t.Errorf("backend survived replacement or cleanup: pid=%d", pid)
	}
}

func requireRecoverableInitialization(t *testing.T, logPath string) {
	t.Helper()
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range bytes.Split(logs, []byte{'\n'}) {
		var event struct {
			Message     string `json:"msg"`
			Recoverable *bool  `json:"recoverable"`
		}
		if json.Unmarshal(line, &event) == nil && event.Message == "initialization recovery eligibility" {
			if event.Recoverable == nil || !*event.Recoverable {
				t.Fatalf("known Codex initialization rejected: %s", line)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("missing initialization recovery eligibility evidence: %s", logs)
	}
}
