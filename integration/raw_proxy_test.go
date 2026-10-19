package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/testbackend"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if proxyBuild.path != "" {
		_ = os.RemoveAll(filepath.Dir(proxyBuild.path))
	}
	os.Exit(code)
}

func TestRawHelperProcess(t *testing.T) {
	if os.Getenv("PROXY_RAW_HELPER") != "1" {
		return
	}
	var inputs []string
	_ = json.Unmarshal([]byte(os.Getenv("PROXY_RAW_INPUTS")), &inputs)
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if err := testbackend.Run(os.Stdin, os.Stdout, testbackend.Config{Inputs: inputs, Log: os.Getenv("PROXY_RAW_LOG"), Args: args}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

type rawPeer struct {
	t             *testing.T
	cmd           *exec.Cmd
	in            io.WriteCloser
	frames        chan []byte
	errors        chan error
	done          chan error
	log           string
	reverseIDs    []string
	notifications [][]byte
	timeout       time.Duration
	roots         []map[string]string
	shutdown      func()
}

func startPeer(t *testing.T, cmd *exec.Cmd, logPath string) *rawPeer {
	t.Helper()
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = stderr
	p := &rawPeer{t: t, cmd: cmd, in: in, frames: make(chan []byte, 256), errors: make(chan error, 1), done: make(chan error, 1), log: logPath, timeout: 10 * time.Second, roots: []map[string]string{{"uri": "file:///client-owned-root", "name": "real client"}}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 32<<20)
		for scanner.Scan() {
			p.frames <- append([]byte(nil), scanner.Bytes()...)
		}
		p.errors <- scanner.Err()
		close(p.frames)
	}()
	go func() { p.done <- cmd.Wait(); _ = stderr.Close() }()
	var shutdownOnce sync.Once
	p.shutdown = func() {
		shutdownOnce.Do(func() {
			_ = p.in.Close()
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-p.done
				t.Errorf("process did not exit within cleanup budget")
			}
			logs, _ := os.ReadFile(logPath)
			if bytes.Contains(logs, []byte("WARNING: DATA RACE")) {
				t.Errorf("subprocess race detector failed:\n%s", logs)
			}
		})
	}
	t.Cleanup(p.shutdown)
	return p
}
func (p *rawPeer) send(raw string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, raw+"\n"); err != nil {
		p.t.Fatal(err)
	}
}
func (p *rawPeer) next() []byte {
	p.t.Helper()
	select {
	case b, ok := <-p.frames:
		if !ok {
			logs, _ := os.ReadFile(p.log)
			p.t.Fatalf("peer stdout ended:\n%s", logs)
		}
		return b
	case <-time.After(p.timeout):
		logs, _ := os.ReadFile(p.log)
		p.t.Fatalf("raw response timeout:\n%s", logs)
	}
	return nil
}
func (p *rawPeer) response(id string) []byte {
	p.t.Helper()
	for {
		raw := p.next()
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			p.t.Fatalf("invalid peer JSON: %s", raw)
		}
		if m.Method == "roots/list" {
			p.reverseIDs = append(p.reverseIDs, string(m.ID))
			roots, _ := json.Marshal(p.roots)
			p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"roots":%s,"_meta":{"client":true}}}`, m.ID, roots))
			continue
		}
		if m.Method != "" {
			p.notifications = append(p.notifications, raw)
			continue
		}
		if string(m.ID) == id {
			return raw
		}
		p.t.Fatalf("unexpected response ID %s, want %s: %s", m.ID, id, raw)
	}
}
func (p *rawPeer) call(id, method, params string) []byte {
	p.t.Helper()
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, id, method, params))
	return p.response(id)
}
func (p *rawPeer) initialize(revision string) {
	p.t.Helper()
	raw := p.call("100", "initialize", fmt.Sprintf(`{"protocolVersion":%q,"clientInfo":{"name":"actual-client","version":"7"},"capabilities":{"roots":{"listChanged":true}}}`, revision))
	if !bytes.Contains(raw, []byte(`"raw-snapshot-fixture"`)) {
		p.t.Fatalf("initialize: %s", raw)
	}
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}
func (p *rawPeer) snapshot(id string, modern bool) testbackend.Snapshot {
	p.t.Helper()
	params := `{}`
	if modern {
		params = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"modern-client","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}`
	}
	raw := p.call(id, "fixture/snapshot", params)
	var msg struct {
		Result testbackend.Snapshot `json:"result"`
		Error  json.RawMessage      `json:"error"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		p.t.Fatal(err)
	}
	if len(msg.Error) > 0 {
		p.t.Fatalf("snapshot failed: %s", raw)
	}
	return msg.Result
}

type fixture struct {
	root, workspace, evidence string
	inputs                    []string
}

// Optional CI evidence retention. Source/fixture inputs are synthetic; every
// archived file remains outside watched roots and contains no production data.
func archiveEvidence(t *testing.T, root string) {
	t.Helper()
	destination := os.Getenv("PROXY_TEST_EVIDENCE_DIR")
	if destination == "" {
		return
	}
	t.Cleanup(func() {
		name := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(t.Name())
		dir := filepath.Join(destination, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Errorf("create evidence directory: %v", err)
			return
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Errorf("read evidence: %v", err)
			return
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, entry.Name()))
			if err != nil {
				t.Errorf("read evidence %s: %v", entry.Name(), err)
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0600); err != nil {
				t.Errorf("save evidence %s: %v", entry.Name(), err)
			}
		}
	})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{root: t.TempDir()}
	archiveEvidence(t, f.root)
	f.workspace = filepath.Join(f.root, "workspace")
	f.evidence = filepath.Join(f.root, "events.jsonl")
	if err := os.Mkdir(f.workspace, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.go", "go.mod", "go.sum", "go.work", "go.work.sum"} {
		path := filepath.Join(f.workspace, name)
		if err := os.WriteFile(path, []byte("A"), 0600); err != nil {
			t.Fatal(err)
		}
		f.inputs = append(f.inputs, path)
	}
	return f
}
func (f *fixture) env() []string {
	inputs, _ := json.Marshal(f.inputs)
	return append(os.Environ(), "PROXY_RAW_HELPER=1", "PROXY_RAW_INPUTS="+string(inputs), "PROXY_RAW_LOG="+f.evidence, "FIXTURE_CUSTOM_ENV=literal $value 中文", "GOWORK=off", "GOENV=off", "GORACE=atexit_sleep_ms=0")
}
func (f *fixture) direct(t *testing.T) *rawPeer {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRawHelperProcess$", "--")
	cmd.Dir = f.workspace
	cmd.Env = f.env()
	return startPeer(t, cmd, filepath.Join(f.root, fmt.Sprintf("direct-%d.log", time.Now().UnixNano())))
}

var proxyBuild struct {
	sync.Once
	path   string
	err    error
	output []byte
}

func proxyBinary(t *testing.T) string {
	t.Helper()
	proxyBuild.Do(func() {
		dir, err := os.MkdirTemp("", "proxy-integration-build-")
		if err != nil {
			proxyBuild.err = err
			return
		}
		proxyBuild.path = filepath.Join(dir, "gopls-mcp-proxy")
		buildArgs := []string{"build"}
		if integrationRace {
			buildArgs = append(buildArgs, "-race")
		}
		buildArgs = append(buildArgs, "-o", proxyBuild.path, "../cmd/gopls-mcp-proxy")
		cmd := exec.Command("go", buildArgs...)
		proxyBuild.output, proxyBuild.err = cmd.CombinedOutput()
	})
	if proxyBuild.err != nil {
		t.Fatalf("build proxy: %v\n%s", proxyBuild.err, proxyBuild.output)
	}
	return proxyBuild.path
}
func (f *fixture) proxy(t *testing.T, args ...string) *rawPeer { return f.proxyFlags(t, nil, args...) }

func (f *fixture) proxyFlags(t *testing.T, extraFlags []string, args ...string) *rawPeer {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	flags := []string{"--disable-watcher", "--poll-interval", "1h", "--debounce", "10ms", "--max-debounce", "50ms", "--drain-timeout", "100ms", "--startup-timeout", "2s", "--shutdown-timeout", "100ms", "--request-timeout", "5s"}
	flags = append(flags, extraFlags...)
	flags = append(flags, "--", exe, "-test.run=^TestRawHelperProcess$", "--")
	flags = append(flags, args...)
	cmd := exec.Command(proxyBinary(t), flags...)
	cmd.Dir = f.workspace
	cmd.Env = f.env()
	return startPeer(t, cmd, filepath.Join(f.root, fmt.Sprintf("proxy-%d.log", time.Now().UnixNano())))
}
func (f *fixture) events(t *testing.T) []testbackend.Event {
	t.Helper()
	raw, err := os.ReadFile(f.evidence)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var events []testbackend.Event
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var event testbackend.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
func (f *fixture) waitExecution(t *testing.T, method string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range f.events(t) {
			if e.Kind == "execution" && e.Method == method {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("helper never independently recorded %s", method)
}
func writeInput(t *testing.T, path, value string, preserveTime bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	if preserveTime {
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
}
func requireInput(t *testing.T, s testbackend.Snapshot, name, want string) {
	t.Helper()
	value := s.Inputs[name]
	if value == nil || *value != want {
		t.Fatalf("input %s=%v want %q, snapshot=%+v", name, value, want, s)
	}
}

// VAL-FIX-001: self-check before using this helper to attribute proxy refreshes.
func TestRawFixtureNeverRefreshesItself(t *testing.T) {
	f := newFixture(t)
	p := f.direct(t)
	a := p.snapshot("1", false)
	writeInput(t, f.inputs[0], "B", true)
	b := p.snapshot("2", false)
	requireInput(t, b, "main.go", "A")
	if a.Boot != b.Boot || a.PID != b.PID || a.Reads != b.Reads {
		t.Fatal("helper unexpectedly refreshed")
	}
	q := f.direct(t)
	c := q.snapshot("1", false)
	requireInput(t, c, "main.go", "B")
	if c.Boot == a.Boot || c.PID == a.PID {
		t.Fatal("new helper process missing")
	}
	t.Logf("VAL-FIX-001 old_boot=%s old_pid=%d reads=%d new_boot=%s new_pid=%d", a.Boot, a.PID, a.Reads, c.Boot, c.PID)
}

// VAL-INPUT-001/002/005/007, GEN-001 and the basic MCP-001 subassertion.
func TestRawProxyRefreshEachRevision(t *testing.T) {
	for _, revision := range []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"} {
		t.Run(revision, func(t *testing.T) {
			f := newFixture(t)
			p := f.proxy(t)
			modern := revision == "2026-07-28"
			if !modern {
				p.initialize(revision)
			}
			previous := p.snapshot("1", modern)
			proxyPID := p.cmd.Process.Pid
			for i, path := range f.inputs {
				for pass, value := range []string{"B", "C"} {
					writeInput(t, path, value, true)
					next := p.snapshot(fmt.Sprint(2+i*2+pass), modern)
					requireInput(t, next, filepath.Base(path), value)
					if next.Boot == previous.Boot || next.PID == previous.PID {
						t.Fatalf("input change did not replace backend: old=%+v new=%+v", previous, next)
					}
					if p.cmd.Process.Pid != proxyPID {
						t.Fatal("proxy changed PID")
					}
					if next.Reads != len(f.inputs) {
						t.Fatalf("helper reread inputs: %d", next.Reads)
					}
					t.Logf("revision=%s proxy_pid=%d old_boot=%s new_boot=%s old_pid=%d new_pid=%d input=%s", revision, proxyPID, previous.Boot, next.Boot, previous.PID, next.PID, filepath.Base(path))
					previous = next
				}
			}
			if modern {
				for _, e := range f.events(t) {
					if e.Method == "initialize" {
						t.Fatal("proxy invented modern initialize")
					}
				}
			}
		})
	}
}

// VAL-RAW-001/002/003/005 and VAL-ID-001: independent bytes, no shared SDK.
func TestRawProxyPayloadIdentity(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.initialize("2025-11-25")
	ids := []string{`1`, `"1"`, `900719925474099312345`}
	for _, id := range ids {
		params := `{"missingIsDifferent":null,"_meta":{"unknown":{"large":900719925474099312345}},"literal":"$HOME 中文"}`
		request := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"fixture/echo","params":%s,"x-unknown":{"keep":true}}`, id, params)
		p.send(request)
		got := p.response(id)
		want := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s,"x-fixture":{"untouched":900719925474099312345}}`, id, params)
		if string(got) != want {
			t.Fatalf("response changed:\ngot  %s\nwant %s", got, want)
		}
		found := false
		for _, e := range f.events(t) {
			if e.Kind == "receipt" && e.Raw == request {
				found = true
			}
		}
		if !found {
			t.Fatal("request raw transcript differs")
		}
	}
	for _, method := range []string{"fixture/error", "fixture/tool-error", "tools/list"} {
		direct := f.direct(t)
		want := direct.call(`"exact"`, method, `{}`)
		got := p.call(`"exact"`, method, `{}`)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s changed:\ngot %s\nwant %s", method, got, want)
		}
	}
	payload := strings.Repeat("0123456789abcdef", 16384)
	params, _ := json.Marshal(map[string]string{"large": payload})
	got := p.call(`"large"`, "fixture/echo", string(params))
	if !bytes.Contains(got, []byte(payload)) {
		t.Fatal("large legal message truncated")
	}
}

// VAL-CLI-001/002/003: literal argv and inherited execution context.
func TestRawProxyArgvAndEnvironment(t *testing.T) {
	f := newFixture(t)
	args := []string{"--help", "--version", "", "a b", "中文", "'quoted'", `$(touch should-not-exist)`, `$HOME`, `;`}
	p := f.proxy(t, args...)
	p.initialize("2025-11-25")
	s := p.snapshot("1", false)
	a, _ := json.Marshal(args)
	b, _ := json.Marshal(s.Args)
	if !bytes.Equal(a, b) {
		t.Fatalf("argv=%s want %s", b, a)
	}
	actual, _ := filepath.EvalSymlinks(s.CWD)
	expected, _ := filepath.EvalSymlinks(f.workspace)
	if actual != expected {
		t.Fatalf("cwd=%q want %q", s.CWD, f.workspace)
	}
	if s.Environment["FIXTURE_CUSTOM_ENV"] != "literal $value 中文" || s.Environment["GOWORK"] != "off" || s.Environment["GOENV"] != "off" {
		t.Fatalf("environment not preserved: %+v", s.Environment)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("shell expression executed")
	}
}

// VAL-NOREPLAY-001/002: evidence is the fixture's independent execution log.
func TestRawProxyNeverReplaysDispatchedBusiness(t *testing.T) {
	for _, method := range []string{"fixture/hold", "fixture/crash", "fixture/half"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t)
			p := f.proxy(t)
			p.initialize("2025-11-25")
			p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{}}`, method))
			f.waitExecution(t, method)
			writeInput(t, f.inputs[0], "B", true)
			p.send(`{"jsonrpc":"2.0","id":2,"method":"fixture/snapshot","params":{}}`)
			raw := p.response("1")
			var msg struct {
				Error struct {
					Data struct {
						Dispatched  bool   `json:"requestDispatched"`
						Replay      bool   `json:"automaticReplay"`
						SideEffects string `json:"sideEffects"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatal(err)
			}
			if !msg.Error.Data.Dispatched || msg.Error.Data.Replay || msg.Error.Data.SideEffects != "unknown" {
				t.Fatalf("missing truthful dispatch/side-effect diagnostic: %s", raw)
			}
			var next struct {
				Result testbackend.Snapshot `json:"result"`
			}
			if err := json.Unmarshal(p.response("2"), &next); err != nil {
				t.Fatal(err)
			}
			requireInput(t, next.Result, "main.go", "B")
			count := 0
			for _, e := range f.events(t) {
				if e.Kind == "execution" && e.Method == method {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("business executed %d times", count)
			}
		})
	}
}

// VAL-ERROR-003: a successful write may have happened before freshness fails.
func TestRawProxyWriteToolReportsSideEffects(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.initialize("2025-11-25")
	params, _ := json.Marshal(map[string]string{"path": f.inputs[0], "content": "B"})
	raw := p.call("1", "fixture/write", string(params))
	for _, want := range []string{`"requestDispatched":true`, `"backendResponseReceived":true`, `"automaticReplay":false`, `"sideEffects":"unknown"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
	actual, _ := os.ReadFile(f.inputs[0])
	if string(actual) != "B" {
		t.Fatal("write did not execute")
	}
	next := p.snapshot("2", false)
	requireInput(t, next, "main.go", "B")
}

// VAL-ID-002 and MCP-002: roots originate at the actual client across refresh.
func TestRawProxyReverseRootsAcrossRefresh(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.initialize("2025-11-25")
	for i := 0; i < 2; i++ {
		if i > 0 {
			writeInput(t, f.inputs[0], "B", true)
		}
		raw := p.call(fmt.Sprint(i+1), "fixture/reverse", `{}`)
		if !bytes.Contains(raw, []byte(`file:///client-owned-root`)) {
			t.Fatalf("roots substituted: %s", raw)
		}
	}
	if len(p.reverseIDs) != 2 || p.reverseIDs[0] == p.reverseIDs[1] {
		t.Fatalf("reverse ID reused across processes: %v", p.reverseIDs)
	}
}

func TestRawProxyLegacyBatch(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.initialize("2025-03-26")
	p.send(`[{"jsonrpc":"2.0","id":1,"method":"fixture/echo","params":null},{"jsonrpc":"2.0","method":"fixture/notice","params":{"future":true}},{"jsonrpc":"2.0","id":"1","method":"fixture/error","params":{}}]`)
	raw := p.next()
	var responses []struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &responses); err != nil {
		t.Fatalf("batch response lost grouping: %s (%v)", raw, err)
	}
	if len(responses) != 2 || string(responses[0].ID) != "1" || string(responses[1].ID) != `"1"` || string(responses[0].Result) != "null" || len(responses[1].Error) == 0 {
		t.Fatalf("invalid mixed batch response: %s", raw)
	}
}

func TestRawProxyConfirmedLegacyStateRecovery(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.initialize("2025-11-25")
	p.call("1", "resources/subscribe", `{"uri":"fixture://keep"}`)
	p.call("2", "resources/subscribe", `{"uri":"fixture://cancel"}`)
	p.call("3", "resources/unsubscribe", `{"uri":"fixture://cancel"}`)
	p.call("4", "logging/setLevel", `{"level":"warning"}`)
	writeInput(t, f.inputs[0], "B", true)
	p.snapshot("5", false)
	counts := map[string]int{}
	for _, e := range f.events(t) {
		if e.Kind == "execution" && (e.Method == "resources/subscribe" || e.Method == "logging/setLevel") {
			if strings.Contains(e.Raw, "fixture://keep") {
				counts["keep"]++
			}
			if strings.Contains(e.Raw, "fixture://cancel") {
				counts["cancel"]++
			}
			if e.Method == "logging/setLevel" {
				counts["logging"]++
			}
		}
	}
	if counts["keep"] != 2 || counts["cancel"] != 1 || counts["logging"] != 2 {
		t.Fatalf("confirmed protocol state not restored correctly: %v", counts)
	}
}

func modernParams(extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"future":{"n":900719925474099312345}}` + extra + `}`
}

// VAL-MCP-005/006 and ID-003 normal reuse; each request carries its own metadata.
func TestRawProxyModernMetadataAndIDReuse(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	for i := 0; i < 3; i++ {
		params := modernParams(fmt.Sprintf(`"sequence":%d`, i))
		raw := p.call(`900719925474099312345`, "fixture/echo", params)
		want := fmt.Sprintf(`{"jsonrpc":"2.0","id":900719925474099312345,"result":%s,"x-fixture":{"untouched":900719925474099312345}}`, params)
		if string(raw) != want {
			t.Fatalf("per-request metadata changed: %s", raw)
		}
	}
	for _, e := range f.events(t) {
		if e.Method == "initialize" || e.Method == "server/discover" {
			t.Fatalf("proxy invented %s", e.Method)
		}
	}
}

// VAL-STATE-002/003: client alone sends continuation; old opaque token rejected
// by the new helper is returned exactly rather than repaired or restarted.
func TestRawProxyModernMRTRAcrossRefresh(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	first := p.call("1", "fixture/mrtr", modernParams(""))
	var reply struct {
		Result struct {
			State json.RawMessage `json:"requestState"`
		} `json:"result"`
	}
	if err := json.Unmarshal(first, &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Result.State) == 0 {
		t.Fatalf("missing MRTR state: %s", first)
	}
	writeInput(t, f.inputs[0], "B", true)
	raw := p.call("2", "fixture/mrtr", modernParams(`"requestState":`+string(reply.Result.State)))
	want := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"error":{"code":-32044,"message":"fixture token belongs to old process","data":{"requestState":%s}}}`, reply.Result.State)
	if string(raw) != want {
		t.Fatalf("opaque MRTR error changed:\n%s\nwant %s", raw, want)
	}
	calls := 0
	for _, e := range f.events(t) {
		if e.Kind == "execution" && e.Method == "fixture/mrtr" {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("proxy invented continuation or replay: %d executions", calls)
	}
}

// VAL-STATE-004 admission/ack/refresh subset: ack remains a live request and
// refresh ends it once, never replaying subscriptions/listen into the new child.
func TestRawProxyModernSubscriptionRefresh(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":"sub-A","method":"subscriptions/listen","params":%s}`, modernParams(`"notifications":{"toolsListChanged":true}`)))
	ack := p.next()
	if !bytes.Contains(ack, []byte(`"notifications/subscriptions/acknowledged"`)) || !bytes.Contains(ack, []byte(`"io.modelcontextprotocol/subscriptionId":"sub-A"`)) {
		t.Fatalf("invalid acknowledgment: %s", ack)
	}
	p.call("1", "fixture/subscriptionEvent", modernParams(""))
	if len(p.notifications) != 1 || !bytes.Contains(p.notifications[0], []byte(`"io.modelcontextprotocol/subscriptionId":"sub-A"`)) {
		t.Fatalf("subscription event missing or mapping lost: %q", p.notifications)
	}
	p.notifications = nil
	writeInput(t, f.inputs[0], "B", true)
	p.snapshot("2", true)
	ended := 0
	for _, raw := range p.notifications {
		if bytes.Contains(raw, []byte(`"notifications/cancelled"`)) && bytes.Contains(raw, []byte(`"requestId":"sub-A"`)) {
			ended++
		}
	}
	if ended != 1 {
		t.Fatalf("expected one subscription cancellation during refresh, got %d: %q", ended, p.notifications)
	}
	count := 0
	for _, e := range f.events(t) {
		if e.Kind == "execution" && e.Method == "subscriptions/listen" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("subscription was automatically replayed: %d", count)
	}
}

// VAL-INPUT-008 subset: with watcher disabled, an idle poll replaces a stale
// process before the client sends its next request.
func TestRawProxyIdlePollRefresh(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--poll-interval", "20ms"})
	p.initialize("2025-11-25")
	before := p.snapshot("1", false)
	writeInput(t, f.inputs[0], "B", true)
	deadline := time.Now().Add(3 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		for _, e := range f.events(t) {
			if e.Kind == "boot" && e.Boot != before.Boot {
				seen = true
			}
		}
		if seen {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !seen {
		t.Fatal("idle poll did not start a new backend")
	}
	after := p.snapshot("2", false)
	requireInput(t, after, "main.go", "B")
	if before.Boot == after.Boot {
		t.Fatal("idle scan reused stale process")
	}
}

// VAL-INPUT-004 event subset: observed same-byte writes invalidate the process.
func TestRawProxyWatcherSameContentInvalidates(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--disable-watcher=false"})
	p.initialize("2025-11-25")
	before := p.snapshot("1", false)
	writeInput(t, f.inputs[0], "A", false)
	deadline := time.Now().Add(3 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		for _, e := range f.events(t) {
			if e.Kind == "boot" && e.Boot != before.Boot {
				seen = true
			}
		}
		if seen {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !seen {
		t.Fatal("observed same-content write did not invalidate")
	}
	after := p.snapshot("2", false)
	if before.Boot == after.Boot {
		t.Fatal("same-content write kept old process")
	}
	requireInput(t, after, "main.go", "A")
}

// VAL-CANCEL-001 and ID-003 ambiguous-reuse subset. A cancelled sent ID is
// isolated from its old process before a newly received call can reuse it.
func TestRawProxyModernCancelAndReuseID(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"fixture/hold","params":%s}`, modernParams("")))
	f.waitExecution(t, "fixture/hold")
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"client cancelled"}}`)
	raw := p.call("7", "fixture/echo", modernParams(`"newInvocation":true`))
	if !bytes.Contains(raw, []byte(`"newInvocation":true`)) || bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("cancelled call leaked terminal or blocked legal new invocation: %s", raw)
	}
	var first, last string
	for _, e := range f.events(t) {
		if e.Kind != "execution" {
			continue
		}
		if e.Method == "fixture/hold" {
			first = e.Boot
		}
		if e.Method == "fixture/echo" {
			last = e.Boot
		}
	}
	if first == "" || last == "" || first == last {
		t.Fatalf("ambiguous reused ID was not isolated: hold=%s echo=%s", first, last)
	}
}

func TestRawProxyCancelQueuedRequest(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--max-concurrent", "1"})
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"fixture/hold","params":%s}`, modernParams("")))
	f.waitExecution(t, "fixture/hold")
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"fixture/echo","params":%s}`, modernParams(`"mustNeverExecute":true`)))
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	p.snapshot("3", true)
	for _, e := range f.events(t) {
		if e.Kind == "execution" && e.Method == "fixture/echo" {
			t.Fatalf("cancelled queued request executed: %s", e.Raw)
		}
	}
}

// VAL-MCP-009 and STATE-006: stable unknown initialization content passes, but
// unknown state must not be represented as transparently recovered.
func TestRawProxyUnknownInitializeRequiresReconnect(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	params := `{"protocolVersion":"2025-11-25","clientInfo":{"name":"unknown-state-client","version":"1"},"capabilities":{},"futureState":{"opaque":900719925474099312345}}`
	p.call("100", "initialize", params)
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s := p.snapshot("1", false)
	if !bytes.Contains(s.Initialize, []byte(`"futureState":{"opaque":900719925474099312345}`)) {
		t.Fatalf("unknown initialization fields lost: %s", s.Initialize)
	}
	writeInput(t, f.inputs[0], "B", true)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"fixture/snapshot","params":{}}`)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case raw, open := <-p.frames:
			if !open {
				return
			}
			if bytes.Contains(raw, []byte(`"result"`)) {
				t.Fatalf("unknown initialization state was hidden across restart: %s", raw)
			}
		case <-deadline.C:
			t.Fatal("unknown initialization did not require reconnect")
		}
	}
}

// VAL-LIFE-004 refresh-only subset; cancellation/crash paths have separate cases.
func TestRawProxyHundredRefreshes(t *testing.T) {
	if testing.Short() {
		t.Skip("100-process refresh stress excluded by -short")
	}
	f := newFixture(t)
	p := f.proxy(t)
	previous := p.snapshot("1", true)
	for i := 0; i < 100; i++ {
		value := fmt.Sprint(i)
		writeInput(t, f.inputs[0], value, true)
		current := p.snapshot(fmt.Sprint(i+2), true)
		requireInput(t, current, "main.go", value)
		if current.Boot == previous.Boot {
			t.Fatalf("iteration %d did not replace backend", i)
		}
		previous = current
	}
	boots := 0
	for _, e := range f.events(t) {
		if e.Kind == "boot" {
			boots++
		}
	}
	if boots != 101 {
		t.Fatalf("unexpected extra/missing spawns: %d", boots)
	}
	t.Logf("100 refreshes, 101 independent helper boots; proxy pid=%d", p.cmd.Process.Pid)
}

func TestRawProxyExternalWatchRootKeepsCWD(t *testing.T) {
	f := newFixture(t)
	external := filepath.Join(f.root, "external")
	if err := os.Mkdir(external, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(external, "resource.bin")
	if err := os.WriteFile(path, []byte("A"), 0600); err != nil {
		t.Fatal(err)
	}
	f.inputs = append(f.inputs, path)
	p := f.proxyFlags(t, []string{"--watch-root", external})
	p.initialize("2025-11-25")
	before := p.snapshot("1", false)
	writeInput(t, path, "B", true)
	after := p.snapshot("2", false)
	requireInput(t, after, "resource.bin", "B")
	actual, _ := filepath.EvalSymlinks(after.CWD)
	expected, _ := filepath.EvalSymlinks(f.workspace)
	if actual != expected || after.Boot == before.Boot {
		t.Fatalf("watch root changed cwd or failed to refresh: before=%+v after=%+v", before, after)
	}
}

func TestRawProxyAllQueuedBatchMembersCancelled(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--max-concurrent", "2"})
	p.initialize("2025-03-26")
	p.send(`{"jsonrpc":"2.0","id":1,"method":"fixture/hold","params":{}}`)
	f.waitExecution(t, "fixture/hold")
	p.send(`[{"jsonrpc":"2.0","id":2,"method":"fixture/echo","params":{"cancelled":2}},{"jsonrpc":"2.0","id":3,"method":"fixture/echo","params":{"cancelled":3}}]`)
	for _, id := range []int{2, 3, 1} {
		p.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d}}`, id))
	}
	p.snapshot("4", false)
	for _, e := range f.events(t) {
		if e.Kind == "execution" && e.Method == "fixture/echo" {
			t.Fatalf("cancelled batch member executed: %s", e.Raw)
		}
		if e.Kind == "receipt" && e.Raw == "[]" {
			t.Fatal("empty cancelled batch transmitted")
		}
	}
}

func TestRawProxyModernMultipleSubscriptions(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	for _, id := range []string{"sub-A", "sub-B"} {
		p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"subscriptions/listen","params":%s}`, id, modernParams(`"notifications":{"toolsListChanged":true}`)))
		ack := p.next()
		if !bytes.Contains(ack, []byte(`"notifications/subscriptions/acknowledged"`)) {
			t.Fatalf("subscription %s not acknowledged: %s", id, ack)
		}
	}
	p.call("1", "fixture/subscriptionEvent", modernParams(""))
	if len(p.notifications) != 2 {
		t.Fatalf("multiple subscriptions not independently live: %q", p.notifications)
	}
	p.notifications = nil
	p.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"sub-A"}}`)
	p.call("2", "fixture/subscriptionEvent", modernParams(""))
	if len(p.notifications) != 1 || !bytes.Contains(p.notifications[0], []byte(`"io.modelcontextprotocol/subscriptionId":"sub-B"`)) {
		t.Fatalf("cancelling A disturbed B: %q", p.notifications)
	}
	p.notifications = nil
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"fixture/subscriptionComplete","params":%s}`, modernParams(`"subscriptionId":"sub-B"`)))
	completion := p.response(`"sub-B"`)
	if !bytes.Contains(completion, []byte(`"resultType":"complete"`)) {
		t.Fatalf("completion not delivered: %s", completion)
	}
	p.response("3")
	writeInput(t, f.inputs[0], "B", true)
	p.snapshot("4", true)
	if len(p.notifications) != 0 {
		t.Fatalf("closed subscriptions emitted ghost terminal after refresh: %q", p.notifications)
	}
	count := 0
	for _, e := range f.events(t) {
		if e.Kind == "execution" && e.Method == "subscriptions/listen" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("closed subscriptions replayed: %d", count)
	}
}

// VAL-NOREPLAY-004 crash subset: every member of a written batch is possibly
// dispatched even when the fixture crashes before writing any response.
func TestRawProxyDispatchedBatchNeverReplayed(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	p.initialize("2025-03-26")
	p.send(`[{"jsonrpc":"2.0","id":1,"method":"fixture/hold","params":{}},{"jsonrpc":"2.0","id":2,"method":"fixture/crash","params":{}},{"jsonrpc":"2.0","method":"fixture/notice","params":{}}]`)
	raw := p.next()
	var replies []struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Data struct {
				Dispatched bool `json:"requestDispatched"`
				Replay     bool `json:"automaticReplay"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &replies); err != nil {
		t.Fatalf("batch terminals lost grouping: %s", raw)
	}
	if len(replies) != 2 {
		t.Fatalf("notification acquired response or batch member missing: %s", raw)
	}
	for _, reply := range replies {
		if !reply.Error.Data.Dispatched || reply.Error.Data.Replay {
			t.Fatalf("batch uncertainty not retained: %s", raw)
		}
	}
	p.snapshot("3", false)
	counts := map[string]int{}
	for _, e := range f.events(t) {
		if e.Kind == "execution" {
			counts[e.Method]++
		}
	}
	if counts["fixture/hold"] != 1 || counts["fixture/crash"] != 1 {
		t.Fatalf("written batch was replayed: %v", counts)
	}
}

func TestRawProxyFrameLimitTerminatesWithoutPartialDispatch(t *testing.T) {
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--max-frame-bytes", "512"})
	p.call("1", "fixture/echo", modernParams(""))
	params := modernParams(`"large":"` + strings.Repeat("x", 1024) + `"`)
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"fixture/echo","params":%s}`, params))
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case raw, open := <-p.frames:
			if !open {
				for _, e := range f.events(t) {
					if e.Kind == "execution" && string(e.ID) == "2" {
						t.Fatalf("oversized frame partly dispatched: %s", e.Raw)
					}
				}
				return
			}
			if bytes.Contains(raw, []byte(`"result"`)) {
				t.Fatalf("oversized frame returned success: %s", raw)
			}
		case <-timer.C:
			t.Fatal("oversized frame did not terminate within budget")
		}
	}
}

// VAL-LIFE-004: deterministic mixed fault sequence and independent execution /
// process evidence. The seed is printed so any failing order is reproducible.
func TestRawProxyHundredMixedFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("100-operation mixed-fault stress excluded by -short")
	}
	const seed int64 = 703031
	random := rand.New(rand.NewSource(seed))
	t.Logf("mixed-fault seed=%d", seed)
	f := newFixture(t)
	p := f.proxyFlags(t, []string{"--drain-timeout", "25ms"})
	p.snapshot(`"initial"`, true)
	for i := 0; i < 100; i++ {
		kind := random.Intn(5)
		id := fmt.Sprintf(`"op-%d"`, i)
		params := modernParams(fmt.Sprintf(`"invocation":%d`, i))
		t.Logf("seed=%d iteration=%d operation=%d", seed, i, kind)
		switch kind {
		case 0:
			writeInput(t, f.inputs[0], fmt.Sprint(i), true)
			p.snapshot(id, true)
		case 1:
			raw := p.call(`"reused-id"`, "fixture/echo", params)
			if !bytes.Contains(raw, []byte(fmt.Sprintf(`"invocation":%d`, i))) {
				t.Fatalf("reused ID response mismatch: %s", raw)
			}
		case 2:
			p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"fixture/hold","params":%s}`, id, params))
			waitHelperID(t, f, id, "fixture/hold")
			p.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s}}`, id))
			p.snapshot(fmt.Sprintf(`"after-%d"`, i), true)
		case 3, 4:
			method := "fixture/crash"
			if kind == 4 {
				method = "fixture/half"
			}
			raw := p.call(id, method, params)
			if !bytes.Contains(raw, []byte(`"requestDispatched":true`)) || !bytes.Contains(raw, []byte(`"automaticReplay":false`)) {
				t.Fatalf("mixed fault lost dispatch evidence: %s", raw)
			}
			p.snapshot(fmt.Sprintf(`"after-%d"`, i), true)
		}
	}
	p.shutdown()
	counts := map[string]int{}
	boots := map[string]int{}
	for _, e := range f.events(t) {
		if e.Kind == "boot" {
			boots[e.Boot] = e.PID
		}
		if e.Kind != "execution" || !strings.HasPrefix(e.Method, "fixture/") {
			continue
		}
		var request struct {
			Params struct {
				Invocation json.RawMessage `json:"invocation"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(e.Raw), &request)
		key := string(e.ID) + "|" + e.Method + "|" + string(request.Params.Invocation)
		counts[key]++
		if counts[key] > 1 {
			t.Fatalf("automatic business replay for %s: %s", key, e.Raw)
		}
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		for boot, pid := range boots {
			process, err := os.FindProcess(pid)
			if err == nil && process.Signal(syscall.Signal(0)) == nil {
				t.Errorf("helper still alive after cleanup: boot=%s pid=%d", boot, pid)
			}
		}
	}
	t.Logf("seed=%d operations=100 independent_boots=%d unique_executions=%d cleanup=complete", seed, len(boots), len(counts))
}

func waitHelperID(t *testing.T, f *fixture, id, method string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range f.events(t) {
			if e.Kind == "execution" && string(e.ID) == id && e.Method == method {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("helper never recorded %s id %s", method, id)
}

func TestRawProxyUnknownRevisionReconnect(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	params := `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01","io.modelcontextprotocol/clientCapabilities":{},"future":true}}`
	raw := p.call("1", "fixture/echo", params)
	if !bytes.Contains(raw, []byte(params)) {
		t.Fatalf("unknown stable profile not preserved: %s", raw)
	}
	writeInput(t, f.inputs[0], "B", true)
	p.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"fixture/echo","params":%s}`, params))
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case raw, open := <-p.frames:
			if !open {
				return
			}
			if bytes.Contains(raw, []byte(`"result"`)) {
				t.Fatalf("unknown profile silently recovered: %s", raw)
			}
		case <-timer.C:
			t.Fatal("unknown profile did not require reconnect")
		}
	}
}

func TestRawProxyDiscoveryPreservesDifferentErrors(t *testing.T) {
	f := newFixture(t)
	p := f.proxy(t)
	for i, code := range []int{-32601, -32042, 1234} {
		id := fmt.Sprint(i + 1)
		raw := p.call(id, "server/discover", modernParams(fmt.Sprintf(`"fixtureErrorCode":%d`, code)))
		want := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"fixture discovery rejected","data":{"opaque":900719925474099312345}}}`, id, code)
		if string(raw) != want {
			t.Fatalf("discovery error normalized:\n%s\nwant %s", raw, want)
		}
	}
}
