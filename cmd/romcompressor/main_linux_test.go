package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBatchRejectsFIFOManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readManifest(context.Background(), path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("expected a regular-file error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("opening the FIFO blocked manifest validation")
	}
}
