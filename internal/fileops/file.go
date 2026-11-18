// Package fileops provides shared file identity, opening, and exclusive
// publication operations. Callers own staging lifetimes and rollback policy.
package fileops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SnapshotStat captures file identity while path still names the selected file.
// The stat function selects whether to follow a final symbolic link.
func SnapshotStat(path string, stat func(string) (os.FileInfo, error)) (os.FileInfo, error) {
	info, err := stat(path)
	if err != nil {
		return nil, err
	}
	// Windows path-based FileInfo loads its file ID on the first SameFile
	// call. Capture it now while the path still names the selected file.
	if !os.SameFile(info, info) {
		// A scratch entry can disappear between stat and Windows' lazy ID
		// lookup. Preserve the filesystem error for callers monitoring it.
		if _, err := stat(path); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("snapshot file identity for %q", path)
	}
	return info, nil
}

// OpenRegular opens a regular file read-only and checks that its identity, size,
// and modification time did not change between selection and opening.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	// Check the file type before opening so named pipes cannot block inspection.
	if !filepath.IsLocal(filepath.Base(path)) {
		return nil, nil, fmt.Errorf("source must name a regular file")
	}
	before, err := SnapshotStat(path, os.Stat)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("source must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	actual, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !actual.Mode().IsRegular() || !SameSnapshot(before, actual) {
		f.Close()
		return nil, nil, fmt.Errorf("source changed while opening")
	}
	return f, actual, nil
}

// SameSnapshot checks identity, size, and modification time together.
func SameSnapshot(before, current os.FileInfo) bool {
	return os.SameFile(before, current) && before.Size() == current.Size() && before.ModTime().Equal(current.ModTime())
}

// RequirePrivate rejects an opened staging file with any other hard-linked name.
func RequirePrivate(file *os.File) error {
	links, err := LinkCount(file)
	if err != nil {
		return err
	}
	if links != 1 {
		return errors.New("staged engine output must not have other hard links")
	}
	return nil
}
