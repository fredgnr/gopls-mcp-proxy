package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type latencyProfile struct {
	Samples int     `json:"samples"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
}

type performanceProfile struct {
	GoVersion          string         `json:"go_version"`
	OS                 string         `json:"os"`
	Arch               string         `json:"arch"`
	CPUs               int            `json:"logical_cpus"`
	Race               bool           `json:"race"`
	Files              int            `json:"files"`
	InputBytes         int            `json:"input_bytes"`
	Cache              string         `json:"cache"`
	MaxConcurrent      int            `json:"configured_max_concurrent"`
	DirectSingle       latencyProfile `json:"direct_single"`
	ProxySingle        latencyProfile `json:"proxy_single"`
	DirectOutstanding8 latencyProfile `json:"direct_outstanding_8"`
	ProxyOutstanding8  latencyProfile `json:"proxy_outstanding_8"`
	RefreshReady       latencyProfile `json:"refresh_ready_observed"`
	RefreshResponse    latencyProfile `json:"refresh_response"`
	DirectPeakRSSBytes *int64         `json:"direct_process_peak_rss_bytes,omitempty"`
	ProxyPeakRSSBytes  *int64         `json:"proxy_process_peak_rss_bytes,omitempty"`
}

func summarizeLatencies(values []time.Duration) latencyProfile {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	if len(ordered) == 0 {
		return latencyProfile{}
	}
	return latencyProfile{Samples: len(ordered), P50MS: float64(ordered[(len(ordered)-1)/2]) / float64(time.Millisecond), P95MS: float64(ordered[(95*len(ordered)+99)/100-1]) / float64(time.Millisecond)}
}

// TestPerformanceProfile is opt-in because the 50k-file case intentionally
// exercises repeated strict full-content dispatch and delivery checkpoints.
// Inputs and raw fixture behavior are fixed. One untimed RPC warms each peer.
// "Outstanding 8" means eight sent requests awaiting replies; it does not claim
// eight backend executions overlap. Refresh-ready timing polls the actual ready
// log every millisecond, so it includes <= one poll interval plus scheduling
// delay. Linux VmHWM is process-only peak RSS, excluding backend descendants.
//
// PROXY_PERF=1 go test ./integration -run '^TestPerformanceProfile$' -count=1 -v
func TestPerformanceProfile(t *testing.T) {
	if os.Getenv("PROXY_PERF") != "1" {
		t.Skip("set PROXY_PERF=1 for the explicitly sized performance profile")
	}
	for _, count := range []int{1000, 10000, 50000} {
		t.Run(fmt.Sprintf("files_%d", count), func(t *testing.T) {
			f := newFixture(t)
			const size = 128
			content := []byte(strings.Repeat("abcdefgh", size/8))
			for _, path := range f.inputs {
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i := len(f.inputs); i < count; i++ {
				dir := filepath.Join(f.workspace, fmt.Sprintf("shard_%03d", i/1000))
				if i == len(f.inputs) || i%1000 == 0 {
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("input_%06d.bin", i)), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			direct := f.direct(t)
			proxy := f.proxyFlags(t, []string{"--max-concurrent", "8", "--request-timeout", "60s", "--startup-timeout", "20s", "--refresh-timeout", "60s"})
			direct.timeout, proxy.timeout = 70*time.Second, 70*time.Second
			direct.initialize("2025-11-25")
			proxy.initialize("2025-11-25")
			direct.snapshot("101", false)
			last := proxy.snapshot("101", false)
			nextID := 1000
			measureRawCalls(t, direct, &nextID, 1, 1)
			measureRawCalls(t, proxy, &nextID, 1, 1)
			profile := performanceProfile{GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Race: integrationRace, Files: count, InputBytes: count * size, Cache: "warm; one untimed fixture/snapshot and echo per connection; no OS cache eviction", MaxConcurrent: 8}
			profile.DirectSingle = summarizeLatencies(measureRawCalls(t, direct, &nextID, 10, 1))
			profile.ProxySingle = summarizeLatencies(measureRawCalls(t, proxy, &nextID, 10, 1))
			profile.DirectOutstanding8 = summarizeLatencies(measureRawCalls(t, direct, &nextID, 2, 8))
			profile.ProxyOutstanding8 = summarizeLatencies(measureRawCalls(t, proxy, &nextID, 2, 8))
			var ready, response []time.Duration
			for sample := 0; sample < 10; sample++ {
				before := countReadyLogs(t, proxy.log)
				value := fmt.Sprintf("%0128d", sample)
				writeInput(t, f.inputs[0], value, true)
				id := strconv.Itoa(nextID)
				nextID++
				start := time.Now()
				proxy.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"fixture/snapshot","params":{}}`, id))
				waitReadyLog(t, proxy, before)
				ready = append(ready, time.Since(start))
				raw := proxy.response(id)
				response = append(response, time.Since(start))
				var message struct {
					Result struct {
						Boot   string             `json:"boot"`
						PID    int                `json:"pid"`
						Inputs map[string]*string `json:"inputs"`
					} `json:"result"`
					Error json.RawMessage `json:"error"`
				}
				if err := json.Unmarshal(raw, &message); err != nil {
					t.Fatal(err)
				}
				if len(message.Error) != 0 || message.Result.Boot == "" || message.Result.Boot == last.Boot || message.Result.PID == last.PID {
					t.Fatalf("refresh did not establish distinct successful backend: %s", raw)
				}
				if input := message.Result.Inputs["main.go"]; input == nil || *input != value {
					t.Fatalf("new backend input mismatch: %s", raw)
				}
				last.Boot, last.PID = message.Result.Boot, message.Result.PID
			}
			profile.RefreshReady, profile.RefreshResponse = summarizeLatencies(ready), summarizeLatencies(response)
			profile.DirectPeakRSSBytes, profile.ProxyPeakRSSBytes = processPeakRSS(direct.cmd.Process.Pid), processPeakRSS(proxy.cmd.Process.Pid)
			encoded, err := json.MarshalIndent(profile, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.root, "performance.json"), append(encoded, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			compact, _ := json.Marshal(profile)
			t.Logf("PERF_PROFILE %s", compact)
		})
	}
}

func measureRawCalls(t *testing.T, peer *rawPeer, nextID *int, waves, outstanding int) []time.Duration {
	t.Helper()
	values := make([]time.Duration, 0, waves*outstanding)
	for wave := 0; wave < waves; wave++ {
		pending := make(map[string]time.Time, outstanding)
		for i := 0; i < outstanding; i++ {
			id := strconv.Itoa(*nextID)
			*nextID++
			pending[id] = time.Now()
			peer.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"fixture/echo","params":{"payload":"0123456789abcdef"}}`, id))
		}
		for len(pending) > 0 {
			raw := peer.next()
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(raw, &message); err != nil {
				t.Fatal(err)
			}
			if message.Method != "" {
				t.Fatalf("unexpected control traffic in stable echo profile: %s", raw)
			}
			start, ok := pending[string(message.ID)]
			if !ok || len(message.Error) != 0 || !bytes.Contains(message.Result, []byte(`"0123456789abcdef"`)) {
				t.Fatalf("profile response mismatch: %s", raw)
			}
			values = append(values, time.Since(start))
			delete(pending, string(message.ID))
		}
	}
	return values
}

func countReadyLogs(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte(`"msg":"backend ready"`))
}

func waitReadyLog(t *testing.T, peer *rawPeer, previous int) {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if countReadyLogs(t, peer.log) > previous {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			data, _ := os.ReadFile(peer.log)
			t.Fatalf("refresh ready measurement timed out:\n%s", data)
		}
	}
}

func processPeakRSS(pid int) *int64 {
	if runtime.GOOS != "linux" {
		return nil
	}
	// Some sandboxes expose a host /proc inside a nested PID namespace. A
	// numeric /proc/<child PID> path can then name an unrelated host process.
	// Omit this optional metric unless the process namespace is confirmed.
	selfNamespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return nil
	}
	childNamespace, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil || selfNamespace != childNamespace {
		return nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmHWM:" && fields[2] == "kB" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return nil
			}
			value := kib * 1024
			return &value
		}
	}
	return nil
}
