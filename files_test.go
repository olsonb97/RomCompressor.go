package romcompressor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestCHDRejectsUnvalidatedCDDescriptor(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.toc")
	if err := os.WriteFile(source, []byte("CD_DA\nTRACK AUDIO\nFILE \"../outside.bin\" 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{Format: CHD, Media: "cd", Tools: map[string]string{"chdman": os.Args[0]}}
	if _, err := Convert(context.Background(), source, filepath.Join(dir, "out.chd"), opts); err == nil || !strings.Contains(err.Error(), "CUE/GDI descriptor") {
		t.Fatalf("expected rejection before invoking chdman, got %v", err)
	}
}

func TestCHDRejectsRetargetedDescriptorDirectory(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const name = "disc.cue"
	const cue = "FILE track.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n"
	if err := os.WriteFile(filepath.Join(first, name), []byte(cue), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(first, name), filepath.Join(second, name)); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	for dir, value := range map[string]byte{first: 0x19, second: 0x47} {
		data := bytes.Repeat([]byte{value}, 4*2048)
		if tool != "" {
			data = testfixture.CHDTrack(data, 2048)
		}
		if err := os.WriteFile(filepath.Join(dir, "track.bin"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "selected")
	testfixture.DirectoryLink(t, alias, first)
	source := filepath.Join(alias, name)
	retarget := func() {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		testfixture.DirectoryLink(t, alias, second)
	}
	if tool == "" {
		before, err := snapshotInputs(source)
		if err != nil {
			t.Fatal(err)
		}
		retarget()
		if err := checkInputs(before); err == nil || !strings.Contains(err.Error(), "source changed") {
			t.Fatalf("selected track changed while the CUE stayed the same: %v", err)
		}
		return
	}
	ctx := context.Background()
	opts := Options{Format: CHD, Tools: map[string]string{"chdman": tool}}
	if _, err := Convert(ctx, source, filepath.Join(root, "control.chd"), opts); err != nil {
		t.Fatalf("stationary directory link: %v", err)
	}
	changed := false
	opts.Progress = func(progress Progress) {
		if progress.Phase == "converting" && !changed {
			retarget()
			changed = true
		}
	}
	output := filepath.Join(root, "changed.chd")
	if _, err := Convert(ctx, source, output, opts); !changed || err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Errorf("Convert accepted a retargeted descriptor directory: %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Errorf("retargeted conversion published an output: %v", err)
	}
}

func TestDiscReferencesRejectLinkedTrackOutsideDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "track.bin"), []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	testfixture.DirectoryLink(t, filepath.Join(root, "linked"), outside)
	descriptor := filepath.Join(root, "game.cue")
	cue := "FILE linked/track.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n"
	if err := os.WriteFile(descriptor, []byte(cue), 0600); err != nil {
		t.Fatal(err)
	}
	if err := disc.ValidateReferences(descriptor); err == nil || !strings.Contains(err.Error(), "leaves its folder") {
		t.Fatalf("expected rejection of an out-of-folder linked track, got %v", err)
	}
}

func TestEngineStageScanRejectsLinkedDirectory(t *testing.T) {
	stage, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(stage, "scratch")
	testfixture.DirectoryLink(t, link, outside)
	if err := fileops.Scan(context.Background(), stage, 1, 3); err == nil || !strings.Contains(err.Error(), "link or special file") {
		t.Fatalf("expected rejection of linked staging directory, got %v", err)
	}
}

func TestReferencedSymlinkReplacementInvalidatesSnapshot(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.bin")
	second := filepath.Join(dir, "second.bin")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, make([]byte, 2352), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "track.bin")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cue := filepath.Join(dir, "game.cue")
	if err := os.WriteFile(cue, []byte("FILE \"track.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"), 0600); err != nil {
		t.Fatal(err)
	}
	inputs, err := snapshotInputs(cue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if err := checkInputs(inputs); err == nil {
		t.Fatal("changed symlink target ignored")
	}
}
