package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"golang.org/x/sys/unix"
)

func TestExternalReadersRejectFIFOs(t *testing.T) {
	for name, read := range map[string]func(context.Context, string) error{
		"digest": func(ctx context.Context, path string) error {
			_, err := fileops.Digest(ctx, path)
			return err
		},
		"Z3DS": func(ctx context.Context, path string) error {
			_, err := read3DSHeader(ctx, path, 1024)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.zcci")
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- read(ctx, path) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "regular file") {
					t.Fatalf("read = %v; want regular file rejection", err)
				}
			case <-ctx.Done():
				t.Fatal("verification blocked while opening a FIFO")
			}
		})
	}
}
