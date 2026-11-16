package romcompressor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

func snapshotInputs(path string) (map[string]os.FileInfo, error) {
	st, err := fileops.SnapshotStat(path, os.Stat)
	if err != nil {
		return nil, err
	}
	inputs := map[string]os.FileInfo{path: st}
	err = disc.VisitReferences(path, func(p string, st os.FileInfo) error { inputs[p] = st; return nil })
	return inputs, err
}

func checkInputs(inputs map[string]os.FileInfo) error {
	for path, before := range inputs {
		after, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("source became unavailable: %w", err)
		}
		if !fileops.SameSnapshot(before, after) {
			return fmt.Errorf("source changed during conversion")
		}
	}
	return nil
}

// Platform publication provides an atomic, exclusive create.
// Rollback removes only this operation's outputs. The entry point is last.
func publish(ctx context.Context, stage, staged, output string, limit int64, companions ...string) (int64, error) {
	allowed := map[string]os.FileInfo{staged: nil}
	for _, path := range companions {
		allowed[path] = nil
	}
	entries, err := readPublishEntries(stage, len(allowed)+1)
	if err != nil {
		return 0, err
	}
	var size int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return 0, fmt.Errorf("unexpected output entry %q", entry.Name())
		}
		path := filepath.Join(stage, entry.Name())
		if _, ok := allowed[path]; !ok {
			return 0, fmt.Errorf("unexpected output file %q", entry.Name())
		}
		st, err := fileops.SnapshotStat(path, os.Lstat)
		if err != nil {
			return 0, err
		}
		if !st.Mode().IsRegular() {
			return 0, fmt.Errorf("unexpected output entry %q", entry.Name())
		}
		if st.Size() > limit-size {
			return 0, fmt.Errorf("output exceeds size limit")
		}
		size += st.Size()
		allowed[path] = st
	}
	if len(entries) != len(allowed) {
		return 0, fmt.Errorf("staging is missing a requested output or companion")
	}
	created := make([]publishedPath, 0, len(allowed))
	for _, path := range companions {
		destination := filepath.Join(filepath.Dir(output), filepath.Base(path))
		item, err := publishRegularFile(ctx, path, destination, allowed[path])
		if err != nil {
			return 0, rollbackPublished(created, err)
		}
		created = append(created, item)
	}
	if _, err := publishRegularFile(ctx, staged, output, allowed[staged]); err != nil {
		return 0, rollbackPublished(created, err)
	}
	return size, nil
}

func readPublishEntries(dir string, count int) ([]os.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(count)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return entries, nil
}

type publishedPath struct {
	path string
	info os.FileInfo
}

func publishRegularFile(ctx context.Context, source, destination string, expected os.FileInfo) (publishedPath, error) {
	if err := ctx.Err(); err != nil {
		return publishedPath{}, err
	}
	before, err := fileops.SnapshotStat(source, os.Lstat)
	if err != nil {
		return publishedPath{}, err
	}
	if !before.Mode().IsRegular() {
		return publishedPath{}, fmt.Errorf("staged output must be a regular file")
	}
	if expected != nil && !fileops.SameSnapshot(expected, before) {
		return publishedPath{}, fmt.Errorf("staged output changed before publication")
	}
	f, err := os.OpenFile(source, os.O_RDWR, 0)
	if err != nil {
		return publishedPath{}, err
	}
	info, statErr := f.Stat()
	if statErr != nil || !fileops.SameSnapshot(before, info) {
		f.Close()
		return publishedPath{}, fmt.Errorf("staged output changed while opening")
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return publishedPath{}, err
	}
	if err := ctx.Err(); err != nil {
		return publishedPath{}, err
	}
	current, err := os.Lstat(source)
	if err != nil {
		return publishedPath{}, err
	}
	if !current.Mode().IsRegular() || !fileops.SameSnapshot(info, current) {
		return publishedPath{}, fmt.Errorf("staged output changed before publication")
	}
	if err := fileops.Publish(source, destination); err != nil {
		return publishedPath{}, fmt.Errorf("publish %s without replacing files: %w", filepath.Base(destination), err)
	}
	return publishedPath{path: destination, info: info}, nil
}

func rollbackPublished(created []publishedPath, cause error) error {
	for i := len(created) - 1; i >= 0; i-- {
		item := created[i]
		current, err := os.Lstat(item.path)
		if err == nil && os.SameFile(item.info, current) {
			err = os.Remove(item.path)
		}
		if err != nil && !os.IsNotExist(err) {
			cause = errors.Join(cause, fmt.Errorf("remove incomplete output: %w", err))
		}
	}
	return cause
}
