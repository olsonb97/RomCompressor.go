package romcompressor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyChecksTempDirWithoutCreatingNativeScratch(t *testing.T) {
	directory := t.TempDir()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("generated game data")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "game.gz")
	if err := os.WriteFile(source, compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(directory, "scratch")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), source, Options{TempDir: parent}); err != nil {
		t.Fatalf("native verification with selected parent: %v", err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("native verification created scratch unnecessarily: %v, %v", entries, err)
	}
	missing := filepath.Join(directory, "missing")
	if err := Verify(context.Background(), source, Options{TempDir: missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid temporary parent was ignored: %v", err)
	}
	if err := Verify(context.Background(), source, Options{TempDir: source}); err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Fatalf("file accepted as native verification scratch parent: %v", err)
	}
	if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, compressed.Bytes()) {
		t.Fatalf("verification changed source container: %v", err)
	}
}

func TestVerificationTempDirValidationIsReadOnly(t *testing.T) {
	directory := t.TempDir()
	parent := filepath.Join(directory, "scratch")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(parent, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("existing user data"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(directory)
	if got, err := normalizeVerifyTempDir("scratch"); err != nil || got != parent {
		t.Fatalf("selected temporary parent = %q, %v", got, err)
	}
	if got, err := normalizeVerifyTempDir(""); err != nil || got != "" {
		t.Fatalf("default temporary parent = %q, %v", got, err)
	}
	missing := filepath.Join(directory, "missing")
	if _, err := normalizeVerifyTempDir(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent lost filesystem error identity: %v", err)
	}
	if _, err := normalizeVerifyTempDir(sentinel); err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Fatalf("file accepted as temporary parent: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
		t.Fatalf("temporary parent validation changed user data: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "existing user data" {
		t.Fatalf("temporary parent validation changed sentinel: %v", err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("validation created missing parent: %v", err)
	}
}
