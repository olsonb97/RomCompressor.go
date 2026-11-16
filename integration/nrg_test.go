package integration_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/chd"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestCHDNRGPreservesStoredSectors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pregap uint64
	}{
		{"no pregap", 0},
		{"stored pregap", 2 * 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			source := filepath.Join(dir, "disc.nrg")
			sectors := make([]byte, 8*2048)
			for i := range sectors {
				sectors[i] = byte(1 + i/2048)
			}
			nrg := testfixture.NRG(sectors, 0, tc.pregap)
			if err := os.WriteFile(source, nrg, 0600); err != nil {
				t.Fatal(err)
			}
			check := func(err error) {
				t.Helper()
				if tc.pregap == 0 && err != nil {
					t.Fatal(err)
				}
				if tc.pregap != 0 && (err == nil || !strings.Contains(err.Error(), "discard the stored pregap")) {
					t.Fatalf("expected NRG stored pregap refusal, got %v", err)
				}
			}
			check(chd.ValidateNRG(ctx, source, kind.DefaultMaxOutput))
			tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
			if tool == "" {
				return
			}
			if tc.pregap == 0 {
				sectors = testfixture.CHDTrack(sectors, 2048)
				nrg = testfixture.NRG(sectors, 0, tc.pregap)
				if err := os.WriteFile(source, nrg, 0600); err != nil {
					t.Fatal(err)
				}
			}
			chd := filepath.Join(dir, "disc.chd")
			opts := romcompressor.Options{Format: romcompressor.CHD, Media: "cd", Tools: map[string]string{"chdman": tool}}
			_, err := romcompressor.Convert(ctx, source, chd, opts)
			check(err)
			if original, err := os.ReadFile(source); err != nil || !bytes.Equal(original, nrg) {
				t.Fatalf("original image changed: %v", err)
			}
			if tc.pregap != 0 {
				if _, err := os.Lstat(chd); !os.IsNotExist(err) {
					t.Fatalf("refused conversion published output: %v", err)
				}
				return
			}
			opts.Format = romcompressor.Raw
			if _, err := romcompressor.Convert(ctx, chd, filepath.Join(dir, "restored.cue"), opts); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(filepath.Join(dir, "restored.bin"))
			if err != nil || !bytes.Equal(restored, sectors) {
				t.Fatalf("restored sectors differ: %v", err)
			}
		})
	}
}
