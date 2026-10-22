package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A bounded real-backend gate. Use GOPLS_LIFECYCLE_CYCLES=100 for soak evidence;
// the PR matrix uses three independent calls and keeps performance non-race.
// Observations are scoped to attributed processes, not arbitrary descendants.
func TestRealGoplsOnDemandLifecycle(t *testing.T) {
	if os.Getenv("GOPLS_INTEGRATION") != "1" {
		t.Skip("set GOPLS_INTEGRATION=1; lifecycle CI rejects this skip")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Fatalf("lifecycle observation unsupported on %s", runtime.GOOS)
	}
	cycles := 3
	if value := os.Getenv("GOPLS_LIFECYCLE_CYCLES"); value != "" {
		var err error
		cycles, err = strconv.Atoi(value)
		if err != nil || cycles < 3 || cycles > 1000 {
			t.Fatal("GOPLS_LIFECYCLE_CYCLES must be 3..1000")
		}
	}
	for _, mode := range []string{"resident", "on-demand"} {
		t.Run(mode, func(t *testing.T) { testRealLifecycle(t, mode, cycles, false) })
	}
}

type lifecycleBoot struct {
	Time    time.Time `json:"time"`
	Message string    `json:"msg"`
	Boot    uint64    `json:"boot_id"`
	PID     int       `json:"pid"`
}

func readLifecycleBoots(t *testing.T, path string) []lifecycleBoot {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var boots []lifecycleBoot
	seen := map[int]bool{}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var event lifecycleBoot
		if json.Unmarshal(line, &event) == nil && event.Message == "backend started" {
			if seen[event.PID] {
				t.Fatalf("boot PID reused: pid=%d; observer-to-boot mapping is ambiguous (unverified)", event.PID)
			}
			seen[event.PID] = true
			boots = append(boots, event)
		}
	}
	return boots
}
func testRealLifecycle(t *testing.T, mode string, cycles int, killParent bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
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
		t.Fatalf("version: %v %s", err, version)
	}
	requireGoplsVersion(t, version)
	root := t.TempDir()
	archiveEvidence(t, root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err = os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	mainFile := filepath.Join(workspace, "main.go")
	for path, value := range map[string]string{filepath.Join(workspace, "go.mod"): "module example.com/lifecycle\n\ngo 1.25.0\n", mainFile: "package fixture\nimport \"net/http\"\nvar LifecycleHTTP = http.MethodGet\nconst LifecycleSymbol = 1\n"} {
		if err = os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	idleTimeout := "300ms"
	if killParent {
		idleTimeout = "3s"
	}
	cmd := exec.CommandContext(ctx, proxyBinary(t), "--refresh-mode", "per-call", "--backend-lifecycle", mode, "--backend-idle-timeout", idleTimeout, "--disable-watcher", "--poll-interval", "1h", "--debounce", "1ms", "--", gopls, "mcp")
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOENV=off")
	p := startPeer(t, cmd, filepath.Join(root, "proxy.log"))
	p.timeout = 95 * time.Second
	p.roots = []map[string]string{{"uri": (&url.URL{Scheme: "file", Path: filepath.ToSlash(workspace)}).String(), "name": "lifecycle workspace"}}
	observer := startLifecycleObserver(t, p.cmd.Process.Pid)
	// Register after the peer: evidence is written while the peer and observer
	// are still live, including failures. archiveEvidence runs last.
	t.Cleanup(func() {
		observer.close()
		if err := observer.write(filepath.Join(root, "process-samples.json")); err != nil {
			t.Error(err)
		}
	})
	type exchange struct {
		Method    string          `json:"method"`
		ElapsedMS int64           `json:"elapsed_ms"`
		Response  json.RawMessage `json:"response"`
		At        time.Time       `json:"at"`
	}
	var exchanges []exchange
	var latencies []int64
	var dormantFDs []int
	call := func(id, method, params string) []byte {
		t.Helper()
		start := time.Now()
		raw := p.call(id, method, params)
		exchanges = append(exchanges, exchange{method, time.Since(start).Milliseconds(), json.RawMessage(raw), start.UTC()})
		var result struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || len(result.Error) > 0 || result.Result.IsError {
			t.Fatalf("native %s failed: %s (%v)", method, raw, err)
		}
		return raw
	}
	if len(readLifecycleBoots(t, p.log)) != 0 {
		t.Fatal("backend started without MCP demand")
	}
	call("1", "initialize", `{"protocolVersion":"2025-11-25","clientInfo":{"name":"lifecycle-acceptance","version":"1"},"capabilities":{"roots":{"listChanged":true}}}`)
	p.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	call("2", "tools/list", `{}`)
	initialBoots := readLifecycleBoots(t, p.log)
	if len(initialBoots) != 1 {
		t.Fatalf("initialize starts=%d", len(initialBoots))
	}
	if killParent {
		killObservedLifecycleMain(t, observer, initialBoots[0].PID)
	}
	if mode == "on-demand" {
		waitLifecycleBootExit(t, observer, initialBoots[0].PID, 7*time.Second)
		observer.waitEmpty(t, 6*time.Second, 6*time.Second)
	}
	for i := 0; i < cycles; i++ {
		params, _ := json.Marshal(map[string]any{"name": "go_diagnostics", "arguments": map[string]any{"files": []string{mainFile}}})
		start := time.Now()
		reply := call(strconv.Itoa(i+10), "tools/call", string(params))
		latencies = append(latencies, time.Since(start).Milliseconds())
		if !bytes.Contains(reply, []byte("No diagnostics.")) {
			t.Fatalf("unexpected native diagnostics: %s", reply)
		}
		boots := readLifecycleBoots(t, p.log)
		if len(boots) != i+2 {
			t.Fatalf("each tools/call needs one new main: calls=%d starts=%d", i+1, len(boots))
		}
		current := boots[len(boots)-1]
		if mode == "on-demand" {
			waitLifecycleBootExit(t, observer, current.PID, 7*time.Second)
			observer.waitEmpty(t, 6*time.Second, 6*time.Second)
			last := observer.latest(t)
			dormantFDs = append(dormantFDs, last.ProxyFD)
		} else {
			time.Sleep(80 * time.Millisecond)
			last := observer.latest(t)
			if last.MainCount != 1 {
				t.Fatalf("resident must retain main backend after tools/call, count=%d", last.MainCount)
			}
		}
		// Both modes receive the same edits and idle interval. The on-demand
		// branch must remain dormant until a client message needs the backend.
		if i+1 < cycles {
			if err = os.WriteFile(mainFile, []byte(fmt.Sprintf("package fixture\nimport \"net/http\"\nvar LifecycleHTTP = http.MethodGet\nconst LifecycleSymbol = %d\n", i+2)), 0600); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(80 * time.Millisecond)
		if len(readLifecycleBoots(t, p.log)) != len(boots) {
			t.Fatal("file change/no demand started a backend")
		}
	}
	if runtime.GOOS == "linux" && len(dormantFDs) > 1 && dormantFDs[len(dormantFDs)-1] > dormantFDs[0]+2 {
		t.Errorf("dormant proxy FDs accumulated: %v", dormantFDs)
	}
	// A real ping after a tool must wake on-demand, and reuse resident.
	before := len(readLifecycleBoots(t, p.log))
	call("9000", "ping", `{}`)
	after := len(readLifecycleBoots(t, p.log))
	if mode == "on-demand" && after != before+1 || mode == "resident" && after != before {
		t.Fatalf("ping starts mode=%s before=%d after=%d", mode, before, after)
	}
	if mode == "on-demand" {
		boots := readLifecycleBoots(t, p.log)
		waitLifecycleBootExit(t, observer, boots[len(boots)-1].PID, 7*time.Second)
		observer.waitEmpty(t, 6*time.Second, 6*time.Second)
	}
	p.shutdown()
	observer.waitEmpty(t, 6*time.Second, 6*time.Second)
	observer.close()
	boots := readLifecycleBoots(t, p.log)
	if len(observer.errors) > 0 || len(observer.samples) == 0 {
		t.Fatalf("observation unverified: %v", observer.errors)
	}
	peak := int64(0)
	liveSamples := 0
	sidecars := 0
	mainIDs := map[int]string{}
	sidecarParents := map[string]bool{}
	fdMin, fdMax := -1, 0
	for id, owned := range observer.owned {
		if owned.Kind == "main" {
			if old := mainIDs[owned.Process.NamespacePID]; old != "" && old != id {
				t.Errorf("main PID lifetime mapping ambiguous: pid=%d", owned.Process.NamespacePID)
			}
			mainIDs[owned.Process.NamespacePID] = id
		}
		if owned.Kind == "telemetry" {
			sidecars++
			sidecarParents[owned.Parent] = true
			parent := observer.owned[owned.Parent]
			if parent == nil || parent.ExitMS == nil || owned.GoneMS == nil {
				t.Errorf("telemetry exit timing unverified for %s", id)
			} else if *owned.GoneMS-*parent.ExitMS > 6100 {
				t.Errorf("telemetry %s remained after parent exit for %dms", id, *owned.GoneMS-*parent.ExitMS)
			}
		}
		if owned.ExitMS == nil || owned.GoneMS == nil {
			t.Errorf("owned process still alive: %+v", owned)
		}
	}
	for _, b := range boots {
		if mainIDs[b.PID] == "" {
			t.Errorf("main boot %d pid %d observation unverified", b.Boot, b.PID)
		} else if !sidecarParents[mainIDs[b.PID]] {
			t.Errorf("telemetry coverage unverified for boot %d pid %d", b.Boot, b.PID)
		}
	}
	// The supported gopls builds normally create a telemetry crashmonitor. If it
	// cannot be attributed, do not silently call the tree-cleanup check a pass.
	if sidecars == 0 {
		t.Error("telemetry observation unverified: no owned sidecar was sampled")
	}
	for _, s := range observer.samples {
		sum := s.ProxyRSS + s.MainRSS + s.SidecarRSS
		if sum > peak {
			peak = sum
		}
		if s.MainCount > 0 {
			liveSamples++
		}
		if s.MainCount > 1 {
			t.Errorf("overlapping live main backends: %+v", s)
		}
		if s.ProxyFD >= 0 {
			if fdMin < 0 || s.ProxyFD < fdMin {
				fdMin = s.ProxyFD
			}
			if s.ProxyFD > fdMax {
				fdMax = s.ProxyFD
			}
		}
	}
	sampleCount := len(observer.samples)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	digest := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(data))
	}
	buildInfo, _ := exec.CommandContext(ctx, "go", "version", "-m", gopls).CombinedOutput()
	summary := map[string]any{
		"mode":                  mode,
		"cycles":                cycles,
		"initial_parent_killed": killParent,
		"gopls_version":         strings.TrimSpace(string(version)),
		"gopls_build_info":      string(buildInfo),
		"gopls_path":            gopls, "gopls_sha256": digest(gopls), "proxy_sha256": digest(cmd.Path),
		"os":                           runtime.GOOS,
		"arch":                         runtime.GOARCH,
		"argv":                         cmd.Args,
		"workspace_scope":              "synthetic single-package Go workspace with net/http dependency",
		"cache_policy":                 "ambient warm Go build/module cache; modes run sequentially",
		"race":                         integrationRace,
		"telemetry_observed":           sidecars,
		"backend_start_count":          len(boots),
		"sampled_residency_ratio":      float64(liveSamples) / float64(sampleCount),
		"peak_sampled_group_rss_bytes": peak,
		"rss_scope":                    "sum within each process-table sample then maximum; OS reads are sequential, not atomic",
		"dormant_proxy_fd_counts":      dormantFDs,
		"proxy_fd_min":                 fdMin,
		"proxy_fd_max":                 fdMax, "call_p50_ms": latencies[(len(latencies)-1)/2], "call_p95_ms": latencies[(len(latencies)-1)*95/100],
		"result_exit_timing": "use proxy successful-write/cleanup logs plus sampled process exit; client receipt is not write ACK",
		"real_codex_status":  "not run",
		"scope":              "attributed gopls/telemetry only; no all-descendant runtime guarantee",
		"boots":              boots,
		"exchanges":          exchanges,
		"measurement_scope":  "synthetic correctness/soak; sampler overhead and OS scheduling affect timing; not a controlled production A/B benchmark",
	}
	raw, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "lifecycle-summary.json"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("mode=%s cycles=%d starts=%d telemetry=%d sampled_residency=%.3f peak_group_rss=%d", mode, cycles, len(boots), sidecars, float64(liveSamples)/float64(sampleCount), peak)
}
func waitLifecycleBootExit(t *testing.T, o *lifecycleObserver, pid int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		seen, exited := false, false
		for _, p := range o.owned {
			if p.Kind == "main" && p.Process.NamespacePID == pid {
				seen = true
				exited = p.ExitMS != nil
				break
			}
		}
		o.mu.Unlock()
		if seen && exited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("main pid %d did not have an observed live-to-exit transition within %s (missing coverage is unverified)", pid, budget)
}

// Killing only the recorded, identity-checked backend exercises telemetry's
// crash-pipe EOF handling after an abnormal parent exit. The next tools/call
// must still work; no dispatched operation is replayed by this test.
func TestRealGoplsOnDemandKilledParentSidecarExit(t *testing.T) {
	if os.Getenv("GOPLS_INTEGRATION") != "1" {
		t.Skip("set GOPLS_INTEGRATION=1; lifecycle CI rejects this skip")
	}
	testRealLifecycle(t, "on-demand", 3, true)
}
func killObservedLifecycleMain(t *testing.T, o *lifecycleObserver, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		var main lifecycleProcess
		hasSidecar := false
		for key, p := range o.owned {
			if p.Kind == "main" && p.Process.NamespacePID == pid && p.ExitMS == nil {
				main = p.Process
				for _, child := range o.owned {
					if child.Kind == "telemetry" && child.Parent == key && child.ExitMS == nil {
						hasSidecar = true
					}
				}
			}
		}
		o.mu.Unlock()
		if hasSidecar {
			rows, err := readLifecycleProcesses()
			if err != nil {
				t.Fatal(err)
			}
			for _, current := range rows {
				if current.identity() == main.identity() && current.live() {
					child, err := os.FindProcess(pid)
					if err != nil {
						t.Fatal(err)
					}
					if err = child.Kill(); err != nil {
						t.Fatal(err)
					}
					return
				}
			}
			t.Fatal("recorded main identity changed before fault injection")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("main + telemetry ownership not established before bounded fault injection")
}
