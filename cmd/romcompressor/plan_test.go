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
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestCLIPlanReportsInputSpecificChoicesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.nds")
	original := testfixture.NDS(false)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	missingTool := filepath.Join(dir, "missing-7z")
	if code := run(context.Background(), []string{"plan", source, "--tool", "7z=" + missingTool, "--json"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("plan: status=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var plan romcompressor.ConversionPlan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil || plan.Input.Format != romcompressor.Raw || plan.Directory || plan.Playlist {
		t.Fatalf("plan result: %+v, %v", plan, err)
	}
	found := make(map[romcompressor.Format]bool)
	for _, target := range plan.Targets {
		found[target.Format] = true
		switch target.Format {
		case romcompressor.CSO, romcompressor.ZSO, romcompressor.CHD, romcompressor.RVZ, romcompressor.CCI, romcompressor.WUX:
			t.Fatalf("cartridge offered incompatible disc route: %+v", target)
		case romcompressor.SevenZip:
			if target.Available || len(target.Dependencies) != 1 || target.Dependencies[0].Engine != "7z" {
				t.Fatalf("missing dependency hidden or invalid route: %+v", target)
			}
		}
	}
	for _, format := range []romcompressor.Format{romcompressor.ZIP, romcompressor.GZIP, romcompressor.SevenZip} {
		if !found[format] {
			t.Fatalf("missing file archive choice %s: %+v", format, plan)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("planning created output or scratch: %v, %v", entries, err)
	}
	if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("planning changed original: %v", err)
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"plan", source}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "DEPENDENCIES") || !strings.Contains(stdout.String(), "Preflight") {
		t.Fatalf("human plan: %d %s %s", code, &stdout, &stderr)
	}
}

func TestCLIPlanMarksUnlistedSevenZipSelection(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.7z")
	if err := os.WriteFile(source, []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"plan", source, "--tool", "7z=" + filepath.Join(dir, "missing"), "--json"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("7z plan requires an engine to inspect: %d %s %s", code, &stdout, &stderr)
	}
	var plan romcompressor.ConversionPlan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	for _, target := range plan.Targets {
		if target.Format == romcompressor.Raw {
			if !target.NeedsArchiveListing || target.Available {
				t.Fatalf("unlisted archive needs selection/dependency information: %+v", target)
			}
			return
		}
	}
	t.Fatal("archive restoration choice missing")
}
