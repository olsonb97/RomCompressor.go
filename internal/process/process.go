package process

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ToolError describes an executable lookup failure without exposing its path.
type ToolError struct {
	Engine     string
	Configured bool
	Err        error
}

func (e *ToolError) Error() string {
	reason := fmt.Sprintf("%s is required; install it on PATH", e.Engine)
	if e.Configured {
		reason = fmt.Sprintf("configured %s executable is unavailable", e.Engine)
	}
	return fmt.Sprintf("%s; set Options.Tools[%q] in Go or use --tool %s=PATH", reason, e.Engine, e.Engine)
}

// Unwrap retains errors.Is support for the executable lookup failure.
func (e *ToolError) Unwrap() error { return e.Err }

// Resolve finds an explicitly configured executable or one of its known PATH aliases.
func Resolve(tools map[string]string, name string) (string, error) {
	path := tools[name]
	if path != "" {
		found, err := exec.LookPath(path)
		if err != nil {
			return "", &ToolError{Engine: name, Configured: true, Err: err}
		}
		return found, nil
	}
	candidates := []string{name}
	switch name {
	case "dolphin-tool":
		candidates = append(candidates, "DolphinTool")
	case "7z":
		candidates = append(candidates, "7zz", "7za", "7zr")
	}
	for _, candidate := range candidates {
		if found, err := exec.LookPath(candidate); err == nil {
			return found, nil
		}
	}
	return "", &ToolError{Engine: name, Err: exec.ErrNotFound}
}

// Log retains at most the last 4 MiB of tool output and rejects overflow.
// Its zero value is ready to use and its methods are safe for concurrent writers.
type Log struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

// Write records output and returns an error once the log exceeds its limit.
func (b *Log) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	const limit = 4 << 20
	if len(p) > limit-len(b.data) {
		b.truncated = true
	}
	if len(p) > limit {
		p = p[len(p)-limit:]
	}
	if len(b.data)+len(p) > limit {
		b.data = b.data[len(b.data)+len(p)-limit:]
	}
	b.data = append(b.data, p...)
	if b.truncated {
		return n, fmt.Errorf("engine output exceeded 4 MiB; result cannot be interpreted safely")
	}
	return n, nil
}

// String returns the retained output.
func (b *Log) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

// Run executes a tool with bounded combined output and process-tree cancellation.
func Run(ctx context.Context, executable string, args ...string) (string, error) {
	return RunTo(ctx, nil, executable, args...)
}

// RunTo streams stdout to dst and bounds captured stderr. A nil dst captures both.
func RunTo(ctx context.Context, dst io.Writer, executable string, args ...string) (string, error) {
	return Execute(ctx, dst, nil, "", executable, args...)
}

// RunEnv executes a tool with additional environment values.
func RunEnv(ctx context.Context, env []string, executable string, args ...string) (string, error) {
	return Execute(ctx, nil, env, "", executable, args...)
}

type cancellingWriter struct {
	writer io.Writer
	cancel context.CancelCauseFunc
}

func (w *cancellingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.cancel(err)
	}
	return n, err
}

// Execute runs a tool in dir with additional environment values and optional stdout.
// Output errors and context cancellation terminate the engine process tree.
func Execute(ctx context.Context, dst io.Writer, env []string, dir, executable string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cmd := exec.Command(executable, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), env...)
	log := &Log{}
	logWriter := &cancellingWriter{writer: log, cancel: cancel}
	cmd.Stderr = logWriter
	cmd.Stdout = logWriter
	if dst != nil {
		cmd.Stdout = &cancellingWriter{writer: dst, cancel: cancel}
	}
	cmd.WaitDelay = 2 * time.Second
	process, err := startEngineProcess(cmd)
	if err != nil {
		return "", err
	}
	defer process.close()
	done := make(chan error, 1)
	go func() { done <- process.wait(cmd) }()
	select {
	case err := <-done:
		if cause := context.Cause(ctx); cause != nil {
			return "", cause
		}
		if log.truncated {
			return "", fmt.Errorf("engine output exceeded 4 MiB; result cannot be interpreted safely")
		}
		if err != nil {
			output := log.String()
			return output, fmt.Errorf("engine failed: %w\n%s", err, FailureDiagnostics(output))
		}
		return log.String(), nil
	case <-ctx.Done():
		process.terminate()
		<-done
		return log.String(), context.Cause(ctx)
	}
}

// FailureDiagnostics trims tool output for an error message, retaining at most
// its final 16 KiB with an explicit truncation marker. Engine parsers must use the
// original captured output; this bounds only retained and displayed diagnostics.
func FailureDiagnostics(output string) string {
	const limit = 16 << 10
	output = strings.TrimSpace(output)
	if len(output) <= limit {
		return output
	}
	return "[engine diagnostics truncated; last 16 KiB]\n" + output[len(output)-limit:]
}
