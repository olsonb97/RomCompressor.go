//go:build windows

package process

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type engineProcess struct {
	process *os.Process
	job     windows.Handle
}

func startEngineProcess(cmd *exec.Cmd) (engineProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return engineProcess{}, fmt.Errorf("create engine job: %w", err)
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	var pin runtime.Pinner
	pin.Pin(&limits) // The Windows wrapper passes this buffer as a uintptr.
	defer pin.Unpin()
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return engineProcess{}, fmt.Errorf("set engine job limits: %w", err)
	}
	// Assign the engine while suspended so its earliest children join the job.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return engineProcess{}, err
	}
	process := engineProcess{process: cmd.Process, job: job}
	if err := activateEngineProcess(process); err != nil {
		process.terminate()
		_ = cmd.Wait()
		process.close()
		return engineProcess{}, fmt.Errorf("activate engine job: %w", err)
	}
	return process, nil
}

func activateEngineProcess(engine engineProcess) error {
	pid := uint32(engine.process.Pid)
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(engine.job, process); err != nil {
		return err
	}
	// os/exec closes the initial thread handle. Find the suspended thread by PID.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID == pid {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			defer windows.CloseHandle(thread)
			_, err = windows.ResumeThread(thread)
			return err
		}
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return err
	}
	return fmt.Errorf("initial engine thread could not be found")
}

func (p engineProcess) terminate() {
	_ = windows.TerminateJobObject(p.job, 1)
	_ = p.process.Kill()
}

func (p engineProcess) wait(cmd *exec.Cmd) error { return cmd.Wait() }

func (p engineProcess) close() { _ = windows.CloseHandle(p.job) }
