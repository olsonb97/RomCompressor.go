//go:build linux

package fileops

import (
	"golang.org/x/sys/unix"
)

// Publish moves source to destination exclusively, preserving an existing path.
// Exclusive rename also supports filesystems such as exFAT that have no hard links.
func Publish(source, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}
