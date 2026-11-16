// Package engine adapts explicitly configured external format tools.
// It writes only into staging paths supplied by the file lifecycle owner.
package engine

import (
	"context"
)

// Config describes a staged conversion without depending on the public API types.
type Config struct {
	Format    string
	Entry     string
	Media     string
	Keys      string
	MaxOutput int64
	TempDir   string
	Tools     map[string]string
	Progress  func(phase string, bytes, total int64)
	// Inspect uses the lifecycle owner's canonical signature detection and is
	// required for Dolphin conversions.
	Inspect func(context.Context, string) (format string, size int64, err error)
}

// Entry describes a regular file reported by an external archive engine.
type Entry struct {
	Name string
	Size uint64
}
