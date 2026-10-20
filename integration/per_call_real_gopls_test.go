package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The independent fixture establishes restart causation. This test additionally
// verifies native gopls results and session recovery across per-call restarts.
func TestRealGoplsPerCallFreshProcessAndFileChanges(t *testing.T) {
	testRealGoplsFileChanges(t, "per-call", `{"protocolVersion":"2025-11-25","clientInfo":{"name":"per-call-acceptance","version":"1"},"capabilities":{"roots":{"listChanged":true}}}`)
}

// Run the incident's known client shape through both refresh triggers. Native
// symbol results prove additions, renames, edits and deletions become visible.
func TestRealGoplsCodexRecovery(t *testing.T) {
	for _, mode := range []string{"per-call", "on-change"} {
		t.Run(mode, func(t *testing.T) {
			testRealGoplsFileChanges(t, mode, codexInitializeParams)
		})
	}
}

func testRealGoplsFileChanges(t *testing.T, mode, initializeParams string) {
	if os.Getenv("GOPLS_INTEGRATION") != "1" {
		t.Skip("set GOPLS_INTEGRATION=1; dedicated interoperability CI must enable this test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	gopls := os.Getenv("GOPLS_BIN")
	if gopls == "" {
		gopls = "gopls"
	}
	gopls, err := exec.LookPath(gopls)
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.CommandContext(ctx, gopls, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("gopls version: %v: %s", err, version)
	}
	requireGoplsVersion(t, version)
	t.Logf("%s", bytes.TrimSpace(version))
	root := t.TempDir()
	archiveEvidence(t, root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(workspace, "go.mod"), "module example.com/per-call-acceptance\n\ngo 1.25.0\n")
	mainFile := filepath.Join(workspace, "main.go")
	write(mainFile, "package fixture\nvar FixtureOriginalSymbol = 1\n")
	proxyPath := filepath.Join(root, "install", "gopls-mcp-proxy")
	backendPath := filepath.Join(filepath.Dir(proxyPath), "gopls")
	copyExecutable(t, proxyBinary(t), proxyPath)
	copyExecutable(t, gopls, backendPath)
	// Suppress asynchronous watcher/poll replacements so unchanged calls must
	// prove the mode itself starts a fresh process. Request scans stay enabled.
	cmd := exec.CommandContext(ctx, proxyPath, "--refresh-mode", mode, "--disable-watcher", "--poll-interval", "1h", "--debounce", "1ms")
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOENV=off")
	p := startPeer(t, cmd, filepath.Join(root, "per-call-gopls.log"))
	p.timeout = 95 * time.Second
	p.roots = []map[string]string{{"uri": (&url.URL{Scheme: "file", Path: filepath.ToSlash(workspace)}).String(), "name": "per-call workspace"}}
	proxyPID := p.cmd.Process.Pid
	call := func(id, method, params string) []byte {
		t.Helper()
		response := p.call(id, method, params)
		f, err := os.OpenFile(filepath.Join(root, "per-call-exchanges.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewEncoder(f).Encode(map[string]any{"proxy_pid": proxyPID, "id": json.RawMessage(id), "method": method, "params": json.RawMessage(params), "response": json.RawMessage(response)})
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("record exchange: %v / %v", err, closeErr)
		}
		return response
	}
	initialized := call("100", "initialize", initializeParams)
	if bytes.Contains(initialized, []byte(`"error"`)) {
		t.Fatalf("initialize: %s", initialized)
	}
	var requested struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	var negotiated struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(initializeParams), &requested) != nil || json.Unmarshal(initialized, &negotiated) != nil || negotiated.Result.ProtocolVersion != requested.ProtocolVersion {
		t.Fatalf("unexpected negotiated version for pinned gopls: request=%s response=%s", initializeParams, initialized)
	}
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	catalogue := call(`"catalogue"`, "tools/list", `{}`)
	if !bytes.Contains(catalogue, []byte(`"go_search"`)) || bytes.Contains(catalogue, []byte(`"error"`)) {
		t.Fatalf("native catalogue: %s", catalogue)
	}
	type backendIdentity struct {
		Message    string `json:"msg"`
		Boot       uint64 `json:"boot_id"`
		PID        int    `json:"pid"`
		Generation uint64 `json:"generation"`
	}
	published := func() []backendIdentity {
		t.Helper()
		logs, err := os.ReadFile(p.log)
		if err != nil {
			t.Fatal(err)
		}
		var identities []backendIdentity
		for _, line := range bytes.Split(logs, []byte{'\n'}) {
			var event backendIdentity
			if json.Unmarshal(line, &event) == nil && event.Message == "backend ready" {
				identities = append(identities, event)
			}
		}
		if len(identities) == 0 {
			t.Fatalf("no published backend identity: %s", logs)
		}
		return identities
	}
	previous := published()
	search := func(id, query, present, absent string, unchanged bool) {
		t.Helper()
		params, _ := json.Marshal(map[string]any{"name": "go_search", "arguments": map[string]string{"query": query}})
		raw := call(id, "tools/call", string(params))
		var reply struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatal(err)
		}
		if len(reply.Error) != 0 || reply.Result.IsError {
			t.Fatalf("go_search %s failed: %s", query, raw)
		}
		var text strings.Builder
		for _, content := range reply.Result.Content {
			text.WriteString(content.Text)
		}
		if present != "" && !strings.Contains(text.String(), present) {
			t.Fatalf("go_search %s missing %q: %s", query, present, raw)
		}
		if absent != "" && strings.Contains(text.String(), absent) {
			t.Fatalf("go_search %s retained %q: %s", query, absent, raw)
		}
		current := published()
		old, next := previous[len(previous)-1], current[len(current)-1]
		if mode == "per-call" || !unchanged {
			if len(current) <= len(previous) || old.Boot == next.Boot || old.PID == next.PID || next.Generation <= old.Generation {
				t.Fatalf("tool call reused backend after required refresh: before=%+v after=%+v", previous, current)
			}
			requireBackendExited(t, old.PID)
			if unchanged && len(current) != len(previous)+1 {
				t.Fatalf("unchanged per-call request must publish exactly one new backend: before=%+v after=%+v", previous, current)
			}
		} else if len(current) != len(previous) || old != next {
			t.Fatalf("on-change mode replaced an unchanged backend: before=%+v after=%+v", previous, current)
		}
		if p.cmd.Process.Pid != proxyPID {
			t.Fatal("frontend process changed")
		}
		previous = current
		t.Logf("mode=%s proxy_pid=%d boot_id=%d backend_pid=%d query=%s", mode, proxyPID, next.Boot, next.PID, query)
	}
	search("1", "FixtureOriginalSymbol", mainFile, "", true)
	search("2", "FixtureOriginalSymbol", mainFile, "", true)
	added := filepath.Join(workspace, "newdir", "added.go")
	write(added, "package added\nvar FixtureAddedSymbol = 2\n")
	search("3", "FixtureAddedSymbol", added, "", false)
	renamed := filepath.Join(filepath.Dir(added), "renamed.go")
	if err := os.Rename(added, renamed); err != nil {
		t.Fatal(err)
	}
	search("4", "FixtureAddedSymbol", renamed, added, false)
	info, err := os.Stat(mainFile)
	if err != nil {
		t.Fatal(err)
	}
	write(mainFile, "package fixture\nvar FixtureModifiedSymbol = 1\n")
	if err := os.Chtimes(mainFile, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	search("5", "FixtureModifiedSymbol", mainFile, "", false)
	search("6", "FixtureOriginalSymbol", "No symbols found.", mainFile, true)
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	search("7", "FixtureAddedSymbol", "No symbols found.", renamed, false)
	if after := call(`"catalogue"`, "tools/list", `{}`); !rawJSONEqual(catalogue, after) {
		t.Fatalf("native tools catalogue changed across per-call recovery:\nbefore=%s\nafter=%s", catalogue, after)
	}
	if got := published(); len(got) != len(previous) {
		t.Fatalf("tools/list triggered per-call replacement: before=%d after=%d", len(previous), len(got))
	}
	p.shutdown()
	logs, err := os.ReadFile(p.log)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logs, []byte(`"mode":"default-gopls"`)) || !bytes.Contains(logs, []byte(`"selection_source":"sibling"`)) || !bytes.Contains(logs, []byte(`"refresh_mode":"`+mode+`"`)) {
		t.Fatalf("missing default sibling/refresh mode launch evidence: %s", logs)
	}
	for _, identity := range published() {
		requireBackendExited(t, identity.PID)
	}
	t.Logf("mode=%s real gopls: %d tool calls, %d published backends, one frontend pid=%d", mode, 7, len(previous), proxyPID)
}
