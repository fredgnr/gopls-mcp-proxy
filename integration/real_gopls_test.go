package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This is deliberately opt-in: it needs a real gopls and Go module downloads.
// CI runs it on both Linux and macOS with a pinned gopls version.
func TestRealGoplsTwoProtobufUpgrades(t *testing.T) {
	if os.Getenv("GOPLS_INTEGRATION") != "1" {
		t.Skip("set GOPLS_INTEGRATION=1 and install gopls to run the real process acceptance test")
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
	root := t.TempDir()
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "gopls-mcp-proxy")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "../cmd/gopls-mcp-proxy")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
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
	logPath := filepath.Join(root, "proxy.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(bin, "--gopls", gopls, "--workspace", workspace, "--debounce", "100ms", "--poll-interval", "200ms", "--startup-timeout", "45s", "--request-timeout", "60s")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stderr = logFile
	client := mcp.NewClient(&mcp.Implementation{Name: "acceptance-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		logs, _ := os.ReadFile(logPath)
		t.Fatalf("connect: %v\n%s", err, logs)
	}
	defer session.Close()
	pid := cmd.Process.Pid
	previousGenerationCount := 0
	for i, version := range []string{"v1.34.2", "v1.35.2", "v1.36.6"} {
		if i > 0 {
			setVersion(version)
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "go_file_context", Arguments: map[string]any{"file": goFile}})
		var text strings.Builder
		if res != nil {
			for _, c := range res.Content {
				if c, ok := c.(*mcp.TextContent); ok {
					text.WriteString(c.Text)
				}
			}
		}
		if err != nil || res == nil || res.IsError || !strings.Contains(text.String(), "protobuf@"+version+"/") {
			logs, _ := os.ReadFile(logPath)
			t.Fatalf("version %s: err=%v result=%s\n%s", version, err, text.String(), logs)
		}
		if cmd.Process.Pid != pid {
			t.Fatal("upstream proxy process changed")
		}
		if err := session.Ping(ctx, nil); err != nil {
			t.Fatal(err)
		}
		logs, _ := os.ReadFile(logPath)
		count := strings.Count(string(logs), `"msg":"backend ready"`)
		if count <= previousGenerationCount {
			t.Fatalf("upgrade did not publish a new generation: %s", logs)
		}
		previousGenerationCount = count
		t.Logf("same MCP session, proxy pid=%d, resolved protobuf@%s", pid, version)
	}
	// gopls must also receive roots: content edits are handled by its own watcher.
	write(goFile, "package fixture\nimport \"google.golang.org/protobuf/proto\"\nvar Unmarshal = proto.Unmarshal\n")
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "go_search", Arguments: map[string]any{"query": "Unmarshal"}})
		found := false
		if res != nil && !res.IsError {
			for _, c := range res.Content {
				if c, ok := c.(*mcp.TextContent); ok && strings.Contains(c.Text, "Unmarshal") && strings.Contains(c.Text, goFile) {
					found = true
				}
			}
		}
		if err == nil && found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gopls did not observe ordinary content edit through roots watcher")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	logs, _ := os.ReadFile(logPath)
	if count := strings.Count(string(logs), `"msg":"backend ready"`); count != previousGenerationCount {
		t.Fatalf("content edit rebuilt backend: before=%d after=%d:\n%s", previousGenerationCount, count, logs)
	}
}
