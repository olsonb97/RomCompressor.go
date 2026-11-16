package fileops

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishFileNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "staged")
	destination := filepath.Join(dir, "existing")
	if err := os.WriteFile(source, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Publish(source, destination); err == nil {
		t.Fatal("existing destination replaced")
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "original" {
		t.Fatalf("original changed: %v", err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := Publish(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(destination)
	if err != nil || string(got) != "new" {
		t.Fatalf("published data incorrect: %v", err)
	}
}

func TestOpenRegularIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	original := []byte("original bytes")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	file, _, err := OpenRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write([]byte("replacement")); err == nil {
		t.Fatal("source handle permits writing")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("original changed: %v", err)
	}
	if _, _, err := OpenRegular(filepath.Dir(path)); err == nil {
		t.Fatal("opened a directory as a regular file")
	}
}

func TestSnapshotDetectsReplacementWithMatchingMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selected")
	replacement := filepath.Join(dir, "replacement")
	for _, name := range []string{path, replacement} {
		if err := os.WriteFile(name, []byte("same bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := SnapshotStat(path, os.Stat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(dir, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if SameSnapshot(before, current) {
		t.Fatal("replacement with matching size and timestamp reused source identity")
	}
}

func TestRequirePrivateRejectsHardLinkedStage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "staged")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := RequirePrivate(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "other-name")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := RequirePrivate(file); err == nil {
		t.Fatal("accepted a staged file with another hard-linked name")
	}
}

func TestOpenDirectoryAllowsRename(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scratch")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := os.Rename(path, filepath.Join(root, "moved")); err != nil {
		t.Fatalf("holding the stage directory blocked a scratch rename: %v", err)
	}
}
