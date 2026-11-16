//go:build !windows

package fileops

import (
	"os"
	"path/filepath"
	"syscall"
)

// Resolve resolves links before containment checks.
func Resolve(path string) (string, error) { return filepath.EvalSymlinks(path) }

// OpenDirectory opens a directory for bounded staging scans.
func OpenDirectory(path string) (*os.File, error) { return os.Open(path) }

// LinkCount returns the number of hard-linked names for an opened file.
func LinkCount(file *os.File) (uint64, error) {
	var info syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.Nlink), nil
}
