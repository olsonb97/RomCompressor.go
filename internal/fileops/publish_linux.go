//go:build linux

package fileops

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Publish moves source to destination exclusively, preserving an existing path.
func Publish(source, destination string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	switch err {
	case unix.EINVAL, unix.EOPNOTSUPP, unix.ENOSYS:
		return fmt.Errorf("exclusive rename failed (try an output folder on a filesystem supporting RENAME_NOREPLACE): %w", err)
	default:
		return err
	}
}
