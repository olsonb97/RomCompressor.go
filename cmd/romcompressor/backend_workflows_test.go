package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
)

func TestVerificationScratchFlagScope(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "chosen scratch")
	args, err := parse([]string{"verify", "game.rvz", "--temp-dir", parent, "--json"})
	if err != nil || options(args).TempDir != parent {
		t.Fatalf("verification scratch option = %+v, %v", args, err)
	}
	for _, argv := range [][]string{
		{"compress", "game.iso", "-f", "cso", "--temp-dir", parent},
		{"decompress", "game.cso", "--temp-dir", parent},
		{"plan", "game.iso", "--temp-dir", parent},
		{"batch", "jobs.json", "--temp-dir", parent},
	} {
		if _, err := parse(argv); err == nil {
			t.Fatalf("irrelevant scratch option accepted: %v", argv)
		}
	}
}

func TestCLINativeVerificationValidatesScratchParent(t *testing.T) {
	dir := t.TempDir()
	source, archive := filepath.Join(dir, "game.iso"), filepath.Join(dir, "game.cso")
	original := bytes.Repeat([]byte{0x35}, 8192)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := romcompressor.Convert(context.Background(), source, archive, romcompressor.Options{Format: romcompressor.CSO}); err != nil {
		t.Fatal(err)
	}
	for _, exists := range []bool{false, true} {
		parent := filepath.Join(dir, "scratch")
		if exists {
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
		}
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"verify", archive, "--temp-dir", parent, "--json"}, &stdout, &stderr)
		var result map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || stderr.Len() != 0 {
			t.Fatalf("verification emitted invalid JSON/progress: %q %q, %v", &stdout, &stderr, err)
		}
		if exists && (code != 0 || result["verified"] != true) || !exists && (code == 0 || result["error"] == nil) {
			t.Fatalf("scratch exists=%v: status=%d result=%v", exists, code, result)
		}
	}
	if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("verification changed original: %v", err)
	}
}

func TestCLIRestoresCompletePlaylistWithDefaultName(t *testing.T) {
	dir := t.TempDir()
	payloads := [][]byte{bytes.Repeat([]byte{0x31}, 8192), bytes.Repeat([]byte{0x61}, 4096)}
	var entries []string
	for index, name := range []string{"first", "second"} {
		folder := filepath.Join(dir, name)
		if err := os.Mkdir(folder, 0700); err != nil {
			t.Fatal(err)
		}
		source, archive := filepath.Join(folder, "disc.iso"), filepath.Join(folder, "disc.cso")
		if err := os.WriteFile(source, payloads[index], 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := romcompressor.Convert(context.Background(), source, archive, romcompressor.Options{Format: romcompressor.CSO}); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, name+"/disc.cso")
	}
	input := filepath.Join(dir, "Game.m3u")
	original := []byte(strings.Join(entries, "\n") + "\n")
	if err := os.WriteFile(input, original, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"decompress", input, "--json"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("restore playlist: %d %s %s", code, &stdout, &stderr)
	}
	var result romcompressor.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || !result.Verified || result.Output != filepath.Join(dir, "Game-restored.m3u") {
		t.Fatalf("restore result: %+v, %v", result, err)
	}
	contents, err := os.ReadFile(result.Output)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != len(payloads) {
		t.Fatalf("restored playlist references: %q", contents)
	}
	for index, line := range lines {
		path := filepath.Join(dir, filepath.FromSlash(line))
		if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, payloads[index]) {
			t.Fatalf("restored disc %d: %v", index, err)
		}
	}
	if actual, err := os.ReadFile(input); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("restoration changed source playlist: %v", err)
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"decompress", input, "--json"}, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "different output") {
		t.Fatalf("repeat restore must report collision: %d %s", code, &stdout)
	}
}
