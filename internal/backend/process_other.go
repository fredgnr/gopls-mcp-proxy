//go:build !unix

package backend

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
func terminateProcess(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
func killProcess(cmd *exec.Cmd)      { _ = cmd.Process.Kill() }
