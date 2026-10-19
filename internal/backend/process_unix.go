//go:build unix

package backend

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func terminateProcess(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
func killProcess(cmd *exec.Cmd)      { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
