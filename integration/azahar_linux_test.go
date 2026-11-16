package integration_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestThreeDSChecksExtractedSignature(t *testing.T) {
	for _, test := range []struct {
		format romcompressor.Format
		magic  string
		offset int
	}{
		{romcompressor.Z3DSX, "3DSX", 0},
		{romcompressor.ZCCI, "NCSD", 0x100},
		{romcompressor.ZCXI, "NCCH", 0x100},
	} {
		t.Run(string(test.format), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game."+string(test.format))
			payload := filepath.Join(dir, "payload")
			tool := filepath.Join(dir, "azahar")
			ext := kind.ThreeDSRawExtension(string(test.format))
			script := "#!/bin/sh\ncp \"$ROMCOMPRESSOR_TEST_3DS_PAYLOAD\" \"$4/game$ROMCOMPRESSOR_TEST_3DS_EXTENSION\"\n"
			if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ROMCOMPRESSOR_TEST_3DS_PAYLOAD", payload)
			t.Setenv("ROMCOMPRESSOR_TEST_3DS_EXTENSION", ext)
			opts := romcompressor.Options{Format: romcompressor.Raw, Tools: map[string]string{"azahar": tool}}
			for _, valid := range []bool{false, true} {
				plain := bytes.Repeat([]byte{0x5a}, 1024)
				if valid {
					copy(plain[test.offset:], test.magic)
				}
				// A 1 KiB Zstandard raw block without a frame content size.
				frame := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0, 1, 0x20, 0}, plain...)
				if err := os.WriteFile(source, testfixture.Z3DSWithFrame(test.magic, frame, nil), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(payload, plain, 0600); err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				output := filepath.Join(dir, "restored"+ext)
				verifyErr := romcompressor.Verify(ctx, source, opts)
				_, convertErr := romcompressor.Convert(ctx, source, output, opts)
				if !valid {
					for _, err := range []error{verifyErr, convertErr} {
						if err == nil || !strings.Contains(err.Error(), test.magic+" signature") {
							t.Fatalf("expected signature rejection, got %v", err)
						}
					}
					if _, err := os.Lstat(output); !os.IsNotExist(err) {
						t.Fatalf("unexpected output publication: %v", err)
					}
					continue
				}
				if verifyErr != nil || convertErr != nil {
					t.Fatalf("valid signature rejected: verify = %v, convert = %v", verifyErr, convertErr)
				}
				if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, plain) {
					t.Fatalf("restored bytes differ: %v", err)
				}
			}
		})
	}
}
