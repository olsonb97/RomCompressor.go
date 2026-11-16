package romcompressor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

const maxSBISize = 1 << 20

type discSidecar struct {
	path   string
	info   os.FileInfo
	shared bool
}

func discSidecarSupported(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cue", ".chd", ".bin", ".img", ".iso", ".cdr", ".toast", ".cdi", ".nrg", ".cso", ".zso", ".ecm", ".pbp":
		return true
	default:
		return false
	}
}

// SBI holds PlayStation subchannel data outside the disc image. Preserve it
// beside renamed disc outputs; single-file archives retain their explicit scope.
func findDiscSidecar(source, output string) (*discSidecar, error) {
	if !discSidecarSupported(source) || !discSidecarSupported(output) {
		return nil, nil
	}
	sidecar, err := findSBI(source)
	if err != nil {
		return nil, err
	}
	existing, err := findSBI(output)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if sidecar == nil || !os.SameFile(existing.info, sidecar.info) {
			return nil, &os.PathError{Op: "create SBI companion", Path: existing.path, Err: os.ErrExist}
		}
		// Converting beside the original can share its unchanged SBI. The
		// existing source is never staged, republished, or included in rollback.
		sidecar.shared = true
	}
	return sidecar, nil
}

func findSBI(disc string) (*discSidecar, error) {
	stem := strings.TrimSuffix(disc, filepath.Ext(disc))
	var found *discSidecar
	// Check every ASCII casing without enumerating an unbounded directory.
	for _, suffix := range []string{".sbi", ".sbI", ".sBi", ".sBI", ".Sbi", ".SbI", ".SBi", ".SBI"} {
		path := stem + suffix
		info, err := fileops.SnapshotStat(path, os.Lstat)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read SBI companion: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxSBISize {
			return nil, fmt.Errorf("SBI companion must be a regular file of at most 1 MiB")
		}
		if found != nil && !os.SameFile(found.info, info) {
			return nil, fmt.Errorf("multiple SBI companions match the disc")
		}
		if found == nil {
			found = &discSidecar{path: path, info: info}
		}
	}
	return found, nil
}

func (s *discSidecar) stage(ctx context.Context, output string, limit int64) (string, error) {
	if s == nil || s.shared {
		return "", nil
	}
	if s.info.Size() > limit {
		return "", fmt.Errorf("SBI companion exceeds output limit")
	}
	src, info, err := fileops.OpenRegular(s.path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	if !fileops.SameSnapshot(s.info, info) {
		return "", errors.New("SBI companion changed during conversion")
	}
	data, err := io.ReadAll(io.LimitReader(&fileops.ContextReader{Context: ctx, Reader: src}, maxSBISize+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) != s.info.Size() || !bytes.HasPrefix(data, []byte{'S', 'B', 'I', 0}) {
		return "", fmt.Errorf("SBI companion is invalid or changed during conversion")
	}
	path := strings.TrimSuffix(output, filepath.Ext(output)) + ".sbi"
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	n, err := dst.Write(data)
	if err != nil {
		return "", err
	}
	if n != len(data) {
		return "", io.ErrShortWrite
	}
	if err := dst.Sync(); err != nil {
		return "", err
	}
	actual := make([]byte, len(data))
	if _, err := dst.ReadAt(actual, 0); err != nil {
		return "", err
	}
	if !bytes.Equal(data, actual) {
		return "", errors.New("copied SBI companion does not match its source")
	}
	return path, ctx.Err()
}
