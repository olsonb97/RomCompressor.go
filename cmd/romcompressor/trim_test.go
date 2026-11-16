package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestCLITrimDryRunAndNewOutput(t *testing.T) {
	download := testfixture.NDS(false)
	downloadEnd := testfixture.NDSTrimmedSize + 0x88
	copy(download[testfixture.NDSTrimmedSize:], "ac")
	download[downloadEnd-1] = 0x56
	for _, tt := range []struct {
		name, ext, format string
		original          []byte
		keep              int
	}{
		{"3DS", ".3ds", "3ds", testfixture.CARD1(), testfixture.CARD1TrimmedSize},
		{"3DS CCI", ".cci", "3ds", testfixture.CARD1(), testfixture.CARD1TrimmedSize},
		{"DS with Download Play", ".nds", "nds", download, downloadEnd},
		{"DSi-enhanced", ".NDS", "nds", testfixture.NDS(true), testfixture.DSiTrimmedSize},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkCLITrim(t, tt.ext, tt.format, tt.original, tt.keep)
		})
	}
}

func checkCLITrim(t *testing.T, ext, format string, original []byte, keep int) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "sample"+ext)
	output := filepath.Join(dir, "sample.trim"+ext)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	args := []string{"trim", source, "--dry-run", "--json"}
	if code := run(ctx, args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("trim dry run: status %d, stdout %s, stderr %s", code, &stdout, &stderr)
	}
	var plan romcompressor.TrimPlan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil || plan.Format != format || plan.OutputBytes != int64(keep) {
		t.Fatalf("dry run returned %+v: %v", plan, err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("dry run created output: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	args = []string{"trim", source, "--json"}
	if code := run(ctx, args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("trim: status %d, stdout %s, stderr %s", code, &stdout, &stderr)
	}
	var result romcompressor.TrimResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Output != output || result.TrimPlan != plan {
		t.Fatalf("trim returned %+v: %v", result, err)
	}
	if clipped, err := os.ReadFile(output); err != nil || !bytes.Equal(clipped, original[:keep]) {
		t.Fatalf("trimmed bytes differ: %v", err)
	}
	if code := run(ctx, args, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
		t.Fatal("trim overwrote an existing output")
	}
	if unchanged, err := os.ReadFile(source); err != nil || !bytes.Equal(unchanged, original) {
		t.Fatalf("original bytes differ: %v", err)
	}
}

func TestCLITrimOptionsAreExplicit(t *testing.T) {
	for _, args := range [][]string{
		{"compress", "game.3ds", "-f", "trim"},
		{"compress", "game.3ds", "-f", "zip", "--dry-run"},
		{"trim", "game.3ds", "-f", "zip"},
		{"trim", "game.3ds", "-n", "-o", "new.3ds"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("accepted mixed trim/compress arguments %v", args)
		}
	}
}
