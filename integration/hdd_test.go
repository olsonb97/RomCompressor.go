package integration_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
)

func TestHDDCHDRoundTripIntegration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
	if tool == "" {
		t.Skip("set ROMCOMPRESSOR_CHDMAN to check hard-drive CHD interoperability")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "disk.img")
	archive := filepath.Join(dir, "disk.chd")
	restored := filepath.Join(dir, "restored.img")
	// A prime sector count can't use common CHS defaults. The adapter must
	// retain the real size instead of allowing chdman to round the geometry up.
	raw := make([]byte, 67*512)
	for i := range raw {
		raw[i] = byte((i*19 + i/179) % 251)
	}
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opts := romcompressor.Options{Format: romcompressor.CHD, Media: "hdd", Tools: map[string]string{"chdman": tool}}
	compressed, err := romcompressor.Convert(ctx, source, archive, opts)
	if err != nil || !compressed.Verified || compressed.InputBytes != int64(len(raw)) {
		t.Fatalf("compress hard drive = %+v, %v", compressed, err)
	}
	if info, err := romcompressor.Inspect(ctx, archive); err != nil || info.Format != romcompressor.CHD || info.Media != "hdd" {
		t.Fatalf("inspect generated hard-drive CHD = %+v, %v", info, err)
	}
	if err := romcompressor.Verify(ctx, archive, opts); err != nil {
		t.Fatal(err)
	}
	opts.Format = romcompressor.Raw
	result, err := romcompressor.Convert(ctx, archive, restored, opts)
	if err != nil || !result.Verified || result.OutputBytes != int64(len(raw)) {
		t.Fatalf("restore hard drive = %+v, %v", result, err)
	}
	for _, name := range []string{source, restored} {
		actual, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(actual, raw) {
			t.Fatalf("incorrect raw file %s: %v", name, err)
		}
	}
	opts.MaxOutput = int64(len(raw) - 1)
	blocked := filepath.Join(dir, "too-large.img")
	if _, err := romcompressor.Convert(ctx, archive, blocked, opts); err == nil {
		t.Fatal("restored a hard drive past its size limit")
	}
	if _, err := os.Stat(blocked); !os.IsNotExist(err) {
		t.Fatalf("published an oversized raw disk: %v", err)
	}
}
