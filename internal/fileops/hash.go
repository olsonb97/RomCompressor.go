package fileops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
)

// CompareFiles compares complete file digests while checking source stability.
func CompareFiles(ctx context.Context, first, second string) error {
	a, err := Digest(ctx, first)
	if err != nil {
		return err
	}
	b, err := Digest(ctx, second)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("verification failed: restored bytes differ from the source")
	}
	return nil
}

// Digest returns a file's SHA-256 after a bounded, cancellation-aware read.
func Digest(ctx context.Context, path string) ([32]byte, error) {
	var result [32]byte
	digest := sha256.New()
	if err := HashFile(ctx, path, digest); err != nil {
		return result, err
	}
	digest.Sum(result[:0])
	return result, nil
}

// HashFile streams a stable regular file into digest without resetting it.
func HashFile(ctx context.Context, path string, digest hash.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, before, err := OpenRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := &ContextReader{Context: ctx, Reader: io.NewSectionReader(file, 0, before.Size())}
	n, err := io.Copy(digest, reader)
	if err != nil {
		return err
	}
	if n != before.Size() {
		return io.ErrUnexpectedEOF
	}
	after, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !SameSnapshot(before, after) {
		return fmt.Errorf("external verification file changed while hashing")
	}
	return ctx.Err()
}
