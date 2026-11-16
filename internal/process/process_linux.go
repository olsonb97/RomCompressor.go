package process

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

func (p *engineProcess) wait(cmd *exec.Cmd) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, p.process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		if err == nil {
			p.mu.Lock()
			// The group leader's PID stays reserved until cmd.Wait reaps it.
			_ = unix.Kill(-p.process.Pid, unix.SIGKILL)
			p.reaping = true
			p.mu.Unlock()
		}
		return cmd.Wait()
	}
}
