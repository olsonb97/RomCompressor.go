//go:build !windows

package process

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
)

type engineProcess struct {
	process *os.Process
	mu      sync.Mutex
	reaping bool
}

func startEngineProcess(cmd *exec.Cmd) (engineProcess, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return engineProcess{}, err
	}
	return engineProcess{process: cmd.Process}, nil
}

func (p *engineProcess) terminate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reaping {
		_ = syscall.Kill(-p.process.Pid, syscall.SIGKILL)
	}
	_ = p.process.Kill()
}

func (*engineProcess) close() {}
