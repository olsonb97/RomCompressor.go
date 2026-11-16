package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

func convertSwitch(ctx context.Context, source, output string, input string, opts Config) error {
	ext := strings.ToLower(filepath.Ext(source))
	compress := input == kind.Raw
	if compress {
		if (ext != ".nsp" || opts.Format != kind.NSZ) && (ext != ".xci" || opts.Format != kind.XCZ) {
			return fmt.Errorf("NSZ requires NSP input; XCZ requires XCI input")
		}
	} else if opts.Format != kind.Raw {
		return fmt.Errorf("NSZ and XCZ decompression requires raw output")
	}
	tool, keys, err := resolveSwitch(opts)
	if err != nil {
		return err
	}
	engineStage, err := fileops.NewStage(filepath.Dir(output), "nsz-")
	if err != nil {
		return err
	}
	defer engineStage.Cleanup()
	engineDir := engineStage.Path()
	engineSource, err := prepareSwitchInput(ctx, source, ext, engineDir, opts.MaxOutput)
	if err != nil {
		return err
	}
	targetExt := ".nsp"
	if input == kind.XCZ {
		targetExt = ".xci"
	}
	args := []string{"-D", "--output", engineDir, "--keys", keys, engineSource}
	if compress {
		targetExt = "." + string(opts.Format)
		args = []string{"-C", "--block", "--keep", "--verify", "--threads", "2", "--multi", "1", "--level", "5", "--output", engineDir, "--keys", keys, engineSource}
	}
	stem := strings.TrimSuffix(filepath.Base(engineSource), filepath.Ext(engineSource))
	generated := filepath.Join(engineDir, stem+targetExt)
	if err = runSwitchConversion(ctx, tool, engineStage, generated, args...); err != nil {
		return err
	}
	if filepath.Dir(engineSource) == engineDir {
		if err = os.Remove(engineSource); err != nil {
			return err
		}
	}
	if compress {
		roundtrip, err := fileops.NewStage(engineDir, "roundtrip-")
		if err != nil {
			return err
		}
		defer roundtrip.Cleanup()
		restored := filepath.Join(roundtrip.Path(), stem+ext)
		if err = runSwitchConversion(ctx, tool, engineStage, restored, "-D", "--output", roundtrip.Path(), "--keys", keys, generated); err != nil {
			return err
		}
		if err = roundtrip.Check(); err != nil {
			return err
		}
		if err = fileops.CompareFiles(ctx, source, restored); err != nil {
			return err
		}
	} else {
		if err = runSwitchVerify(ctx, tool, engineStage, keys, generated); err != nil {
			return err
		}
	}
	return os.Rename(generated, output)
}

func resolveSwitch(opts Config) (tool, keys string, err error) {
	if opts.Keys == "" {
		return "", "", fmt.Errorf("Switch conversion and verification require an explicit keys file")
	}
	keys, err = filepath.Abs(opts.Keys)
	if err != nil {
		return "", "", fmt.Errorf("invalid Switch keys file path")
	}
	info, err := os.Stat(keys)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", "", fmt.Errorf("Switch keys path must name an existing nonempty regular file")
	}
	// NSZ loads the entire text file into memory rather than streaming it.
	if info.Size() > 1<<20 {
		return "", "", fmt.Errorf("Switch keys file exceeds 1 MiB")
	}
	tool, err = resolveTool(opts, "nsz")
	return tool, keys, err
}

// NSZ resolves links before matching lowercase filename suffixes. Supply a
// private copy if the resolved source cannot be recognized by its filename.
func prepareSwitchInput(ctx context.Context, source, ext, stage string, limit int64) (string, error) {
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Switch source must be a regular file")
	}
	if filepath.Ext(source) == ext && filepath.Base(source) != ext {
		return source, nil
	}
	if info.Size() > limit {
		return "", fmt.Errorf("Switch input staging exceeds output limit")
	}
	file, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer file.Close()
	path := filepath.Join(stage, "input"+ext)
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(dst, io.LimitReader(&fileops.ContextReader{Context: ctx, Reader: file}, info.Size()))
	closeErr := dst.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n != info.Size() {
		return "", fmt.Errorf("Switch source changed during staging")
	}
	return path, ctx.Err()
}

func runSwitchConversion(ctx context.Context, tool string, stage fileops.Stage, output string, args ...string) error {
	status := &switchStatus{output: filepath.Base(output)}
	if err := runSwitch(ctx, tool, stage, status, args...); err != nil {
		return err
	}
	info, err := os.Lstat(output)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("NSZ did not create the expected output file")
	}
	return nil
}

func runSwitchVerify(ctx context.Context, tool string, stage fileops.Stage, keys, path string) error {
	ext := strings.ToUpper(strings.TrimPrefix(filepath.Ext(path), "."))
	status := &switchStatus{verifyMessage: fmt.Sprintf("[VERIFY %s] %s", ext, filepath.Base(path))}
	return runSwitch(ctx, tool, stage, status, "--verify", "--keys", keys, path)
}

// Switch tools may include key values in diagnostics. Only their structured
// success status crosses this boundary; raw output is never returned to callers.
func runSwitch(ctx context.Context, tool string, stage fileops.Stage, status *switchStatus, args ...string) error {
	if err := stage.Check(); err != nil {
		return err
	}
	// Keep NSZ's user-profile key fallbacks inside staging. NSZ can also try
	// default files beside its executable after an explicit file fails to load.
	env := []string{"HOME=" + stage.Path(), "USERPROFILE=" + stage.Path(), "XDG_CONFIG_HOME=" + stage.Path()}
	env = append(env, temporaryEnvironment(stage.Path())...)
	_, err := process.Execute(ctx, status, env, "", tool, append([]string{"--machine-readable"}, args...)...)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("NSZ failed; check your keys file and use NSZ 5.0 or newer")
	}
	if err = status.consume(); err != nil {
		return err
	}
	if !status.confirmed || status.failed {
		return fmt.Errorf("NSZ did not confirm successful completion; use NSZ 5.0 or newer")
	}
	return stage.Check()
}

type switchStatus struct {
	line          []byte
	output        string
	verifyMessage string
	observed      bool
	checkedHash   bool
	confirmed     bool
	failed        bool
}

func (s *switchStatus) Write(data []byte) (int, error) {
	for i, b := range data {
		if b == '\n' {
			if err := s.consume(); err != nil {
				return i, err
			}
			continue
		}
		if len(s.line) >= 64*1024 {
			return i, fmt.Errorf("NSZ status line exceeded its limit")
		}
		s.line = append(s.line, b)
	}
	return len(data), nil
}

func (s *switchStatus) consume() error {
	if len(s.line) == 0 {
		return nil
	}
	var message struct {
		Type       string `json:"type"`
		Action     string `json:"action"`
		File       string `json:"file"`
		Message    string `json:"message"`
		Success    bool   `json:"success"`
		ErrorCount *int   `json:"errorCount"`
	}
	err := json.Unmarshal(s.line, &message)
	s.line = s.line[:0]
	if err != nil {
		return fmt.Errorf("NSZ returned an invalid status message")
	}
	if message.Type == "error" {
		return fmt.Errorf("NSZ reported a processing error")
	}
	if message.Action == "CORRUPTED" || message.Action == "MISMATCH" || message.Action == "TICKETLESS" {
		s.failed = true
	}
	if message.Type == "complete" && s.output != "" && filepath.Base(message.File) == s.output ||
		message.Type == "info" && s.verifyMessage != "" && message.Message == s.verifyMessage {
		s.observed = true
	}
	if message.Type == "info" && message.Action == "VERIFIED" && s.verifyMessage != "" && s.observed {
		s.checkedHash = true
	}
	if message.Type == "summary" {
		// NSZ can return a successful batch summary after skipping the input.
		s.confirmed = !s.confirmed && s.observed && (s.verifyMessage == "" || s.checkedHash) &&
			message.Success && message.ErrorCount != nil && *message.ErrorCount == 0
		if !s.confirmed {
			s.failed = true
		}
	}
	return nil
}
