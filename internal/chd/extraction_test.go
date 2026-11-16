package chd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDVDCHDRejectsPartialSector(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "partial.iso")
	if err := os.WriteFile(source, make([]byte, dvdSectorSize+1), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "partial.chd")
	if err := Convert(context.Background(), os.Args[0], source, output, false, "dvd", testMaxOutput); err == nil || !strings.Contains(err.Error(), "complete 2048-byte sectors") {
		t.Fatalf("expected partial DVD sector rejection, got %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("partial DVD was published: %v", err)
	}
}

func TestExtractedCDRequiresItsStagedTrack(t *testing.T) {
	dir := t.TempDir()
	cue, bin := filepath.Join(dir, "game.cue"), filepath.Join(dir, "game.bin")
	other := filepath.Join(dir, "other.bin")
	if err := os.WriteFile(bin, make([]byte, 2352), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte{4, 5, 6}, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		reference string
		valid     bool
	}{
		{"game.bin", true}, {"other.bin", false}, {"missing.bin", false}, {"../outside.bin", false},
	} {
		if err := os.WriteFile(cue, []byte("FILE \""+tc.reference+"\" BINARY\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readExtractedCHDCUE(cue, bin, testMaxOutput)
		if (err == nil) != tc.valid {
			t.Errorf("reference %q: %v", tc.reference, err)
		}
	}
	if err := os.WriteFile(bin, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cue, []byte("FILE \"game.bin\" BINARY\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readExtractedCHDCUE(cue, bin, testMaxOutput); err == nil {
		t.Fatal("accepted empty extracted track")
	}
}
