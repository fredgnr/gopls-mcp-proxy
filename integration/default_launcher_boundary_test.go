package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runDefaultBoundary executes the built CLI with separate stdout/stderr. All
// probes, traces and synthetic inputs stay outside the selected project cwd.
// Its process group is bounded even if a regression accidentally recurses.
func runDefaultBoundary(t *testing.T, l *defaultLayout, caseID, cwd string, args, env []string) ([]byte, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.proxy, args...)
	cmd.Dir, cmd.Env = cwd, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	err := cmd.Run()
	if cmd.Process != nil {
		// Normally the CLI never spawns here. Clean any accidental descendants
		// before reporting the failed assertion rather than leaving a recursion.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		t.Fatalf("boundary CLI did not exit promptly: %v\nstderr: %s", ctx.Err(), stderr.Bytes())
	}
	for name, data := range map[string][]byte{"boundary-stdout.log": stdout.Bytes(), "boundary-stderr.log": stderr.Bytes()} {
		if writeErr := os.WriteFile(filepath.Join(l.f.root, name), data, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	image, readErr := os.ReadFile(l.proxy)
	if readErr != nil {
		t.Fatal(readErr)
	}
	record := map[string]any{
		"case_id": caseID, "phase": "L2/L3", "assertion_scope": "real CLI local failure/diagnostic boundary; not Codex",
		"proxy": l.proxy, "proxy_sha256": fmt.Sprintf("%x", sha256.Sum256(image)), "argv": args, "cwd": cwd,
		"started_at": started.UTC().Format(time.RFC3339Nano), "elapsed": time.Since(started).String(),
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
	}
	if cmd.ProcessState != nil {
		record["exit_code"] = cmd.ProcessState.ExitCode()
	}
	data, marshalErr := json.MarshalIndent(record, "", "  ")
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if writeErr := os.WriteFile(filepath.Join(l.f.root, "boundary-launch.json"), data, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func assertNoBoundaryBackend(t *testing.T, l *defaultLayout, stderr []byte) {
	t.Helper()
	if len(l.f.events(t)) != 0 {
		t.Fatal("local boundary unexpectedly executed a fixture backend")
	}
	if bytes.Contains(stderr, []byte("backend started")) || bytes.Contains(stderr, []byte("input monitoring configured")) {
		t.Fatalf("local boundary entered backend/workspace setup: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(l.f.root, "go-invoked")); !os.IsNotExist(err) {
		t.Fatalf("local boundary invoked Go discovery: %v", err)
	}
}

// VAL-LAUNCH-016: production CaptureSelf and Check reject the actual running
// executable and filesystem aliases before tracker creation or any spawn. A
// direct default self path necessarily is the sibling named gopls; PATH cases
// therefore exercise symlink/hardlink aliases, with a valid later fallback.
func TestRawDefaultLauncherInitialSelfRejection(t *testing.T) {
	for _, kind := range []string{"sibling-direct", "sibling-symlink", "sibling-hardlink", "path-symlink", "path-hardlink", "explicit-direct", "explicit-symlink", "explicit-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			viaPath := strings.HasPrefix(kind, "path-")
			l := newDefaultLayout(t, viaPath)
			fallbackDir := filepath.Join(l.f.root, "fallback")
			copyExecutable(t, defaultFixtureBinary(t, false), filepath.Join(fallbackDir, "gopls"))
			path := l.path + string(os.PathListSeparator) + fallbackDir
			if err := os.Remove(l.backend); err != nil {
				t.Fatal(err)
			}
			var args []string
			switch {
			case kind == "sibling-direct":
				if err := os.Rename(l.proxy, l.backend); err != nil {
					t.Fatal(err)
				}
				l.proxy = l.backend
			case kind == "explicit-direct":
				args = []string{"--", l.proxy, "mcp"}
			case strings.HasSuffix(kind, "symlink"):
				if err := os.Symlink(l.proxy, l.backend); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(kind, "hardlink"):
				if err := os.Link(l.proxy, l.backend); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(kind, "explicit-") && args == nil {
				args = []string{"--", l.backend, "mcp"}
			}
			stdout, stderr, err := runDefaultBoundary(t, l, "VAL-LAUNCH-016", l.f.workspace, args, fixtureEnvWithPath(l.f, path))
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(stdout) != 0 || !bytes.Contains(stderr, []byte("proxy itself")) {
				t.Fatalf("self was not rejected locally: err=%v stdout=%q stderr=%s", err, stdout, stderr)
			}
			assertNoBoundaryBackend(t, l, stderr)
		})
	}
}

// VAL-LAUNCH-028: default resolution succeeds with a real fixture installed,
// but caller cwd remains subject to the existing home/root scan refusal.
func TestRawDefaultLauncherRefusesHomeAndRoot(t *testing.T) {
	for _, location := range []string{"home", "root"} {
		t.Run(location, func(t *testing.T) {
			l := newDefaultLayout(t, false)
			env := fixtureEnvWithPath(l.f, l.path)
			cwd := string(filepath.Separator)
			if location == "home" {
				cwd = l.f.workspace
				// A synthetic child-only HOME avoids reading or changing the user's
				// actual home while exercising os.UserHomeDir and physical cwd.
				filtered := env[:0]
				for _, entry := range env {
					if !strings.HasPrefix(entry, "HOME=") {
						filtered = append(filtered, entry)
					}
				}
				env = append(filtered, "HOME="+cwd)
			}
			stdout, stderr, err := runDefaultBoundary(t, l, "VAL-LAUNCH-028", cwd, nil, env)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(stdout) != 0 || !bytes.Contains(stderr, []byte("refusing to scan filesystem root or home directory")) {
				t.Fatalf("unsafe default cwd was not refused after resolution: err=%v stdout=%q stderr=%s", err, stdout, stderr)
			}
			assertNoBoundaryBackend(t, l, stderr)
		})
	}
}

// VAL-LAUNCH-006: actual process diagnostics succeed with an unusable default
// backend, no PATH tools, and a cwd that real runtime setup would refuse.
func TestRawDefaultLauncherLocalDiagnosticsKeepStdoutClean(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "--version"} {
		t.Run(flag, func(t *testing.T) {
			l := newDefaultLayout(t, false)
			if err := os.Remove(l.backend); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(l.proxy, l.backend); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := runDefaultBoundary(t, l, "VAL-LAUNCH-006", string(filepath.Separator), []string{flag}, fixtureEnvWithPath(l.f, ""))
			want := "Usage:"
			if flag == "--version" {
				want = "gopls-mcp-proxy"
			}
			if err != nil || len(stdout) != 0 || !bytes.Contains(stderr, []byte(want)) {
				t.Fatalf("diagnostic touched runtime or stdout: err=%v stdout=%q stderr=%s", err, stdout, stderr)
			}
			assertNoBoundaryBackend(t, l, stderr)
		})
	}
}
