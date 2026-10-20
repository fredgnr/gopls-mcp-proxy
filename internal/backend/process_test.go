//go:build unix

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCloseKillsDescendantAfterParentEOF(t *testing.T) {
	p := launchHelper(t, helperConfig(t, "parent"))
	pid, err := strconv.Atoi(line(t, p))
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 {
		t.Fatal("invalid descendant PID")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processRunning(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived group cleanup", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func processRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return false
	}
	if runtime.GOOS == "linux" {
		// Minimal containers may not reap adopted zombies; zombies cannot execute
		// or retain pipes. The direct child is always reaped by Process.Wait.
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return false
		}
		if end := strings.LastIndex(string(b), ") "); end >= 0 && len(b) > end+2 && b[end+2] == 'Z' {
			return false
		}
	}
	return true
}

func TestFactoryRequiresCleanupBeforeNextSpawn(t *testing.T) {
	f, err := NewFactory(helperConfig(t, "hang"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := f.Start(context.Background()); err == nil {
		t.Fatal("started second backend before cleanup")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	q, err := f.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutableFIFOIsRejectedAtResolutionAndRespawn(t *testing.T) {
	dir := t.TempDir()
	fifo := dir + "/fifo"
	if err := syscall.Mkfifo(fifo, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFactory(Config{Executable: fifo, Workspace: dir}); err == nil {
		t.Fatal("accepted executable FIFO")
	}
	regular := dir + "/program"
	if err := os.WriteFile(regular, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{Executable: regular, Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(regular); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(regular, 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := factory.Start(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO after executable replacement")
		}
	case <-time.After(time.Second):
		t.Fatal("spawn blocked opening executable FIFO")
	}
}

func TestExecutableDigestHonorsCancellation(t *testing.T) {
	path := t.TempDir() + "/program"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(128 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := executableDigest(ctx, path); err != context.DeadlineExceeded {
		t.Fatalf("digest did not obey deadline: %v", err)
	}
}

func TestBeforeStartFailureClosesAllPipes(t *testing.T) {
	fdDir := "/dev/fd"
	if runtime.GOOS == "linux" {
		fdDir = "/proc/self/fd"
	}
	countFDs := func() int {
		t.Helper()
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	dir := t.TempDir()
	program := dir + "/program"
	marker := dir + "/spawned"
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf spawned > spawned\n"), 0755); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("deny spawn")
	f, err := NewFactory(Config{
		Executable: program, Workspace: dir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BeforeStart: func(context.Context, string) error { return denied },
	})
	if err != nil {
		t.Fatal(err)
	}
	before := countFDs()
	for range 20 {
		if p, err := f.Start(context.Background()); p != nil || !errors.Is(err, denied) {
			t.Fatalf("guard failure: process=%v, err=%v", p, err)
		}
	}
	if after := countFDs(); after > before {
		t.Fatalf("guard failure leaked descriptors: before=%d, after=%d", before, after)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("denied child executed: %v", err)
	}
	if f.previous != nil {
		t.Fatal("denied attempt became previous process")
	}
}
