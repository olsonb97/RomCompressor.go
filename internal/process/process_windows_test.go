//go:build windows

package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestEngineWindowsProcessTreeHelper(t *testing.T) {
	mode := os.Getenv("ROMCOMPRESSOR_TEST_WINDOWS_TREE")
	switch mode {
	case "parent", "parent-exits", "parent-keeps-pipe":
		cmd := exec.Command(os.Args[0], "-test.run=^TestEngineWindowsProcessTreeHelper$")
		cmd.Env = append(os.Environ(), "ROMCOMPRESSOR_TEST_WINDOWS_TREE=child")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if mode == "parent-keeps-pipe" {
			cmd.Stdout = os.Stdout
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Println(cmd.Process.Pid)
		if mode != "parent" {
			finish := os.Getenv("ROMCOMPRESSOR_TEST_WINDOWS_FINISH")
			for {
				if _, err := os.Stat(finish); err == nil {
					os.Exit(0)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		_ = cmd.Wait()
	case "child":
		time.Sleep(time.Minute)
	}
}

func TestWindowsEngineCancellationStopsTreeWithoutPATH(t *testing.T) {
	testWindowsProcessTree(t, "parent", context.Canceled)
}

func TestWindowsEngineExitStopsDescendants(t *testing.T) {
	testWindowsProcessTree(t, "parent-exits", nil)
}

func TestWindowsEnginePipeTimeoutStopsDescendants(t *testing.T) {
	testWindowsProcessTree(t, "parent-keeps-pipe", exec.ErrWaitDelay)
}

func testWindowsProcessTree(t *testing.T, mode string, want error) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ROMCOMPRESSOR_TEST_WINDOWS_TREE", mode)
	finish := filepath.Join(t.TempDir(), "exit")
	t.Setenv("ROMCOMPRESSOR_TEST_WINDOWS_FINISH", finish)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	read, write := io.Pipe()
	defer read.Close()
	done := make(chan error, 1)
	go func() {
		_, err := RunTo(ctx, write, os.Args[0], "-test.run=^TestEngineWindowsProcessTreeHelper$")
		_ = write.CloseWithError(err)
		done <- err
	}()
	scanner := bufio.NewScanner(read)
	if !scanner.Scan() {
		t.Fatalf("engine did not report the child process: %v", scanner.Err())
	}
	pid, err := strconv.ParseUint(scanner.Text(), 10, 32)
	if err != nil {
		t.Fatalf("invalid child process ID %q: %v", scanner.Text(), err)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	defer windows.TerminateProcess(child, 1)
	if mode == "parent" {
		cancel()
	} else if err := os.WriteFile(finish, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("engine returned %v, want %v", err, want)
	}
	if status, err := windows.WaitForSingleObject(child, 5000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("engine child survived process exit: wait status = %#x, error = %v", status, err)
	}
}
