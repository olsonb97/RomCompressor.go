package fileops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
)

// ContextReader stops before the next read when its context is cancelled.
type ContextReader struct {
	Context context.Context
	Reader  io.Reader
}

// Read checks cancellation before delegating to the underlying reader.
func (r *ContextReader) Read(p []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// Stage records the identity of a private staging directory.
type Stage struct {
	path string
	info os.FileInfo
}

// NewStage creates and snapshots a private directory beside its destination.
func NewStage(parent, pattern string) (Stage, error) {
	path, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return Stage{}, err
	}
	info, err := SnapshotStat(path, os.Lstat)
	if err != nil {
		return Stage{}, err
	}
	if !info.IsDir() {
		return Stage{}, errors.New("staging path is not a directory")
	}
	return Stage{path: path, info: info}, nil
}

// Path returns the directory created by NewStage.
func (stage Stage) Path() string { return stage.path }

// CheckDirectory verifies that path still names the selected directory.
func CheckDirectory(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(expected, current) {
		return errors.New("staging directory was replaced")
	}
	return nil
}

// Check verifies that the staging directory has not been replaced.
func (stage Stage) Check() error { return CheckDirectory(stage.path, stage.info) }

// Cleanup removes the stage only while its recorded directory identity matches.
func (stage Stage) Cleanup() {
	if stage.Check() == nil {
		_ = os.RemoveAll(stage.path)
	}
}

// BoundedStageFile bounds writes by their position, including writes after seeks
// used by native encoders to fill headers or indexes.
type BoundedStageFile struct {
	file     *os.File
	limit    int64
	position int64
}

// NewBoundedStageFile wraps a newly opened staging file at offset zero.
func NewBoundedStageFile(file *os.File, limit int64) *BoundedStageFile {
	return &BoundedStageFile{file: file, limit: limit}
}

// Write rejects writes past the configured output limit.
func (w *BoundedStageFile) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit-w.position {
		return 0, errors.New("staged output exceeds size limit")
	}
	n, err := w.file.Write(data)
	w.position += int64(n)
	return n, err
}

// Seek tracks the write position and rejects positions beyond the output limit.
func (w *BoundedStageFile) Seek(offset int64, whence int) (int64, error) {
	position, err := w.file.Seek(offset, whence)
	if err != nil {
		return position, err
	}
	w.position = position
	if position > w.limit {
		return position, errors.New("staged output exceeds size limit")
	}
	return position, nil
}

// ReadAt delegates reads needed by streaming encoders that reuse staged data.
func (w *BoundedStageFile) ReadAt(data []byte, offset int64) (int, error) {
	return w.file.ReadAt(data, offset)
}

// WriteText creates a new staging file without replacing an existing path.
func WriteText(path, contents string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(contents)
	return errors.Join(writeErr, file.Close())
}

// CheckDigest verifies the size, SHA-256 digest, and identity of an opened stage.
func CheckDigest(ctx context.Context, file *os.File, size int64, expected []byte) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return errors.New("staged output does not have the expected size")
	}
	actual := sha256.New()
	reader := &ContextReader{Context: ctx, Reader: io.NewSectionReader(file, 0, size)}
	n, err := io.Copy(actual, reader)
	if err != nil {
		return err
	}
	if n != size || !bytes.Equal(actual.Sum(nil), expected) {
		return errors.New("staged output does not reproduce the expected bytes")
	}
	current, err := os.Lstat(file.Name())
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !SameSnapshot(info, current) {
		return errors.New("staged output changed while checking its digest")
	}
	return ctx.Err()
}
