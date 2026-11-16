//go:build darwin

package fileops

import (
	"golang.org/x/sys/unix"
)

// Publish moves source to destination exclusively, preserving an existing path.
func Publish(source, destination string) error {
	return unix.RenamexNp(source, destination, unix.RENAME_EXCL)
}
