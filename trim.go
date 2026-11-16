package romcompressor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	cardtrim "github.com/olsonb97/romcompressor.go/internal/trim"
)

// TrimPlan describes a validated cartridge image and its removable padding.
// PaddingByte is meaningful when OutputBytes is smaller than InputBytes.
type TrimPlan struct {
	Format      string `json:"format"`
	InputBytes  int64  `json:"inputBytes"`
	OutputBytes int64  `json:"outputBytes"`
	PaddingByte byte   `json:"paddingByte"`
}

// TrimResult describes the new cartridge image and the SHA-256 of its bytes.
// The size and padding fields can be used to reconstruct the original suffix.
type TrimResult struct {
	TrimPlan
	Output string `json:"output"`
	SHA256 string `json:"sha256"`
}

// TrimOptions controls trim preview and explicit, copy-only trimming.
type TrimOptions struct {
	// Progress is called synchronously during preview or trimming.
	Progress func(Progress)
}

// PlanTrim validates the image and scans all removable bytes without writing files.
func PlanTrim(ctx context.Context, source string) (TrimPlan, error) {
	return PlanTrimWithOptions(ctx, source, TrimOptions{})
}

// PlanTrimWithOptions previews trimming with cancellation and progress reporting.
// It writes no files and rechecks the source after the final progress callback.
func PlanTrimWithOptions(ctx context.Context, source string, opts TrimOptions) (TrimPlan, error) {
	input, inputs, err := openTrimSource(ctx, source)
	if err != nil {
		return TrimPlan{}, err
	}
	defer input.Close()
	notify := progressReporter(opts.Progress)
	notify("validating cartridge", 0, 0)
	plan, err := inspectTrimWithProgress(ctx, input, inputs[source].Size(), notify)
	if err != nil {
		return TrimPlan{}, err
	}
	notify("complete", plan.InputBytes-plan.OutputBytes, plan.InputBytes-plan.OutputBytes)
	if err := checkInputs(inputs); err != nil {
		return TrimPlan{}, err
	}
	return plan, ctx.Err()
}

// Trim validates and copies a cartridge image to a new output without its
// verified trailing padding. It opens the original with read-only access and
// refuses any existing output, including when it aliases the original.
func Trim(ctx context.Context, source, output string, opts TrimOptions) (TrimResult, error) {
	if source == "" || output == "" {
		return TrimResult{}, errors.New("trim source and output paths are required")
	}
	var err error
	source, err = filepath.Abs(source)
	if err != nil {
		return TrimResult{}, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return TrimResult{}, err
	}
	if !filepath.IsLocal(filepath.Base(output)) {
		return TrimResult{}, errors.New("trim output must name a regular file")
	}
	if _, err := os.Lstat(output); err == nil {
		return TrimResult{}, &os.PathError{Op: "create output", Path: output, Err: os.ErrExist}
	} else if !os.IsNotExist(err) {
		return TrimResult{}, err
	}
	input, inputs, err := openTrimSource(ctx, source)
	if err != nil {
		return TrimResult{}, err
	}
	defer input.Close()
	notify := progressReporter(opts.Progress)
	notify("validating cartridge", 0, 0)
	plan, err := inspectTrimWithProgress(ctx, input, inputs[source].Size(), notify)
	if err != nil {
		return TrimResult{}, err
	}
	if plan.OutputBytes == plan.InputBytes {
		return TrimResult{}, ErrNoRemovablePadding
	}
	if err := checkInputs(inputs); err != nil {
		return TrimResult{}, err
	}
	stage, err := fileops.NewStage(filepath.Dir(output), ".romcompressor-trim-")
	if err != nil {
		return TrimResult{}, err
	}
	defer stage.Cleanup()
	staged := filepath.Join(stage.Path(), filepath.Base(output))
	notify("trimming copy", 0, plan.OutputBytes)
	if err := stage.Check(); err != nil {
		return TrimResult{}, err
	}
	stagedCopy, err := stageTrimmedImage(ctx, input, staged, plan.OutputBytes)
	if err != nil {
		return TrimResult{}, err
	}
	notify("verifying copy", 0, plan.OutputBytes)
	notify("publishing", 0, 0)
	// Progress callbacks are caller code. Finish every source and staged-file
	// check after the last callback that runs before publication.
	if err := stage.Check(); err != nil {
		return TrimResult{}, err
	}
	if err := checkInputs(inputs); err != nil {
		return TrimResult{}, err
	}
	verified, err := inspectTrim(ctx, input, plan.InputBytes)
	if err != nil {
		return TrimResult{}, err
	}
	if verified.OutputBytes != plan.OutputBytes || verified.Format != plan.Format {
		return TrimResult{}, errors.New("cartridge boundary changed during trim")
	}
	verifiedCopy, err := verifyTrimmedImage(ctx, input, staged, stagedCopy)
	if err != nil {
		return TrimResult{}, err
	}
	if err := checkInputs(inputs); err != nil {
		return TrimResult{}, err
	}
	if err := stage.Check(); err != nil {
		return TrimResult{}, err
	}
	if _, err := publishRegularFile(ctx, staged, output, verifiedCopy); err != nil {
		return TrimResult{}, err
	}
	notify("complete", plan.OutputBytes, plan.OutputBytes)
	return TrimResult{TrimPlan: verified, Output: output, SHA256: hex.EncodeToString(stagedCopy.digest[:])}, nil
}

func openTrimSource(ctx context.Context, source string) (*os.File, map[string]os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !filepath.IsLocal(filepath.Base(source)) {
		return nil, nil, errors.New("trim source must name a regular file")
	}
	before, err := fileops.SnapshotStat(source, os.Lstat)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errors.New("trim source must be a regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return nil, nil, err
	}
	actual, err := input.Stat()
	if err != nil {
		input.Close()
		return nil, nil, err
	}
	if !fileops.SameSnapshot(before, actual) {
		input.Close()
		return nil, nil, errors.New("source changed while opening for trim")
	}
	return input, map[string]os.FileInfo{source: actual}, nil
}

func inspectTrim(ctx context.Context, input *os.File, size int64) (TrimPlan, error) {
	return inspectTrimWithProgress(ctx, input, size, nil)
}

func inspectTrimWithProgress(ctx context.Context, input *os.File, size int64, notify func(string, int64, int64)) (TrimPlan, error) {
	plan, err := readTrimLayout(ctx, input, size)
	if err != nil || plan.OutputBytes == size {
		return plan, err
	}
	paddingSize := size - plan.OutputBytes
	if notify != nil {
		notify("checking padding", 0, paddingSize)
	}
	suffix := io.NewSectionReader(input, plan.OutputBytes, size-plan.OutputBytes)
	plan.PaddingByte, err = scanTrimPadding(ctx, suffix, paddingSize, func(done int64) {
		if notify != nil {
			notify("checking padding", done, paddingSize)
		}
	})
	if err != nil {
		return TrimPlan{}, err
	}
	return plan, nil
}

func readTrimLayout(ctx context.Context, input *os.File, size int64) (TrimPlan, error) {
	if err := ctx.Err(); err != nil {
		return TrimPlan{}, err
	}
	var plan TrimPlan
	var end int64
	var err error
	switch strings.ToLower(filepath.Ext(input.Name())) {
	case ".nds":
		plan.Format = "nds"
		end, err = cardtrim.NDSBoundary(ctx, input, size)
	case ".3ds", ".cci":
		plan.Format = "3ds"
		end, err = cardtrim.N3DSBoundary(input, size)
	case ".xci":
		plan.Format = "xci"
		end, err = cardtrim.XCIBoundary(ctx, input, size)
	default:
		return TrimPlan{}, errors.New("trim supports .nds, .3ds, Nintendo 3DS .cci, and .xci images")
	}
	if err != nil {
		return TrimPlan{}, fmt.Errorf("validate %s for trim: %w", plan.Format, err)
	}
	if end <= 0 || end > size {
		return TrimPlan{}, errors.New("validated cartridge boundary is outside the input")
	}
	plan.InputBytes, plan.OutputBytes = size, end
	return plan, ctx.Err()
}

func trimPadding(ctx context.Context, input io.Reader, size int64) (byte, error) {
	return scanTrimPadding(ctx, input, size, nil)
}

func scanTrimPadding(ctx context.Context, input io.Reader, size int64, progress func(int64)) (byte, error) {
	reader := &fileops.ContextReader{Context: ctx, Reader: input}
	var marker [1]byte
	if _, err := io.ReadFull(reader, marker[:]); err != nil {
		return 0, fmt.Errorf("read cartridge padding: %w", err)
	}
	if marker[0] != 0 && marker[0] != 0xff {
		return 0, errors.New("cartridge suffix contains data beyond the declared boundary")
	}
	buffer := make([]byte, 128<<10)
	for remaining := size - 1; remaining > 0; {
		chunk := buffer[:min(int64(len(buffer)), remaining)]
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return 0, fmt.Errorf("read cartridge padding: %w", err)
		}
		if bytes.Count(chunk, marker[:]) != len(chunk) {
			return 0, errors.New("cartridge suffix is not uniform padding")
		}
		remaining -= int64(len(chunk))
		if progress != nil {
			progress(size - remaining)
		}
	}
	return marker[0], ctx.Err()
}

type trimmedCopy struct {
	info   os.FileInfo
	digest [sha256.Size]byte
}

func stageTrimmedImage(ctx context.Context, input *os.File, staged string, size int64) (trimmedCopy, error) {
	file, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return trimmedCopy{}, err
	}
	hash := sha256.New()
	prefix := io.NewSectionReader(input, 0, size)
	n, copyErr := io.Copy(io.MultiWriter(file, hash), &fileops.ContextReader{Context: ctx, Reader: prefix})
	info, statErr := file.Stat()
	if err := errors.Join(copyErr, statErr, file.Close()); err != nil {
		return trimmedCopy{}, err
	}
	if n != size || info.Size() != size {
		return trimmedCopy{}, io.ErrUnexpectedEOF
	}
	stagedCopy := trimmedCopy{info: info}
	hash.Sum(stagedCopy.digest[:0])
	return stagedCopy, ctx.Err()
}

func verifyTrimmedImage(ctx context.Context, input *os.File, staged string, stagedCopy trimmedCopy) (os.FileInfo, error) {
	size := stagedCopy.info.Size()
	info, err := os.Lstat(staged)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != size || !os.SameFile(stagedCopy.info, info) {
		return nil, errors.New("staged trim was replaced or resized")
	}
	file, err := os.Open(staged)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !fileops.SameSnapshot(info, opened) {
		return nil, errors.New("staged trim changed while opening")
	}
	prefix := io.NewSectionReader(input, 0, size)
	original := make([]byte, 128<<10)
	copied := make([]byte, len(original))
	hash := sha256.New()
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := min(int64(len(original)), remaining)
		if _, err := io.ReadFull(prefix, original[:n]); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(file, copied[:n]); err != nil {
			return nil, err
		}
		if !bytes.Equal(original[:n], copied[:n]) {
			return nil, errors.New("staged trim does not reproduce the original prefix")
		}
		hash.Write(copied[:n])
		remaining -= n
	}
	if !bytes.Equal(stagedCopy.digest[:], hash.Sum(nil)) {
		return nil, errors.New("staged trim changed after it was copied")
	}
	return opened, ctx.Err()
}
