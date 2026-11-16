package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/chd"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

func TestTinyCHDConversionPreservesInputsIntegration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
	if tool == "" {
		t.Skip("ROMCOMPRESSOR_CHDMAN is not set")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "disc.cue")
	track := bytes.Repeat([]byte{1}, 2048)
	inputs := map[string][]byte{
		"disc.cue":  []byte("FILE track.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n"),
		"track.bin": track,
	}
	for name, data := range inputs {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		t.Cleanup(func() {
			actual, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(actual) != digest {
				t.Errorf("original %s changed: %v", name, err)
			}
		})
	}
	if err := chd.ValidateCUE(source, "", kind.DefaultMaxOutput); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	compressed := filepath.Join(dir, "tiny.chd")
	opts := romcompressor.Options{Format: romcompressor.CHD, Tools: map[string]string{"chdman": tool}}
	result, err := romcompressor.Convert(ctx, source, compressed, opts)
	if err != nil {
		// Some upstream versions create an unreadable compressed map for tiny
		// CDs. Such an engine failure must never publish an unchecked output.
		if !strings.Contains(err.Error(), "engine failed") {
			t.Fatalf("unexpected tiny CD failure: %v", err)
		}
		if result.Verified {
			t.Error("failed conversion reported verification success")
		}
		if _, err := os.Lstat(compressed); !os.IsNotExist(err) {
			t.Fatalf("failed conversion published a CHD: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 2 || entries[0].Name() != "disc.cue" || entries[1].Name() != "track.bin" {
			t.Fatalf("failed conversion left files beyond its original inputs: %v, %v", entries, err)
		}
		return
	}
	if !result.Verified {
		t.Fatal("successful tiny CD conversion was not verified")
	}
	if err := romcompressor.Verify(ctx, compressed, opts); err != nil {
		t.Fatal(err)
	}
	opts.Format, opts.Media = romcompressor.Raw, "cd"
	if _, err := romcompressor.Convert(ctx, compressed, filepath.Join(dir, "restored.cue"), opts); err != nil {
		t.Fatal(err)
	}
	if restored, err := os.ReadFile(filepath.Join(dir, "restored.bin")); err != nil || !bytes.Equal(restored, track) {
		t.Fatalf("tiny CD round trip changed data: %v", err)
	}
}

func TestWUXRoundTripIntegration(t *testing.T) {
	jar := os.Getenv("ROMCOMPRESSOR_JWUDTOOL")
	if jar == "" {
		t.Skip("set ROMCOMPRESSOR_JWUDTOOL to an installed JWUDTool JAR; this test writes a full-size WUD")
	}
	const discSize = 0x5d3a00000
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.wud")
	file, err := os.OpenFile(source, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if err := file.Truncate(discSize); err != nil {
		t.Fatal(err)
	}
	pattern := bytes.Repeat([]byte("generated WUD sector\x00"), 100)
	for _, offset := range []int64{0, 0x8000, discSize - 0x8000} {
		if _, err := file.WriteAt(pattern, offset); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := filepath.Join(dir, "game.wux")
	restored := filepath.Join(dir, "restored.wud")
	opts := romcompressor.Options{Format: romcompressor.WUX, Tools: map[string]string{"jwudtool": jar}}
	result, err := romcompressor.Convert(ctx, source, compressed, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.OutputBytes >= discSize {
		t.Fatalf("expected verified compression of the generated WUD: %+v", result)
	}
	opts.Format = romcompressor.Raw
	if _, err := romcompressor.Convert(ctx, compressed, restored, opts); err != nil {
		t.Fatal(err)
	}
	if err := fileops.CompareFiles(ctx, source, restored); err != nil {
		t.Fatal(err)
	}
}

func TestNSZRoundTripIntegration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_NSZ")
	keys := os.Getenv("ROMCOMPRESSOR_NSZ_KEYS")
	if tool == "" || keys == "" {
		t.Skip("set ROMCOMPRESSOR_NSZ and ROMCOMPRESSOR_NSZ_KEYS to opt in")
	}
	for _, tc := range []struct {
		format romcompressor.Format
		env    string
		ext    string
	}{
		{romcompressor.NSZ, "ROMCOMPRESSOR_NSZ_NSP", ".nsp"},
		{romcompressor.XCZ, "ROMCOMPRESSOR_NSZ_XCI", ".xci"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			source := os.Getenv(tc.env)
			if source == "" {
				t.Skipf("set %s to an explicitly selected fixture", tc.env)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			dir := t.TempDir()
			compressed := filepath.Join(dir, "game."+string(tc.format))
			restored := filepath.Join(dir, "restored"+tc.ext)
			opts := romcompressor.Options{Format: tc.format, Tools: map[string]string{"nsz": tool}, Keys: keys}
			if _, err := romcompressor.Convert(ctx, source, compressed, opts); err != nil {
				t.Fatal(err)
			}
			if err := romcompressor.Verify(ctx, compressed, opts); err != nil {
				t.Fatal(err)
			}
			opts.Format = romcompressor.Raw
			if _, err := romcompressor.Convert(ctx, compressed, restored, opts); err != nil {
				t.Fatal(err)
			}
			if err := fileops.CompareFiles(ctx, source, restored); err != nil {
				t.Fatal(err)
			}
		})
	}
}
