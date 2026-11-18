package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEngineProcessHelper(t *testing.T) {
	if os.Getenv("ROMCOMPRESSOR_TEST_PROCESS") != "1" {
		return
	}
	if os.Getenv("ROMCOMPRESSOR_TEST_FLOOD") == "1" {
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for {
			if _, err := os.Stderr.Write(chunk); err != nil {
				os.Exit(0)
			}
		}
	}
	for {
		if _, err := fmt.Fprintln(os.Stdout, "synthetic engine output"); err != nil {
			os.Exit(0)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEngineCancellation(t *testing.T) {
	t.Setenv("ROMCOMPRESSOR_TEST_PROCESS", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, os.Args[0], "-test.run=^TestEngineProcessHelper$")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("engine did not stop promptly")
	}
}

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestEngineStopsWhenOutputWriterFails(t *testing.T) {
	t.Setenv("ROMCOMPRESSOR_TEST_PROCESS", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := RunTo(ctx, failingOutput{}, os.Args[0], "-test.run=^TestEngineProcessHelper$")
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected writer failure without waiting for timeout, got %v", err)
	}
}

func TestEngineStopsWhenLogsOverflow(t *testing.T) {
	t.Setenv("ROMCOMPRESSOR_TEST_PROCESS", "1")
	t.Setenv("ROMCOMPRESSOR_TEST_FLOOD", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Run(ctx, os.Args[0], "-test.run=^TestEngineProcessHelper$")
	if err == nil || !strings.Contains(err.Error(), "engine output exceeded 4 MiB") {
		t.Fatalf("expected log limit failure without waiting for timeout, got %v", err)
	}
}

func TestBoundedEngineOutputNeverSilentlyTruncates(t *testing.T) {
	log := &Log{}
	_, _ = log.Write([]byte(strings.Repeat("x", 5<<20)))
	if !log.truncated || len(log.String()) > 4<<20 {
		t.Fatal("unbounded or silent truncation")
	}
}

func TestEngineDiagnosticHelper(t *testing.T) {
	mode := os.Getenv("ROMCOMPRESSOR_TEST_DIAGNOSTICS")
	if mode == "" {
		return
	}
	_, _ = os.Stderr.WriteString(testDiagnosticOutput())
	// A simulated engine must not append the Go test binary's coverage log.
	if mode == "failure" {
		syscall.Exit(3)
	}
	syscall.Exit(0)
}

func testDiagnosticOutput() string {
	return "engine log begins\n" + strings.Repeat("x", 3<<20) + "\nfinal engine diagnosis\n"
}

func TestEngineFailureDiagnosticsAreBounded(t *testing.T) {
	t.Setenv("ROMCOMPRESSOR_TEST_DIAGNOSTICS", "failure")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := Run(ctx, os.Args[0], "-test.run=^TestEngineDiagnosticHelper$")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !errors.Is(err, exit) {
		t.Fatalf("expected original exit error, got %v", err)
	}
	if len(err.Error()) > (16<<10)+200 || !strings.Contains(err.Error(), "diagnostics truncated; last 16 KiB") {
		t.Fatalf("unbounded or silently truncated diagnostics: %d bytes", len(err.Error()))
	}
	if strings.Contains(err.Error(), "engine log begins") || !strings.HasSuffix(err.Error(), "final engine diagnosis") {
		t.Fatal("failure diagnostics did not retain the final engine diagnosis")
	}
	if output != testDiagnosticOutput() {
		t.Fatal("bounded failure diagnostics changed the raw tool output")
	}
}

func TestEngineSuccessOutputIsUnchanged(t *testing.T) {
	t.Setenv("ROMCOMPRESSOR_TEST_DIAGNOSTICS", "success")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := Run(ctx, os.Args[0], "-test.run=^TestEngineDiagnosticHelper$")
	if err != nil {
		t.Fatal(err)
	}
	if output != testDiagnosticOutput() {
		t.Fatalf("unexpected engine output (%d bytes), ending in %q", len(output), output[max(0, len(output)-200):])
	}
}

func TestShortFailureDiagnosticsAreUnchanged(t *testing.T) {
	if got := FailureDiagnostics(" \nsmall engine failure\n "); got != "small engine failure" {
		t.Fatalf("small failure diagnostics changed: %q", got)
	}
}

func TestEngineWorkingDirectory(t *testing.T) {
	const marker = "romcompressor-engine-cwd-test.txt"
	const child = "ROMCOMPRESSOR_TEST_ENGINE_CWD"
	if os.Getenv(child) == "1" {
		if _, err := os.Stat(marker); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv(child, "1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, marker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Execute(ctx, nil, nil, dir, os.Args[0], "-test.run=^TestEngineWorkingDirectory$"); err != nil {
		t.Fatal(err)
	}
}
