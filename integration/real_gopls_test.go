package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The mandatory raw helper suite establishes causation without a self-refreshing
// backend. This separately checks pinned real gopls interoperability on CI.
func TestRealGoplsTwoProtobufUpgrades(t *testing.T) {
	if os.Getenv("GOPLS_INTEGRATION") != "1" {
		t.Skip("set GOPLS_INTEGRATION=1; dedicated interoperability CI must enable this test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	gopls := os.Getenv("GOPLS_BIN")
	if gopls == "" {
		gopls = "gopls"
	}
	var err error
	gopls, err = exec.LookPath(gopls)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"go", "version"}, {gopls, "version"}} {
		output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("version evidence: %v: %s", err, output)
		}
		t.Logf("%s", bytes.TrimSpace(output))
	}
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
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	goFile := filepath.Join(workspace, "main.go")
	write(goFile, "package fixture\nimport \"google.golang.org/protobuf/proto\"\nvar Marshal = proto.Marshal\n")
	write(filepath.Join(workspace, "go.mod"), "module example.com/proxy-acceptance\n\ngo 1.25.0\n")
	setVersion := func(version string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", "get", "google.golang.org/protobuf@"+version)
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go get %s: %v\n%s", version, err, output)
		}
	}
	setVersion("v1.34.2")
	startGopls := func(proxied bool) *rawPeer {
		t.Helper()
		var cmd *exec.Cmd
		name := "direct.log"
		if proxied {
			name = "proxy.log"
			cmd = exec.CommandContext(ctx, proxyBinary(t), "--disable-watcher", "--poll-interval", "1h", "--debounce", "50ms", "--startup-timeout", "45s", "--request-timeout", "90s", "--", gopls, "mcp")
		} else {
			cmd = exec.CommandContext(ctx, gopls, "mcp")
		}
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "GOWORK=off")
		p := startPeer(t, cmd, filepath.Join(root, name))
		p.timeout = 95 * time.Second
		p.roots = []map[string]string{{"uri": (&url.URL{Scheme: "file", Path: filepath.ToSlash(workspace)}).String(), "name": "actual client workspace"}}
		reply := p.call("100", "initialize", `{"protocolVersion":"2025-11-25","clientInfo":{"name":"raw-gopls-acceptance","version":"1"},"capabilities":{"roots":{"listChanged":true}}}`)
		if bytes.Contains(reply, []byte(`"error"`)) {
			t.Fatalf("initialize: %s", reply)
		}
		p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		return p
	}
	p := startGopls(true)
	proxyPID := p.cmd.Process.Pid
	// Compare a complete, unchanged tools directory from an independent raw client.
	direct := startGopls(false)
	directTools := direct.call(`"catalog"`, "tools/list", `{}`)
	proxyTools := p.call(`"catalog"`, "tools/list", `{}`)
	if !rawJSONEqual(directTools, proxyTools) {
		t.Fatalf("native tools directory differs:\ndirect=%s\nproxy=%s", directTools, proxyTools)
	}
	_ = direct.in.Close()
	callTool := func(id, name string, args map[string]string) string {
		t.Helper()
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
		raw := p.call(id, "tools/call", string(params))
		var msg struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatal(err)
		}
		if len(msg.Error) > 0 || msg.Result.IsError {
			t.Fatalf("native %s failed: %s", name, raw)
		}
		var text strings.Builder
		for _, c := range msg.Result.Content {
			text.WriteString(c.Text)
		}
		return text.String()
	}
	generation := func() int { logs, _ := os.ReadFile(p.log); return strings.Count(string(logs), `"msg":"backend ready"`) }
	previousGeneration := 0
	for i, version := range []string{"v1.34.2", "v1.35.2", "v1.36.6"} {
		if i > 0 {
			setVersion(version)
		}
		text := callTool(fmt.Sprint(i+1), "go_file_context", map[string]string{"file": goFile})
		if !strings.Contains(text, "protobuf@"+version+"/") {
			t.Fatalf("resolved wrong protobuf version %s: %s", version, text)
		}
		current := generation()
		if current <= previousGeneration {
			logs, _ := os.ReadFile(p.log)
			t.Fatalf("upgrade did not publish new backend: %s", logs)
		}
		if p.cmd.Process.Pid != proxyPID {
			t.Fatal("proxy PID changed")
		}
		previousGeneration = current
		t.Logf("VAL-GOPLS-001 proxy_pid=%d published_generations=%d protobuf=%s", proxyPID, current, version)
	}
	// Ordinary source edits now require a proxy-owned replacement. No assertion
	// relies on gopls' own watcher or keeps generation unchanged.
	verifyChange := func(id, query, want string) {
		t.Helper()
		text := callTool(id, "go_search", map[string]string{"query": query})
		if !strings.Contains(text, want) {
			t.Fatalf("source result missing %q: %s", want, text)
		}
		current := generation()
		if current <= previousGeneration {
			logs, _ := os.ReadFile(p.log)
			t.Fatalf("source change failed to replace backend: %s", logs)
		}
		previousGeneration = current
		t.Logf("VAL-GOPLS-002 proxy_pid=%d published_generations=%d query=%s", proxyPID, current, query)
	}
	write(goFile, "package fixture\nimport \"google.golang.org/protobuf/proto\"\nvar Unmarshal = proto.Unmarshal\n")
	verifyChange("10", "Unmarshal", goFile)
	added := filepath.Join(workspace, "added.go")
	write(added, "package fixture\nvar FixtureAddedSymbol = 7\n")
	verifyChange("11", "FixtureAddedSymbol", added)
	if err := os.Remove(added); err != nil {
		t.Fatal(err)
	}
	verifyChange("12", "Unmarshal", goFile)
	temporary := filepath.Join(workspace, "save.tmp")
	write(temporary, "package fixture\nimport \"google.golang.org/protobuf/proto\"\nvar FixtureAtomicMarshal = proto.Marshal\n")
	if err := os.Rename(temporary, goFile); err != nil {
		t.Fatal(err)
	}
	verifyChange("13", "FixtureAtomicMarshal", goFile)
	logs, _ := os.ReadFile(p.log)
	seenBoots := map[uint64]bool{}
	lastGeneration := uint64(0)
	for _, line := range bytes.Split(logs, []byte{'\n'}) {
		var event struct {
			Message    string `json:"msg"`
			Generation uint64 `json:"generation"`
			Boot       uint64 `json:"boot_id"`
			PID        int    `json:"pid"`
		}
		if json.Unmarshal(line, &event) != nil || event.Message != "backend ready" {
			continue
		}
		if event.Boot == 0 || event.PID <= 0 || seenBoots[event.Boot] || event.Generation <= lastGeneration {
			t.Fatalf("invalid published backend identity: %s", line)
		}
		seenBoots[event.Boot] = true
		lastGeneration = event.Generation
	}
	if len(seenBoots) != 7 {
		t.Fatalf("want seven published boot identities, got %d", len(seenBoots))
	}
	t.Logf("real gopls evidence:\n%s", logs)
}

func rawJSONEqual(a, b []byte) bool {
	var aa, bb any
	ad := json.NewDecoder(bytes.NewReader(a))
	ad.UseNumber()
	bd := json.NewDecoder(bytes.NewReader(b))
	bd.UseNumber()
	if ad.Decode(&aa) != nil || bd.Decode(&bb) != nil {
		return false
	}
	ac, _ := json.Marshal(aa)
	bc, _ := json.Marshal(bb)
	return bytes.Equal(ac, bc)
}
