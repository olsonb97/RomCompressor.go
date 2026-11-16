//go:build !windows && !linux

package process

import (
	"os/exec"
)

func (*engineProcess) wait(cmd *exec.Cmd) error { return cmd.Wait() }
