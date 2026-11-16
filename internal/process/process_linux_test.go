//go:build linux

package process

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxEngineCancellationStopsProcessGroup(t *testing.T) {
	testLinuxProcessGroup(t, true)
}

func TestLinuxEngineExitStopsProcessGroup(t *testing.T) {
	testLinuxProcessGroup(t, false)
}

func testLinuxProcessGroup(t *testing.T, cancelEngine bool) {
	t.Helper()
	self, err := unix.PidfdOpen(os.Getpid(), 0)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		t.Skipf("process handles unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(self)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	read, write := io.Pipe()
	defer read.Close()
	done := make(chan error, 1)
	finish := filepath.Join(t.TempDir(), "exit")
	go func() {
		_, err := RunTo(ctx, write, "/bin/sh", "-c",
			`sleep 60 >/dev/null 2>&1 & printf '%s\n' "$!"; while [ ! -f "$1" ]; do sleep 0.01; done`, "engine", finish)
		_ = write.CloseWithError(err)
		done <- err
	}()
	scanner := bufio.NewScanner(read)
	if !scanner.Scan() {
		t.Fatalf("engine did not report the child process: %v", scanner.Err())
	}
	pid, err := strconv.Atoi(scanner.Text())
	if err != nil {
		t.Fatalf("invalid child process ID %q: %v", scanner.Text(), err)
	}
	child, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(child)
	defer unix.PidfdSendSignal(child, unix.SIGKILL, nil, 0)
	var want error
	if cancelEngine {
		cancel()
		want = context.Canceled
	} else if err := os.WriteFile(finish, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("engine returned %v, want %v", err, want)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		poll := []unix.PollFd{{Fd: int32(child), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, int(time.Until(deadline).Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if poll[0].Revents&unix.POLLIN != 0 {
			return
		}
		break
	}
	t.Fatal("engine child survived")
}
