package romcompressor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestPublicExternalVerifyUsesSelectedScratchParent(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "selected scratch")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	source, tool, marker := filepath.Join(dir, "game.rvz"), filepath.Join(dir, "dolphin"), filepath.Join(dir, "observed")
	original := testfixture.RVZHeader(2048)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
test "$TMPDIR" = "$TMP" && test "$TMP" = "$TEMP"
printf '%s' "$TMPDIR" > "$ROMCOMPRESSOR_TEST_PUBLIC_SCRATCH"
printf 'generated scratch' > "$TMPDIR/data"
printf '0123456789012345678901234567890123456789\n'
`
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROMCOMPRESSOR_TEST_PUBLIC_SCRATCH", marker)
	if err := Verify(context.Background(), source, Options{TempDir: parent, Tools: map[string]string{"dolphin-tool": tool}}); err != nil {
		t.Fatal(err)
	}
	observed, err := os.ReadFile(marker)
	if err != nil || filepath.Dir(string(observed)) != parent {
		t.Fatalf("public Verify scratch = %q, %v", observed, err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("verification left scratch: %v, %v", entries, err)
	}
	if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("verification changed source: %v", err)
	}
}
