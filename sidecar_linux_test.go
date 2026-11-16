package romcompressor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSBIMixedCaseAliasesAreUnambiguousOnlyForSameFile(t *testing.T) {
	for _, sameFile := range []bool{false, true} {
		name := "separate files"
		if sameFile {
			name = "hard links"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game.iso")
			first := filepath.Join(dir, "game.sbi")
			second := filepath.Join(dir, "game.sBi")
			output := filepath.Join(dir, "packed.cso")
			raw := make([]byte, 8192)
			sbi := []byte("SBI\x00generated shared subchannel data")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(first, sbi, 0600); err != nil {
				t.Fatal(err)
			}
			if sameFile {
				if err := os.Link(first, second); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(second, sbi, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := Convert(context.Background(), source, output, Options{Format: CSO})
			if sameFile {
				if err != nil || !result.Verified {
					t.Fatalf("same-file case aliases were rejected: %+v, %v", result, err)
				}
				if actual, err := os.ReadFile(filepath.Join(dir, "packed.sbi")); err != nil || !bytes.Equal(actual, sbi) {
					t.Fatalf("shared SBI companion was omitted or changed: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "multiple SBI companions") {
					t.Fatalf("ambiguous case variants were accepted: %+v, %v", result, err)
				}
				entries, readErr := os.ReadDir(dir)
				if readErr != nil || len(entries) != 3 {
					t.Fatalf("ambiguity left output or staging files: %v, %v", entries, readErr)
				}
			}
			for path, expected := range map[string][]byte{source: raw, first: sbi, second: sbi} {
				if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("case alias handling changed an original: %v", err)
				}
			}
		})
	}
}

func TestSBIRejectsReplacementFIFO(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "original.sbi")
	if err := os.WriteFile(input, []byte{'S', 'B', 'I', 0, 1}, 0600); err != nil {
		t.Fatal(err)
	}
	sidecar, err := findSBI(filepath.Join(dir, "original.iso"))
	if err != nil || sidecar == nil {
		t.Fatalf("snapshot SBI: %v, %v", sidecar, err)
	}
	if err := os.Remove(input); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(input, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sidecar.stage(ctx, filepath.Join(dir, "converted.cso"), maxSBISize)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("stage = %v; want regular file rejection", err)
		}
	case <-ctx.Done():
		t.Fatal("staging blocked while opening a FIFO")
	}
}
