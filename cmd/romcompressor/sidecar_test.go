package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSBICollisionExplainsDestinationAndRecovery(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.bin")
	output := filepath.Join(dir, "restored.ecm")
	conflict := filepath.Join(dir, "restored.sbi")
	original := []byte("generated disc bytes")
	existing := []byte("SBI\x00existing")
	for path, data := range map[string][]byte{source: original, conflict: existing} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"compress", source, "-f", "ecm", "-o", output, "--json"}, &stdout, &stderr)
	var failure map[string]string
	if code != 1 || stderr.Len() != 0 || json.Unmarshal(stdout.Bytes(), &failure) != nil {
		t.Fatalf("collision: %d %s %s", code, &stdout, &stderr)
	}
	if !strings.Contains(failure["error"], conflict) || !strings.Contains(failure["error"], "--output") {
		t.Fatalf("collision did not identify the companion and recovery: %s", &stdout)
	}
	for path, want := range map[string][]byte{source: original, conflict: existing} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("collision changed %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("collision left output or staging files: %v, %v", entries, err)
	}
}
