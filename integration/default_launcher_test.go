package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/testbackend"
)

// Both fixture builds share the existing integration build directory, removed
// by TestMain. Separate images have identical protocol contracts and snapshots.
var defaultFixtureBuild struct {
	sync.Once
	paths  [2]string
	err    error
	output []byte
}

func defaultFixtureBinary(t *testing.T, upgraded bool) string {
	t.Helper()
	proxy := proxyBinary(t)
	defaultFixtureBuild.Do(func() {
		for i, image := range []string{"initial", "upgraded"} {
			path := filepath.Join(filepath.Dir(proxy), "default-gopls-"+image)
			args := []string{"build"}
			if integrationRace {
				args = append(args, "-race")
			}
			args = append(args, "-ldflags=-X main.image="+image, "-o", path, "./testdata/default-gopls")
			cmd := exec.Command("go", args...)
			defaultFixtureBuild.output, defaultFixtureBuild.err = cmd.CombinedOutput()
			if defaultFixtureBuild.err != nil {
				return
			}
			defaultFixtureBuild.paths[i] = path
		}
	})
	if defaultFixtureBuild.err != nil {
		t.Fatalf("build default fixture: %v\n%s", defaultFixtureBuild.err, defaultFixtureBuild.output)
	}
	if upgraded {
		return defaultFixtureBuild.paths[1]
	}
	return defaultFixtureBuild.paths[0]
}
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".new", data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dst+".new", dst); err != nil {
		t.Fatal(err)
	}
}
func fixtureEnvWithPath(f *fixture, path string) []string {
	env := []string{}
	for _, entry := range f.env() {
		if !strings.HasPrefix(entry, "PATH=") && !strings.HasPrefix(entry, "PROXY_RAW_HELPER=") {
			env = append(env, entry)
		}
	}
	return append(env, "PATH="+path)
}

type defaultLayout struct {
	f                    *fixture
	proxy, backend, path string
}

func newDefaultLayout(t *testing.T, viaPath bool) *defaultLayout {
	t.Helper()
	f := newFixture(t)
	l := &defaultLayout{f: f, proxy: filepath.Join(f.root, "install 中文", "gopls-mcp-proxy")}
	copyExecutable(t, proxyBinary(t), l.proxy)
	l.path = filepath.Join(f.root, "path bins")
	if err := os.Mkdir(l.path, 0700); err != nil {
		t.Fatal(err)
	}
	l.backend = filepath.Join(filepath.Dir(l.proxy), "gopls")
	if viaPath {
		l.backend = filepath.Join(l.path, "gopls")
	}
	copyExecutable(t, defaultFixtureBinary(t, false), l.backend)
	// A real executable probe proves GoDiscovery stays disabled. No Go binary
	// is required by the fixture, and logs are outside the watched workspace.
	probe := "#!/bin/sh\nprintf invoked > '" + filepath.Join(f.root, "go-invoked") + "'\nexit 93\n"
	if err := os.WriteFile(filepath.Join(l.path, "go"), []byte(probe), 0700); err != nil {
		t.Fatal(err)
	}
	return l
}
func (l *defaultLayout) start(t *testing.T, args ...string) *rawPeer {
	t.Helper()
	cmd := exec.Command(l.proxy, args...)
	cmd.Dir = l.f.workspace
	cmd.Env = fixtureEnvWithPath(l.f, l.path)
	started := time.Now()
	p := startPeer(t, cmd, filepath.Join(l.f.root, fmt.Sprintf("default-proxy-%d.log", time.Now().UnixNano())))
	p.timeout = 15 * time.Second
	evidence := map[string]any{"phase": "L3", "assertion_scope": "actual built proxy process; synthetic backend, not Codex", "proxy": l.proxy, "argv": args, "cwd": cmd.Dir, "path": l.path, "proxy_pid": p.cmd.Process.Pid, "status": "started", "case_ids": defaultCaseIDs(t.Name()), "revision": os.Getenv("GITHUB_SHA"), "run_id": os.Getenv("GITHUB_RUN_ID"), "run_attempt": os.Getenv("GITHUB_RUN_ATTEMPT"), "start": started.UTC().Format(time.RFC3339Nano), "os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version()}
	for name, path := range map[string]string{"proxy_sha256": l.proxy, "fixture_sha256": l.backend} {
		data, err := os.ReadFile(path)
		if err == nil {
			evidence[name] = fmt.Sprintf("%x", sha256.Sum256(data))
		}
	}
	raw, _ := json.MarshalIndent(evidence, "", "  ")
	if err := os.WriteFile(filepath.Join(l.f.root, fmt.Sprintf("launch-%d.json", p.cmd.Process.Pid)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join(l.f.root, "go-invoked")); !os.IsNotExist(err) {
			t.Error("default launch unexpectedly invoked the Go executable")
		}
		evidence["status"] = "passed"
		if t.Failed() {
			evidence["status"] = "failed"
		}
		evidence["end"] = time.Now().UTC().Format(time.RFC3339Nano)
		raw, _ := json.MarshalIndent(evidence, "", "  ")
		if err := os.WriteFile(filepath.Join(l.f.root, fmt.Sprintf("launch-%d.json", p.cmd.Process.Pid)), raw, 0600); err != nil {
			t.Error(err)
		}
	})
	return p
}
func (l *defaultLayout) direct(t *testing.T) *rawPeer {
	t.Helper()
	cmd := exec.Command(defaultFixtureBinary(t, false), "mcp")
	cmd.Dir = l.f.workspace
	cmd.Env = fixtureEnvWithPath(l.f, l.path)
	return startPeer(t, cmd, filepath.Join(l.f.root, fmt.Sprintf("default-direct-%d.log", time.Now().UnixNano())))
}
func requireNewDefaultBoot(t *testing.T, previous, next testbackend.Snapshot, f *fixture) {
	t.Helper()
	if previous.Boot == next.Boot || previous.PID == next.PID {
		t.Fatalf("backend not replaced: previous=%+v next=%+v", previous, next)
	}
	if next.Reads != len(f.inputs) {
		t.Fatalf("input read count=%d want %d", next.Reads, len(f.inputs))
	}
	if !slices.Equal(next.Args, []string{"mcp"}) {
		t.Fatalf("default argv=%q", next.Args)
	}
}
func waitLogContains(t *testing.T, p *rawPeer, fragments ...string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(p.log)
		for _, s := range fragments {
			if bytes.Contains(b, []byte(s)) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(p.log)
	t.Fatalf("missing diagnostic %q:\n%s", fragments, b)
}
func requireDefaultCleanup(t *testing.T, l *defaultLayout, p *rawPeer) {
	t.Helper()
	p.shutdown()
	for _, event := range l.f.events(t) {
		if event.Kind != "boot" {
			continue
		}
		process, err := os.FindProcess(event.PID)
		if err == nil && process.Signal(syscall.Signal(0)) == nil {
			t.Errorf("backend survived cleanup pid=%d boot=%s", event.PID, event.Boot)
		}
	}
}

// VAL-LAUNCH-026: the exact standalone image used by default mode reads only at
// boot. The image has no imports of production wire/protocol or an MCP SDK.
func TestRawDefaultLauncherFixtureNeverRefreshesItself(t *testing.T) {
	l := newDefaultLayout(t, false)
	p := l.direct(t)
	a := p.snapshot("1", false)
	writeInput(t, l.f.inputs[0], "B", true)
	b := p.snapshot("2", false)
	requireInput(t, b, "main.go", "A")
	if a.Boot != b.Boot || a.PID != b.PID || a.Reads != b.Reads {
		t.Fatal("standalone fixture reread input")
	}
	q := l.direct(t)
	c := q.snapshot("1", false)
	requireInput(t, c, "main.go", "B")
	requireNewDefaultBoot(t, a, c, l.f)
	t.Logf("VAL-LAUNCH-026 same_pid=%d old_boot=%s reads=%d new_pid=%d new_boot=%s", a.PID, a.Boot, a.Reads, c.PID, c.Boot)
}

// VAL-LAUNCH-008/023/024/028/029: these are real zero-argument executable
// layouts. No proxy flags, environment-selected backend, or go-run wrapper.
func TestRawDefaultLauncherLayoutsAndOpaqueProtocol(t *testing.T) {
	for _, layout := range []string{"sibling", "path", "proxy-symlink", "proxy-hardlink"} {
		t.Run(layout, func(t *testing.T) {
			l := newDefaultLayout(t, layout == "path")
			if layout == "proxy-symlink" || layout == "proxy-hardlink" {
				alias := filepath.Join(l.f.root, "alias", "gopls-mcp-proxy")
				if err := os.Mkdir(filepath.Dir(alias), 0700); err != nil {
					t.Fatal(err)
				}
				if layout == "proxy-symlink" {
					if err := os.Symlink(l.proxy, alias); err != nil {
						t.Fatal(err)
					}
					copyExecutable(t, proxyBinary(t), filepath.Join(filepath.Dir(alias), "gopls"))
				} else {
					if err := os.Link(l.proxy, alias); err != nil {
						t.Fatal(err)
					}
					copyExecutable(t, defaultFixtureBinary(t, false), filepath.Join(filepath.Dir(alias), "gopls"))
					l.backend = filepath.Join(filepath.Dir(alias), "gopls")
					// The original proxy directory is deliberately unusable for this launch.
					copyExecutable(t, proxyBinary(t), filepath.Join(filepath.Dir(l.proxy), "gopls"))
				}
				l.proxy = alias
			}
			if layout == "sibling" {
				copyExecutable(t, proxyBinary(t), filepath.Join(l.path, "gopls"))
				copyExecutable(t, proxyBinary(t), filepath.Join(l.f.workspace, "gopls"))
			}
			p := l.start(t)
			if len(p.cmd.Args) != 1 {
				t.Fatalf("not zero argv: %q", p.cmd.Args)
			}
			p.initialize("2025-11-25")
			s := p.snapshot("1", false)
			if !slices.Equal(s.Args, []string{"mcp"}) {
				t.Fatalf("argv=%q", s.Args)
			}
			var toolReply struct {
				Result struct {
					Content    []struct{ Type, Text string } `json:"content"`
					Structured testbackend.Snapshot          `json:"structuredContent"`
					IsError    bool                          `json:"isError"`
				} `json:"result"`
				Error json.RawMessage `json:"error"`
			}
			toolRaw := p.call(`"tool-call"`, "tools/call", `{"name":"future_tool"}`)
			if err := json.Unmarshal(toolRaw, &toolReply); err != nil {
				t.Fatal(err)
			}
			tool := toolReply.Result
			if len(toolReply.Error) != 0 || tool.IsError || len(tool.Content) != 1 || tool.Content[0].Type != "text" || tool.Content[0].Text == "" {
				t.Fatalf("tool invocation failed: %s", toolRaw)
			}
			if tool.Structured.PID != s.PID || tool.Structured.Boot != s.Boot || tool.Structured.Reads != len(l.f.inputs) || !slices.Equal(tool.Structured.Args, []string{"mcp"}) {
				t.Fatalf("tool result lost snapshot identity: %s", toolRaw)
			}
			var contentSnapshot testbackend.Snapshot
			if err := json.Unmarshal([]byte(tool.Content[0].Text), &contentSnapshot); err != nil || contentSnapshot.Boot != s.Boot {
				t.Fatalf("tool text does not describe current boot: %s", toolRaw)
			}
			unknown := p.call(`"unknown-tool"`, "tools/call", `{"name":"missing_tool"}`)
			if !bytes.Contains(unknown, []byte(`"code":-32602`)) || bytes.Contains(unknown, []byte(`"result"`)) {
				t.Fatalf("unknown tool did not fail: %s", unknown)
			}

			want, _ := filepath.EvalSymlinks(l.f.workspace)
			got, _ := filepath.EvalSymlinks(s.CWD)
			if got != want {
				t.Fatalf("cwd=%q want %q", s.CWD, want)
			}
			if s.Environment["PATH"] != l.path || s.Environment["FIXTURE_CUSTOM_ENV"] != "literal $value 中文" || s.Environment["GOWORK"] != "off" || s.Environment["GOENV"] != "off" {
				t.Fatalf("environment changed: %+v", s.Environment)
			}
			for _, id := range []string{"900719925474099312345", `"unicode 中文"`} {
				params := `{"opaque":null,"literal":"$HOME 中文","n":900719925474099312345}`
				request := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"fixture/echo","params":%s,"unknown":{"preserve":true}}`, id, params)
				p.send(request)
				got := p.response(id)
				want := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s,"x-fixture":{"untouched":900719925474099312345}}`, id, params)
				if string(got) != want {
					t.Fatalf("raw frame changed: %s", got)
				}
				found := false
				for _, e := range l.f.events(t) {
					if e.Kind == "receipt" && e.Raw == request {
						found = true
					}
				}
				if !found {
					t.Fatal("raw request not preserved")
				}
			}
			direct := l.direct(t)
			for _, method := range []string{"tools/list", "fixture/error", "fixture/tool-error"} {
				if a, b := p.call(`"opaque"`, method, `{}`), direct.call(`"opaque"`, method, `{}`); !bytes.Equal(a, b) {
					t.Fatalf("%s payload changed\n%s\n%s", method, a, b)
				}
			}
			direct.shutdown()
			if raw := p.call("2", "fixture/reverse", `{}`); !bytes.Contains(raw, []byte("file:///client-owned-root")) {
				t.Fatalf("roots changed: %s", raw)
			}
			requireDefaultCleanup(t, l, p)
		})
	}
}

// VAL-LAUNCH-027: equal-length edits with restored mtime, for all five input
// classes, twice each. Every result belongs to a new independently observed
// backend boot while the actual proxy process and front connection stay fixed.
func TestRawDefaultLauncherRefreshAndNoReplay(t *testing.T) {
	l := newDefaultLayout(t, false)
	p := l.start(t)
	p.initialize("2025-11-25")
	previous := p.snapshot("1", false)
	proxyPID := p.cmd.Process.Pid
	for i, path := range l.f.inputs {
		for pass, value := range []string{"B", "C"} {
			writeInput(t, path, value, true)
			next := p.snapshot(fmt.Sprint(2+i*2+pass), false)
			requireInput(t, next, filepath.Base(path), value)
			requireNewDefaultBoot(t, previous, next, l.f)
			if p.cmd.Process.Pid != proxyPID {
				t.Fatal("proxy process changed")
			}
			t.Logf("VAL-LAUNCH-027 proxy_pid=%d input=%s value=%s old_boot=%s new_boot=%s old_pid=%d new_pid=%d reads=%d", proxyPID, filepath.Base(path), value, previous.Boot, next.Boot, previous.PID, next.PID, next.Reads)
			previous = next
		}
	}
	p.send(`{"jsonrpc":"2.0","id":90,"method":"fixture/hold","params":{}}`)
	l.f.waitExecution(t, "fixture/hold")
	writeInput(t, l.f.inputs[0], "D", true)
	p.send(`{"jsonrpc":"2.0","id":91,"method":"fixture/snapshot","params":{}}`)
	raw := p.response("90")
	for _, want := range []string{`"requestDispatched":true`, `"automaticReplay":false`, `"sideEffects":"unknown"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
	var reply struct {
		Result testbackend.Snapshot `json:"result"`
	}
	if err := json.Unmarshal(p.response("91"), &reply); err != nil {
		t.Fatal(err)
	}
	requireInput(t, reply.Result, "main.go", "D")
	requireNewDefaultBoot(t, previous, reply.Result, l.f)
	count := 0
	for _, e := range l.f.events(t) {
		if e.Kind == "execution" && e.Method == "fixture/hold" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dispatched request executed %d times", count)
	}
	requireDefaultCleanup(t, l, p)
}

// VAL-LAUNCH-019/022: discovery happens once. Later higher-priority candidates
// cannot redirect the process; replacing the selected file or symlink can.
func TestRawDefaultLauncherFixedLocatorAndUpgrade(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			l := newDefaultLayout(t, true)
			early := filepath.Join(l.f.root, "earlier path")
			if err := os.Mkdir(early, 0700); err != nil {
				t.Fatal(err)
			}
			l.path = early + string(os.PathListSeparator) + l.path
			if kind == "symlink" {
				if err := os.Remove(l.backend); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(defaultFixtureBinary(t, false), l.backend); err != nil {
					t.Fatal(err)
				}
			}
			p := l.start(t)
			p.initialize("2025-11-25")
			first := p.snapshot("1", false)
			copyExecutable(t, proxyBinary(t), filepath.Join(filepath.Dir(l.proxy), "gopls"))
			copyExecutable(t, proxyBinary(t), filepath.Join(early, "gopls"))
			if kind == "file" {
				copyExecutable(t, defaultFixtureBinary(t, true), l.backend)
			} else {
				tmp := l.backend + ".next"
				if err := os.Symlink(defaultFixtureBinary(t, true), tmp); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(tmp, l.backend); err != nil {
					t.Fatal(err)
				}
			}
			writeInput(t, l.f.inputs[0], "B", true)
			next := p.snapshot("2", false)
			requireInput(t, next, "main.go", "B")
			requireNewDefaultBoot(t, first, next, l.f)
			raw, err := os.ReadFile(l.f.evidence)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, line := range bytes.Split(raw, []byte{'\n'}) {
				var event struct {
					Kind, Image string
					PID         int
				}
				_ = json.Unmarshal(line, &event)
				if event.Kind == "image" && event.Image == "upgraded" && event.PID == next.PID {
					found = true
				}
			}
			if !found {
				t.Fatal("new PID did not run upgraded image")
			}
			requireDefaultCleanup(t, l, p)
		})
	}
}

// VAL-LAUNCH-020/021: no rediscovery or old success after the fixed locator is
// damaged. Repair inside the original default recovery window resumes the same
// front session. These cases intentionally leave production budgets untouched.
func TestRawDefaultLauncherRejectsChangedLocatorAndRecovers(t *testing.T) {
	for _, kind := range []string{"self-path", "self-symlink", "self-hardlink", "deleted", "broken-link", "not-executable", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			l := newDefaultLayout(t, false)
			p := l.start(t)
			p.initialize("2025-11-25")
			first := p.snapshot("1", false)
			// A valid fallback would hide a failure if the implementation rediscovers.
			copyExecutable(t, defaultFixtureBinary(t, true), filepath.Join(l.path, "gopls"))
			if err := os.Remove(l.backend); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "self-path": // The running executable is atomically moved to the locator,
				// leaving its original pathname as a hardlink; identities stay observable.
				if err := os.Rename(l.proxy, l.backend); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(l.backend, l.proxy); err != nil {
					t.Fatal(err)
				}
			case "self-symlink":
				if err := os.Symlink(l.proxy, l.backend); err != nil {
					t.Fatal(err)
				}
			case "self-hardlink":
				if err := os.Link(l.proxy, l.backend); err != nil {
					t.Fatal(err)
				}
			case "deleted":
			case "broken-link":
				if err := os.Symlink(filepath.Join(l.f.root, "missing"), l.backend); err != nil {
					t.Fatal(err)
				}
			case "not-executable":
				if err := os.WriteFile(l.backend, []byte("invalid executable"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(l.backend, 0700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(l.backend, 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeInput(t, l.f.inputs[0], "B", true)
			p.send(`{"jsonrpc":"2.0","id":2,"method":"fixture/snapshot","params":{}}`)
			waitLogContains(t, p, "backend start failed", "backend startup failed", "backend_start_failed", "start attempt failed")
			for _, e := range l.f.events(t) {
				if e.Kind == "boot" && e.Boot != first.Boot {
					t.Fatalf("bad locator spawned/fell back: %+v", e)
				}
			}
			select {
			case raw, ok := <-p.frames:
				if ok {
					t.Fatalf("unexpected response before repair: %s", raw)
				} else {
					t.Fatal("connection closed before repair")
				}
			default:
			}
			if err := os.Remove(l.backend); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			copyExecutable(t, defaultFixtureBinary(t, false), l.backend)
			var reply struct {
				Result testbackend.Snapshot `json:"result"`
			}
			raw := p.response("2")
			if err := json.Unmarshal(raw, &reply); err != nil {
				t.Fatal(err)
			}
			requireInput(t, reply.Result, "main.go", "B")
			requireNewDefaultBoot(t, first, reply.Result, l.f)
			requireDefaultCleanup(t, l, p)
		})
	}
}

// VAL-LAUNCH-025/028: optional watch roots expand coverage, never change cwd or
// backend argv. This is deliberately classified as a flags-only default mode.
func TestRawDefaultLauncherProxyOptionsAndExternalInputs(t *testing.T) {
	l := newDefaultLayout(t, false)
	outside := filepath.Join(l.f.root, "external")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(outside, "external.txt")
	if err := os.WriteFile(input, []byte("A"), 0600); err != nil {
		t.Fatal(err)
	}
	l.f.inputs = append(l.f.inputs, input)
	p := l.start(t, "--watch-root", outside, "--include", "**", "--exclude", "**/ignored.txt", "--go-discovery=false")
	p.initialize("2025-11-25")
	first := p.snapshot("1", false)
	writeInput(t, input, "B", true)
	next := p.snapshot("2", false)
	requireInput(t, next, "external.txt", "B")
	requireNewDefaultBoot(t, first, next, l.f)
	cwd, _ := filepath.EvalSymlinks(next.CWD)
	want, _ := filepath.EvalSymlinks(l.f.workspace)
	if cwd != want {
		t.Fatalf("watch root changed cwd %q", next.CWD)
	}
	requireDefaultCleanup(t, l, p)
	// With no watch root the external input remains the old boot's snapshot. A
	// later in-root edit reloads it only as part of the ordinary backend restart.
	l = newDefaultLayout(t, false)
	input = filepath.Join(l.f.root, "external.txt")
	if err := os.WriteFile(input, []byte("A"), 0600); err != nil {
		t.Fatal(err)
	}
	l.f.inputs = append(l.f.inputs, input)
	q := l.start(t)
	q.initialize("2025-11-25")
	a := q.snapshot("1", false)
	writeInput(t, input, "B", true)
	b := q.snapshot("2", false)
	requireInput(t, b, "external.txt", "A")
	if a.Boot != b.Boot {
		t.Fatal("default watch scope expanded outside cwd")
	}
	writeInput(t, l.f.inputs[0], "B", true)
	c := q.snapshot("3", false)
	requireInput(t, c, "external.txt", "B")
	requireNewDefaultBoot(t, b, c, l.f)
	requireDefaultCleanup(t, l, q)
}

func defaultCaseIDs(name string) []string {
	mapping := map[string][]string{
		"FixtureNeverRefreshesItself":      {"VAL-LAUNCH-026"},
		"LayoutsAndOpaqueProtocol":         {"VAL-LAUNCH-008", "VAL-LAUNCH-023", "VAL-LAUNCH-024", "VAL-LAUNCH-028", "VAL-LAUNCH-029"},
		"RefreshAndNoReplay":               {"VAL-LAUNCH-027"},
		"FixedLocatorAndUpgrade":           {"VAL-LAUNCH-019", "VAL-LAUNCH-022"},
		"RejectsChangedLocatorAndRecovers": {"VAL-LAUNCH-020", "VAL-LAUNCH-021"},
		"ProxyOptionsAndExternalInputs":    {"VAL-LAUNCH-025", "VAL-LAUNCH-028"},
		"ProxyIdentityReplacement":         {"VAL-LAUNCH-017"},
		"RecoveryDeadline":                 {"VAL-LAUNCH-021"},
		"IncludeExcludeAndGoDiscovery":     {"VAL-LAUNCH-025", "VAL-LAUNCH-028"},
	}
	for fragment, ids := range mapping {
		if strings.Contains(name, fragment) {
			return ids
		}
	}
	return []string{}
}

// VAL-LAUNCH-017: after atomic proxy upgrade, both the old running inode and
// the newly installed locator remain forbidden backend identities. Losing the
// required self locator fails closed until it is restored.
func TestRawDefaultLauncherProxyIdentityReplacement(t *testing.T) {
	for _, kind := range []string{"old-inode", "current-inode", "missing-self"} {
		t.Run(kind, func(t *testing.T) {
			l := newDefaultLayout(t, false)
			p := l.start(t)
			p.initialize("2025-11-25")
			first := p.snapshot("1", false)
			old := filepath.Join(l.f.root, "old-running-proxy")
			if err := os.Link(l.proxy, old); err != nil {
				t.Fatal(err)
			}
			copyExecutable(t, proxyBinary(t), l.proxy)
			if kind == "missing-self" {
				if err := os.Remove(l.proxy); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(l.backend); err != nil {
					t.Fatal(err)
				}
				target := l.proxy
				if kind == "old-inode" {
					target = old
				}
				if err := os.Link(target, l.backend); err != nil {
					t.Fatal(err)
				}
			}
			writeInput(t, l.f.inputs[0], "B", true)
			p.send(`{"jsonrpc":"2.0","id":2,"method":"fixture/snapshot","params":{}}`)
			waitLogContains(t, p, "backend start failed")
			for _, e := range l.f.events(t) {
				if e.Kind == "boot" && e.Boot != first.Boot {
					t.Fatalf("forbidden or uncheckable identity started: %+v", e)
				}
			}
			if kind == "missing-self" {
				copyExecutable(t, proxyBinary(t), l.proxy)
			} else {
				copyExecutable(t, defaultFixtureBinary(t, false), l.backend)
			}
			var reply struct {
				Result testbackend.Snapshot `json:"result"`
			}
			if err := json.Unmarshal(p.response("2"), &reply); err != nil {
				t.Fatal(err)
			}
			requireInput(t, reply.Result, "main.go", "B")
			requireNewDefaultBoot(t, first, reply.Result, l.f)
			requireDefaultCleanup(t, l, p)
		})
	}
}

// VAL-LAUNCH-021: exercise the production 60-second recovery window without
// flags or altered clocks. This is engineering evidence, not a Codex timeout
// test. Parent/fixture cleanup must finish within the documented extra 6s.
func TestRawDefaultLauncherRecoveryDeadline(t *testing.T) {
	l := newDefaultLayout(t, false)
	p := l.start(t)
	p.initialize("2025-11-25")
	first := p.snapshot("1", false)
	if err := os.Remove(l.backend); err != nil {
		t.Fatal(err)
	}
	copyExecutable(t, defaultFixtureBinary(t, true), filepath.Join(l.path, "gopls"))
	started := time.Now()
	writeInput(t, l.f.inputs[0], "B", true)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"fixture/snapshot","params":{}}`)
	timer := time.NewTimer(69 * time.Second)
	defer timer.Stop()
	for {
		select {
		case raw, open := <-p.frames:
			if !open {
				elapsed := time.Since(started)
				if elapsed < 59*time.Second || elapsed > 68*time.Second {
					t.Fatalf("default recovery deadline elapsed=%s", elapsed)
				}
				requireDefaultCleanup(t, l, p)
				logs, _ := os.ReadFile(p.log)
				if !bytes.Contains(logs, []byte("refresh_timeout")) {
					t.Fatalf("not refresh deadline:\n%s", logs)
				}
				for _, event := range l.f.events(t) {
					if event.Kind == "boot" && event.Boot != first.Boot {
						t.Fatalf("deleted locator silently rediscovered: %+v", event)
					}
				}
				t.Logf("VAL-LAUNCH-021 phase=L3 zero_argv=true default_refresh_budget=60s cleanup_budget=6s elapsed=%s status=passed", elapsed)
				return
			}
			var msg struct {
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatal(err)
			}
			if len(msg.Result) > 0 {
				t.Fatalf("invalid locator returned success: %s", raw)
			}
		case <-timer.C:
			t.Fatal("default recovery and cleanup exceeded 60s+6s budget plus scheduler allowance")
		}
	}
}

// Include/exclude and opt-in Go workspace discovery are ordinary proxy flags,
// separate from the zero-argument default-launch product target.
func TestRawDefaultLauncherIncludeExcludeAndGoDiscovery(t *testing.T) {
	t.Run("include-exclude", func(t *testing.T) {
		l := newDefaultLayout(t, false)
		for _, name := range []string{"ignored.txt", "included.txt", "not-included.bin"} {
			path := filepath.Join(l.f.workspace, name)
			if err := os.WriteFile(path, []byte("A"), 0600); err != nil {
				t.Fatal(err)
			}
			l.f.inputs = append(l.f.inputs, path)
		}
		p := l.start(t, "--include", "*.txt", "--exclude", "ignored.txt")
		p.initialize("2025-11-25")
		first := p.snapshot("1", false)
		for _, name := range []string{"ignored.txt", "not-included.bin", "main.go"} {
			writeInput(t, filepath.Join(l.f.workspace, name), "B", true)
		}
		same := p.snapshot("2", false)
		if same.Boot != first.Boot {
			t.Fatal("excluded or non-included input triggered refresh")
		}
		requireInput(t, same, "ignored.txt", "A")
		requireInput(t, same, "not-included.bin", "A")
		writeInput(t, filepath.Join(l.f.workspace, "included.txt"), "B", true)
		next := p.snapshot("3", false)
		requireNewDefaultBoot(t, first, next, l.f)
		requireInput(t, next, "included.txt", "B")
		requireDefaultCleanup(t, l, p)
	})
	t.Run("go-discovery", func(t *testing.T) {
		l := newDefaultLayout(t, false)
		outside := filepath.Join(l.f.root, "use-module")
		if err := os.Mkdir(outside, 0700); err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(outside, "external.go")
		if err := os.WriteFile(input, []byte("A"), 0600); err != nil {
			t.Fatal(err)
		}
		l.f.inputs = append(l.f.inputs, input)
		work := filepath.Join(l.f.workspace, "go.work")
		if err := os.WriteFile(work, []byte("go 1.25.0\nuse ../use-module\n"), 0600); err != nil {
			t.Fatal(err)
		}
		// The existing fixture uses GOWORK=off. This explicit opt-in case supplies
		// only a standard Go input, never a fixture-controlled backend selector.
		env := fixtureEnvWithPath(l.f, l.path)
		for i, value := range env {
			if strings.HasPrefix(value, "GOWORK=") {
				env[i] = "GOWORK=" + work
			}
		}
		cmd := exec.Command(l.proxy, "--go-discovery")
		cmd.Dir = l.f.workspace
		cmd.Env = env
		p := startPeer(t, cmd, filepath.Join(l.f.root, "go-discovery.log"))
		p.initialize("2025-11-25")
		first := p.snapshot("1", false)
		if first.Environment["GOWORK"] != work {
			t.Fatal("GOWORK was rewritten")
		}
		writeInput(t, input, "B", true)
		next := p.snapshot("2", false)
		requireInput(t, next, "external.go", "B")
		requireNewDefaultBoot(t, first, next, l.f)
		requireDefaultCleanup(t, l, p)
		if _, err := os.Stat(filepath.Join(l.f.root, "go-invoked")); !os.IsNotExist(err) {
			t.Fatal("Go executable was unnecessarily invoked")
		}
	})
}
