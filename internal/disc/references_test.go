package disc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCueRejectsDirectiveHiddenAfterChdmanLineLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disc.cue")
	text := "REM " + strings.Repeat(" ", 506) + "FILE \"../outside.bin\" BINARY\nFILE \"track.bin\" BINARY\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "track.bin"), []byte{0}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReferences(path); err == nil || !strings.Contains(err.Error(), "510-byte line limit") {
		t.Fatalf("expected rejection of split line, got %v", err)
	}
}

func TestDiscReferencesRejectUnconfinedPathSyntax(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "track.bin"), []byte{0}, 0600); err != nil {
		t.Fatal(err)
	}
	formats := []struct {
		ext  string
		text string
	}{
		{"cue", "FILE \"%s\" BINARY\n TRACK 01 MODE1/2352\n INDEX 01 00:00:00\n"},
		{"gdi", "1\n1 0 4 2352 \"%s\" 0\n"},
	}
	paths := []struct {
		name    string
		ref     string
		wantErr string
	}{
		{"slash parent", "nested/../track.bin", "parent directory"},
		{"backslash parent", `nested\..\track.bin`, "parent directory"},
		{"rooted", `/track.bin`, "must be relative"},
		{"drive rooted", `\track.bin`, "must be relative"},
	}
	for _, format := range formats {
		for _, path := range paths {
			t.Run(format.ext+"/"+path.name, func(t *testing.T) {
				descriptor := filepath.Join(dir, "disc."+format.ext)
				if err := os.WriteFile(descriptor, []byte(fmt.Sprintf(format.text, path.ref)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := ValidateReferences(descriptor); err == nil || !strings.Contains(err.Error(), path.wantErr) {
					t.Fatalf("reference %q returned %v, want %q", path.ref, err, path.wantErr)
				}
			})
		}
	}
}
