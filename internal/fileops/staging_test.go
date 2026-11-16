package fileops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
