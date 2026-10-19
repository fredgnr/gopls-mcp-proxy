//go:build !unix

package backend

import (
	"errors"
	"os"
	"os/exec"
)

func openExecutable(path string) (*os.File, error) { return os.Open(path) }

func configureProcess(cmd *exec.Cmd)       {}
func terminateProcess(cmd *exec.Cmd) error { return killProcess(cmd) }
func killProcess(cmd *exec.Cmd) error {
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
