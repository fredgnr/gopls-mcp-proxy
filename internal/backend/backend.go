// Package backend manages raw child-process stdio. It has no MCP or business
// protocol knowledge: the caller owns framing, handshakes, and recovery.
package backend

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Executable      string
	Args            []string
	Workspace       string
	Env             []string
	ShutdownTimeout time.Duration
	CleanupTimeout  time.Duration
	Logger          *slog.Logger
	Stderr          io.Writer
}

// Factory fixes the command locator, arguments, cwd and environment for one
// proxy lifetime. A locator may be a symlink; its target is deliberately not
// resolved here so an upgrade is observed at the next ordinary spawn.
type Factory struct {
	cfg      Config
	mu       sync.Mutex
	previous *Process
}

var bootSequence atomic.Uint64

func NewFactory(cfg Config) (*Factory, error) {
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	cfg.Env = slices.Clone(cfg.Env)
	cfg.Args = slices.Clone(cfg.Args)
	if cfg.Workspace == "" {
		var err error
		cfg.Workspace, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	var err error
	cfg.Workspace, err = filepath.Abs(cfg.Workspace)
	if err != nil {
		return nil, err
	}
	cfg.Executable, err = resolveExecutable(cfg.Executable, cfg.Env, cfg.Workspace)
	if err != nil {
		return nil, err
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 2 * time.Second
	}
	if cfg.CleanupTimeout <= 0 {
		cfg.CleanupTimeout = 6 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	return &Factory{cfg: cfg}, nil
}

func resolveExecutable(name string, env []string, cwd string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("backend program is required after --")
	}
	check := func(path string) (string, error) {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return "", fmt.Errorf("backend program %q is not executable", path)
		}
		return filepath.Clean(path), nil
	}
	if strings.ContainsAny(name, `/\`) {
		return check(name)
	}
	var path string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if resolved, err := check(filepath.Join(dir, name)); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("resolve backend program %q: %w", name, exec.ErrNotFound)
}

// Process exposes the raw pipes and a unique ID allocated before each spawn
// attempt, including attempts that fail and are only visible in diagnostics.
type Process struct {
	ID              uint64
	Digest          [32]byte
	Stdin           io.WriteCloser
	Stdout          io.ReadCloser
	Done            <-chan struct{}
	cmd             *exec.Cmd
	done            chan struct{}
	once            sync.Once
	mu              sync.Mutex
	err             error
	closeErr        error
	shutdownTimeout time.Duration
	cleanupTimeout  time.Duration
	logger          *slog.Logger
	cleaned         atomic.Bool
}

func (f *Factory) Start(ctx context.Context) (*Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := bootSequence.Add(1)
	started := time.Now()
	failed := func(err error) (*Process, error) {
		f.cfg.Logger.Error("backend start failed", "boot_id", id, "error", err, "elapsed", time.Since(started))
		return nil, fmt.Errorf("backend boot %d: %w", id, err)
	}
	if f.previous != nil && !f.previous.cleaned.Load() {
		return failed(fmt.Errorf("previous backend boot %d has not completed cleanup", f.previous.ID))
	}
	if err := ctx.Err(); err != nil {
		return failed(err)
	}
	digest, err := executableDigest(ctx, f.cfg.Executable)
	if err != nil {
		return failed(err)
	}
	if err := ctx.Err(); err != nil {
		return failed(err)
	}
	childIn, parentIn, err := os.Pipe()
	if err != nil {
		return failed(err)
	}
	parentOut, childOut, err := os.Pipe()
	if err != nil {
		_ = childIn.Close()
		_ = parentIn.Close()
		return failed(err)
	}
	cmd := exec.Command(f.cfg.Executable, f.cfg.Args...)
	cmd.Dir, cmd.Env = f.cfg.Workspace, slices.Clone(f.cfg.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = childIn, childOut, f.cfg.Stderr
	configureProcess(cmd)
	err = cmd.Start()
	_ = childIn.Close()
	_ = childOut.Close()
	if err != nil {
		_ = parentIn.Close()
		_ = parentOut.Close()
		return failed(err)
	}
	done := make(chan struct{})
	p := &Process{ID: id, Digest: digest, Stdin: parentIn, Stdout: parentOut, Done: done, cmd: cmd, done: done,
		shutdownTimeout: f.cfg.ShutdownTimeout, cleanupTimeout: f.cfg.CleanupTimeout, logger: f.cfg.Logger}
	f.previous = p
	go func() { err := cmd.Wait(); p.mu.Lock(); p.err = err; p.mu.Unlock(); close(done) }()
	f.cfg.Logger.Info("backend started", "boot_id", id, "pid", p.PID(), "image_sha256", fmt.Sprintf("%x", digest), "elapsed", time.Since(started))
	if err := ctx.Err(); err != nil {
		if cleanupErr := p.Close(); cleanupErr != nil {
			return failed(errors.Join(err, cleanupErr))
		}
		return failed(err)
	}
	return p, nil
}

// The digest is observational: a changed image is accepted. Hashing and exec are
// separate filesystem operations, so this is not a secure attestation.
func executableDigest(ctx context.Context, path string) ([32]byte, error) {
	// Recheck on every spawn: a configured symlink or executable may have been
	// replaced since factory construction. Never hash pipes/devices/directories.
	info, err := os.Stat(path)
	if err != nil {
		return [32]byte{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return [32]byte{}, fmt.Errorf("backend program %q is not a regular executable", path)
	}
	f, err := openExecutable(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return [32]byte{}, err
	}
	if !info.Mode().IsRegular() {
		return [32]byte{}, fmt.Errorf("backend program %q changed to a non-regular file", path)
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		n, err := f.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return [32]byte{}, err
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func (p *Process) PID() int { return p.cmd.Process.Pid }

// Err reports the Wait result after Done closes. An intentional signal during
// shutdown may produce an ExitError even when resource cleanup succeeded.
func (p *Process) Err() error { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *Process) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), p.cleanupTimeout)
	defer cancel()
	return p.CloseContext(ctx)
}

// CloseContext closes both pipes to unblock concurrent IO, then performs
// EOF -> TERM -> KILL -> Wait within the caller's remaining cleanup budget.
// A failure to confirm Wait is an error; callers must not start another child.
func (p *Process) CloseContext(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.cleanupTimeout)
	defer cancel()
	p.once.Do(func() {
		started := time.Now()
		_ = p.Stdin.Close()
		_ = p.Stdout.Close()
		if !waitFor(p.done, ctx, p.shutdownTimeout) {
			_ = terminateProcess(p.cmd)
			_ = waitFor(p.done, ctx, p.shutdownTimeout)
		}
		// Always kill the process group, including descendants whose parent exited.
		if err := killProcess(p.cmd); err != nil {
			p.closeErr = fmt.Errorf("backend boot %d pid %d process-group kill failed: %w", p.ID, p.PID(), err)
		}
		if !waitFor(p.done, ctx, p.cleanupTimeout) {
			cause := ctx.Err()
			if cause == nil {
				cause = context.DeadlineExceeded
			}
			p.closeErr = errors.Join(p.closeErr, fmt.Errorf("backend boot %d pid %d cleanup could not confirm Wait: %w", p.ID, p.PID(), cause))
		}
		if p.closeErr == nil {
			p.cleaned.Store(true)
		}
		p.logger.Info("backend cleanup", "boot_id", p.ID, "pid", p.PID(), "elapsed", time.Since(started), "error", p.closeErr)
	})
	return p.closeErr
}

func waitFor(done <-chan struct{}, ctx context.Context, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}
