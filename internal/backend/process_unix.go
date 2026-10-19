//go:build unix

package backend

import (
	"os"
	"os/exec"
	"syscall"
)

// A nonblocking open also prevents a regular-file-to-FIFO replacement between
// Stat and Open from hanging before the descriptor's own type is checked.
func openExecutable(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func configureProcess(cmd *exec.Cmd)       { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func terminateProcess(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGTERM) }
func killProcess(cmd *exec.Cmd) error      { return signalGroup(cmd, syscall.SIGKILL) }
func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
