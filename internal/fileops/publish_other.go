//go:build !windows && !linux && !darwin

package fileops

import (
	"os"
)

// Publish links source to destination exclusively, preserving an existing path.
// Other Unix targets require a filesystem with hard-link support.
func Publish(source, destination string) error { return os.Link(source, destination) }
