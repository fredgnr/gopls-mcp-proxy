package integration

// This observer runs outside the proxy and never sends signals. Attribution is
// established while PPID links are observable, then retained across reparenting.
// It is a sampler, not a proof that arbitrary daemonized descendants were seen.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type lifecycleProcess struct {
	PID          int    `json:"pid"`
	NamespacePID int    `json:"namespace_pid"`
	PPID         int    `json:"ppid"`
	PGID         int    `json:"pgid"`
	SID          string `json:"sid"`
	Start        string `json:"start_identity"`
	State        string `json:"state"`
	RSS          int64  `json:"rss_bytes"`
	Command      string `json:"command"`
}

func (p lifecycleProcess) identity() string { return fmt.Sprintf("%d:%s", p.PID, p.Start) }
func (p lifecycleProcess) live() bool       { return p.State != "Z" && p.State != "X" }

type lifecycleOwned struct {
	Process lifecycleProcess `json:"process"`
	Kind    string           `json:"kind"`
	Parent  string           `json:"parent_identity,omitempty"`
	FirstMS int64            `json:"first_seen_ms"`
	LastMS  int64            `json:"last_seen_ms"`
	GoneMS  *int64           `json:"first_observed_absent_ms,omitempty"`
	ExitMS  *int64           `json:"first_observed_exit_ms,omitempty"`
	Samples int              `json:"samples"`
}
type lifecycleSample struct {
	AtMS         int64              `json:"at_ms"`
	MainCount    int                `json:"backend_live_count"`
	SidecarCount int                `json:"owned_sidecar_live_count"`
	ProxyRSS     int64              `json:"rss_proxy"`
	MainRSS      int64              `json:"rss_backend"`
	SidecarRSS   int64              `json:"rss_sidecar"`
	ProxyFD      int                `json:"proxy_fd_count"`
	Processes    []lifecycleProcess `json:"processes"`
}
type lifecycleObserver struct {
	mu            sync.Mutex
	pid           int
	proxyIdentity string
	started       time.Time
	owned         map[string]*lifecycleOwned
	samples       []lifecycleSample
	errors        []string
	stop          chan struct{}
	done          chan struct{}
}

func startLifecycleObserver(t *testing.T, pid int) *lifecycleObserver {
	t.Helper()
	processes, err := readLifecycleProcesses()
	if err != nil {
		t.Fatalf("process observation unverified: %v", err)
	}
	hostPID := 0
	proxyIdentity := ""
	for _, p := range processes {
		if p.NamespacePID == pid {
			hostPID = p.PID
			proxyIdentity = p.identity()
			break
		}
	}
	if hostPID == 0 {
		t.Fatalf("proxy pid %d cannot be mapped into observed process namespace", pid)
	}
	o := &lifecycleObserver{pid: hostPID, proxyIdentity: proxyIdentity, started: time.Now(), owned: map[string]*lifecycleOwned{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			o.sample()
			select {
			case <-o.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	t.Cleanup(func() { o.close() })
	return o
}
func (o *lifecycleObserver) close() {
	select {
	case <-o.done:
		return
	default:
	}
	select {
	case <-o.stop:
	default:
		close(o.stop)
	}
	<-o.done
}
func (o *lifecycleObserver) sample() {
	processes, err := readLifecycleProcesses()
	o.observe(processes, err)
}
func (o *lifecycleObserver) observe(processes []lifecycleProcess, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		o.errors = append(o.errors, err.Error())
		return
	}
	at := time.Since(o.started).Milliseconds()
	sample := lifecycleSample{AtMS: at, ProxyFD: -1}
	present := map[string]lifecycleProcess{}
	for _, p := range processes {
		present[p.identity()] = p
	}
	// Discover only direct proxy children as main backends. No name-only or
	// process-group-only attribution is used.
	for _, p := range processes {
		proxy, proxyPresent := present[o.proxyIdentity]
		if proxyPresent && proxy.live() && p.PPID == o.pid && p.live() {
			key := p.identity()
			if o.owned[key] == nil {
				o.owned[key] = &lifecycleOwned{Process: p, Kind: "main", FirstMS: at}
			}
		}
	}
	// Iteration allows descendants at any observed depth, including telemetry
	// with a different session. The parent must be alive in this SAME sample.
	for changed := true; changed; {
		changed = false
		for _, p := range processes {
			if o.owned[p.identity()] != nil || !p.live() {
				continue
			}
			for key, parent := range o.owned {
				current, ok := present[key]
				if ok && current.live() && p.PPID == parent.Process.PID {
					kind := "descendant"
					if strings.Contains(p.Command, "telemetry") {
						kind = "telemetry"
					}
					o.owned[p.identity()] = &lifecycleOwned{Process: p, Kind: kind, Parent: key, FirstMS: at}
					changed = true
					break
				}
			}
		}
	}
	for key, owned := range o.owned {
		p, ok := present[key]
		if ok && p.NamespacePID == 0 {
			p.NamespacePID = owned.Process.NamespacePID
		} // immutable alias may be unreadable after exit
		if ok && p.SID == "unavailable:zombie" && owned.Process.SID != "" {
			p.SID = owned.Process.SID
		}
		if ok && owned.Kind == "descendant" && strings.Contains(p.Command, "telemetry") {
			owned.Kind = "telemetry"
		} // fork may be sampled before exec
		if !ok || !p.live() {
			if ok {
				owned.Process = p
			} else if owned.GoneMS == nil {
				gone := at
				owned.GoneMS = &gone
			}
			if owned.ExitMS == nil {
				exit := at
				owned.ExitMS = &exit
			}
			continue
		}
		owned.LastMS = at
		owned.Samples++
		owned.Process = p
		sample.Processes = append(sample.Processes, p)
		if owned.Kind == "main" {
			sample.MainCount++
			sample.MainRSS += p.RSS
		}
		if owned.Kind == "telemetry" {
			sample.SidecarCount++
			sample.SidecarRSS += p.RSS
		}
	}
	for _, p := range processes {
		if p.identity() == o.proxyIdentity {
			sample.ProxyRSS = p.RSS
			sample.Processes = append(sample.Processes, p)
			break
		}
	}
	if proxy, ok := present[o.proxyIdentity]; runtime.GOOS == "linux" && ok && proxy.live() {
		if fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", o.pid)); err == nil {
			sample.ProxyFD = len(fds)
		} else if !os.IsNotExist(err) && !errors.Is(err, syscall.ESRCH) {
			o.errors = append(o.errors, fmt.Sprintf("proxy FD observation: %v", err))
		}
	}
	sort.Slice(sample.Processes, func(i, j int) bool { return sample.Processes[i].PID < sample.Processes[j].PID })
	o.samples = append(o.samples, sample)
}
func (o *lifecycleObserver) write(path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	status := "observed"
	if len(o.errors) > 0 || len(o.samples) == 0 {
		status = "unverified"
	}
	data, err := json.MarshalIndent(map[string]any{"status": status, "sample_interval_ms": 20, "scope": "observed PPID-owned processes only; not arbitrary complete process trees", "fd_scope": "Linux /proc only; -1 means unavailable", "sidecar_exit_budget_ms": 6000, "sampling_slack_ms": 100, "exit_semantics": "exit=non-running; absent=identity gone; telemetry gate requires absent, including zombies", "owned": o.owned, "samples": o.samples, "errors": o.errors}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
func (o *lifecycleObserver) waitEmpty(t *testing.T, mainBudget, sidecarBudget time.Duration) {
	t.Helper()
	entered := time.Since(o.started).Milliseconds()
	deadline := time.Now().Add(mainBudget)
	var mainExit time.Time
	for {
		o.mu.Lock()
		n, s := 0, 0
		fresh := false
		if len(o.samples) > 0 {
			last := o.samples[len(o.samples)-1]
			n = last.MainCount
			for _, owned := range o.owned {
				if owned.Kind == "telemetry" && owned.GoneMS == nil {
					s++
				}
			}
			fresh = last.AtMS > entered
		}
		errs := append([]string(nil), o.errors...)
		o.mu.Unlock()
		if len(errs) > 0 {
			t.Fatalf("process observation unverified: %v", errs)
		}
		if fresh && n == 0 {
			if mainExit.IsZero() {
				mainExit = time.Now()
			}
			if s == 0 {
				return
			}
			if time.Since(mainExit) > sidecarBudget {
				t.Fatalf("owned telemetry did not exit within %s after main exit", sidecarBudget)
			}
		}
		if (!fresh || n > 0) && time.Now().After(deadline) {
			t.Fatalf("main backend did not exit within %s", mainBudget)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func readLifecycleProcesses() ([]lifecycleProcess, error) {
	if runtime.GOOS == "linux" {
		self, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return nil, err
		}
		selfIDs := lifecycleNamespacePIDs(string(self))
		if len(selfIDs) == 0 {
			return nil, fmt.Errorf("self NSpid unavailable")
		}
		nested := selfIDs[0] != os.Getpid()
		var namespace string
		if nested {
			namespace, err = os.Readlink("/proc/self/ns/pid")
			if err != nil {
				return nil, err
			}
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil, err
		}
		var result []lifecycleProcess
		for _, e := range entries {
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
					continue
				} // exited during enumeration
				return nil, fmt.Errorf("read process %s stat: %w", e.Name(), err)
			}
			p, err := parseLifecycleProcStat(string(raw))
			if err != nil {
				return nil, err
			}
			cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err == nil {
				p.Command = strings.ReplaceAll(string(cmd), "\x00", " ")
			} else if !os.IsNotExist(err) && !errors.Is(err, syscall.ESRCH) {
				return nil, fmt.Errorf("read process %s command: %w", e.Name(), err)
			}
			p.NamespacePID = p.PID
			if nested {
				p.NamespacePID = 0
				ns, nsErr := os.Readlink(filepath.Join("/proc", e.Name(), "ns/pid"))
				if nsErr == nil && ns == namespace {
					status, statusErr := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
					if statusErr != nil {
						if os.IsNotExist(statusErr) || errors.Is(statusErr, syscall.ESRCH) {
							continue
						}
						return nil, statusErr
					}
					ids := lifecycleNamespacePIDs(string(status))
					if len(ids) != len(selfIDs) {
						return nil, fmt.Errorf("inconsistent process PID namespace depth")
					}
					p.NamespacePID = ids[len(ids)-1]
				}
			}
			result = append(result, p)
		}
		found := false
		for _, p := range result {
			if p.NamespacePID == os.Getpid() {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("observer cannot observe itself in /proc")
		}
		return result, nil
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("process observation unsupported on %s", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,sess=,lstart=,rss=,state=,command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	snapshotStarted := time.Now()
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps observation: %w", err)
	}
	rows, err := parseLifecyclePS(string(raw))
	if err != nil {
		return nil, err
	}
	var verified []lifecycleProcess
	for _, p := range rows {
		p.NamespacePID = p.PID
		p, err = refineLifecycleIdentity(p, snapshotStarted)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			return nil, fmt.Errorf("process identity %d: %w", p.PID, err)
		}
		verified = append(verified, p)
	}
	return verified, nil
}
func parseLifecycleProcStat(raw string) (lifecycleProcess, error) {
	left, right := strings.Index(raw, "("), strings.LastIndex(raw, ")")
	if left < 1 || right <= left {
		return lifecycleProcess{}, fmt.Errorf("invalid /proc stat")
	}
	f := strings.Fields(raw[right+1:])
	if len(f) < 22 {
		return lifecycleProcess{}, fmt.Errorf("short /proc stat")
	}
	number := func(s string) int { n, _ := strconv.Atoi(s); return n }
	pid, err := strconv.Atoi(strings.TrimSpace(raw[:left]))
	if err != nil {
		return lifecycleProcess{}, err
	}
	rss, err := strconv.ParseInt(f[21], 10, 64)
	if err != nil {
		return lifecycleProcess{}, err
	}
	return lifecycleProcess{PID: pid, PPID: number(f[1]), PGID: number(f[2]), SID: f[3], Start: f[19], State: f[0], RSS: rss * int64(os.Getpagesize()), Command: raw[left+1 : right]}, nil
}
func parseLifecyclePS(raw string) ([]lifecycleProcess, error) {
	var result []lifecycleProcess
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 12 {
			return nil, fmt.Errorf("short ps row")
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		rss, e4 := strconv.ParseInt(f[9], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			return nil, fmt.Errorf("invalid ps identity")
		}
		result = append(result, lifecycleProcess{PID: pid, PPID: ppid, PGID: pgid, SID: f[3], Start: strings.Join(f[4:9], " "), RSS: rss * 1024, State: f[10][:1], Command: strings.Join(f[11:], " ")})
	}
	return result, nil
}
func TestRawLifecycleObserverIdentityParsing(t *testing.T) {
	proc := `123 (gopls (worker)) S 100 123 123 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 987654 999999 42`
	p, err := parseLifecycleProcStat(proc)
	if err != nil || p.PID != 123 || p.PPID != 100 || p.Start != "987654" || p.RSS != 42*int64(os.Getpagesize()) {
		t.Fatalf("parse proc=%+v %v", p, err)
	}
	rows, err := parseLifecyclePS("123 100 123 abc Thu Oct 22 12:34:56 2026 1024 S /tmp/gopls telemetry\n")
	if err != nil || len(rows) != 1 || rows[0].RSS != 1048576 || rows[0].SID != "abc" || rows[0].Start != "Thu Oct 22 12:34:56 2026" {
		t.Fatalf("parse ps=%+v %v", rows, err)
	}
	changed := p
	changed.Start = "987655"
	if p.identity() == changed.identity() {
		t.Fatal("PID reuse merged identities")
	}
}

func (o *lifecycleObserver) latest(t *testing.T) lifecycleSample {
	t.Helper()
	o.mu.Lock()
	n := len(o.samples)
	errs := append([]string(nil), o.errors...)
	var last lifecycleSample
	if n > 0 {
		last = o.samples[n-1]
	}
	o.mu.Unlock()
	if n == 0 || len(errs) > 0 {
		t.Fatalf("process observation unverified: samples=%d errors=%v", n, errs)
	}
	return last
}

// NSpid is ordered from the procfs mount's PID namespace to the process's.
func lifecycleNamespacePIDs(status string) []int {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			var ids []int
			for _, f := range strings.Fields(strings.TrimPrefix(line, "NSpid:")) {
				id, err := strconv.Atoi(f)
				if err != nil {
					return nil
				}
				ids = append(ids, id)
			}
			return ids
		}
	}
	return nil
}

func TestRawLifecycleObserverOwnershipSurvivesReparenting(t *testing.T) {
	proxy := lifecycleProcess{PID: 900001, NamespacePID: 1, Start: "1", State: "S"}
	main := lifecycleProcess{PID: 900002, NamespacePID: 2, PPID: proxy.PID, Start: "2", State: "S"}
	sidecar := lifecycleProcess{PID: 900003, NamespacePID: 3, PPID: main.PID, PGID: 900003, SID: "900003", Start: "3", State: "S", Command: "gopls mcp"}
	unrelated := lifecycleProcess{PID: 900004, PPID: 1, Start: "4", State: "S", Command: "gopls telemetry"}
	o := &lifecycleObserver{pid: proxy.PID, proxyIdentity: proxy.identity(), started: time.Now(), owned: map[string]*lifecycleOwned{}}
	o.observe([]lifecycleProcess{proxy, main, sidecar, unrelated}, nil)
	if len(o.owned) != 2 || o.owned[sidecar.identity()].Kind != "descendant" {
		t.Fatalf("wrong initial attribution: %+v", o.owned)
	}
	sidecar.Command = "gopls telemetry"
	o.observe([]lifecycleProcess{proxy, main, sidecar}, nil)
	if o.owned[sidecar.identity()].Kind != "telemetry" {
		t.Fatal("same-identity fork-to-exec telemetry was not promoted")
	}
	sidecar.PPID = 1
	reused := main
	reused.Start = "22"
	reused.PPID = 1
	o.observe([]lifecycleProcess{proxy, sidecar, reused}, nil)
	if o.owned[sidecar.identity()].ExitMS != nil || o.owned[main.identity()].ExitMS == nil || o.owned[reused.identity()] != nil {
		t.Fatal("reparenting/PID reuse corrupted identity")
	}
	sidecar.State = "Z"
	sidecar.NamespacePID = 0 // /proc namespace aliases may disappear for a zombie
	o.observe([]lifecycleProcess{proxy, sidecar}, nil)
	if o.owned[sidecar.identity()].ExitMS == nil || o.owned[sidecar.identity()].GoneMS != nil {
		t.Fatal("zombie was mistaken for absent/reaped process")
	}
	if o.owned[sidecar.identity()].Process.NamespacePID != 3 {
		t.Fatal("terminal snapshot discarded verified namespace identity")
	}
	fakeProxy := proxy
	fakeProxy.Start = "11"
	reused.PPID = fakeProxy.PID
	o.observe([]lifecycleProcess{fakeProxy, reused}, nil)
	if o.owned[reused.identity()] != nil || o.owned[sidecar.identity()].ExitMS == nil {
		t.Fatal("reused proxy PID adopted unrelated descendants")
	}
	if got := lifecycleNamespacePIDs("Name:\tgopls\nNSpid:\t900003\t3\n"); len(got) != 2 || got[1] != 3 {
		t.Fatalf("namespace PID mapping: %v", got)
	}
}
