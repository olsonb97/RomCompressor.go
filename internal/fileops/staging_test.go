package fileops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageReadAtPreservesEncoderPosition(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "output-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stage := NewBoundedStageFile(f, 4)
	if _, err := stage.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	var data [2]byte
	if n, err := stage.ReadAt(data[:], 1); n != 2 || err != nil || string(data[:]) != "bc" {
		t.Fatalf("read staged bytes: %q, %d, %v", data, n, err)
	}
	if n, err := stage.ReadAt(data[:], 2); n != 1 || !errors.Is(err, io.EOF) || data[0] != 'c' {
		t.Fatalf("read past staged data: %q, %d, %v", data, n, err)
	}
	if _, err := stage.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	var result [4]byte
	if _, err := stage.ReadAt(result[:], 0); err != nil || string(result[:]) != "abcd" {
		t.Fatalf("resumed write changed prior output: %q, %v", result, err)
	}
}

func TestScanDuringScratchCleanup(t *testing.T) {
	ctx := context.Background()
	stage := t.TempDir()
	done := make(chan error, 1)
	go func() {
		for i := range 500 {
			path := filepath.Join(stage, fmt.Sprintf("scratch-%d", i))
			if err := os.Mkdir(path, 0700); err != nil {
				done <- err
				return
			}
			if err := os.Remove(path); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
scanning:
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			break scanning
		default:
			if err := Scan(ctx, stage, 100, 10000); err != nil {
				<-done
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	if err := Scan(ctx, stage, 100, 100); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disappearing staging root: %v", err)
	}
}

func TestStageCleanupDoesNotRemoveReplacement(t *testing.T) {
	root := t.TempDir()
	stage, err := NewStage(root, "stage-*")
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "moved")
	if err := os.Rename(stage.Path(), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage.Path(), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stage.Path(), "keep")
	if err := os.WriteFile(marker, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stage.Check(); err == nil {
		t.Fatal("accepted a replacement staging directory")
	}
	stage.Cleanup()
	if data, err := os.ReadFile(marker); err != nil || string(data) != "replacement" {
		t.Fatalf("cleanup touched replacement directory: %v", err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("cleanup touched moved original directory: %v", err)
	}
}

func TestMonitorChecksBoundsOnFinish(t *testing.T) {
	stage := t.TempDir()
	scratch := filepath.Join(stage, "scratch")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 130 {
		if err := os.WriteFile(filepath.Join(scratch, fmt.Sprintf("%d.tmp", i)), []byte{1}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name       string
		maxBytes   int64
		maxEntries int
		err        string
	}{
		{"fits", 130, 132, ""},
		{"byte limit", 129, 132, "output limit"},
		{"entry limit", 130, 131, "exceeds 131 entries"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done, stopped := make(chan struct{}), make(chan struct{})
			close(done) // The engine exited before the first periodic scan.
			Monitor(ctx, cancel, stage, test.maxBytes, test.maxEntries, done, stopped)
			err := context.Cause(ctx)
			if test.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Fatalf("expected %q, got %v", test.err, err)
			}
		})
	}
}
