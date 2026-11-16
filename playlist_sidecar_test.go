package romcompressor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPlaylistRawCDCopiesPreserveSBI(t *testing.T) {
	for _, extension := range []string{".cdr", ".toast", ".cdi", ".nrg"} {
		t.Run(extension, func(t *testing.T) {
			dir := t.TempDir()
			image := filepath.Join(dir, "disc"+extension)
			playlist := filepath.Join(dir, "game.m3u")
			sidecar := filepath.Join(dir, "disc.sBi")
			originals := map[string][]byte{
				image:    []byte("generated raw CD image"),
				playlist: []byte("disc" + extension + "\n"),
				sidecar:  []byte("SBI\x00generated companion"),
			}
			for path, data := range originals {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(dir, "restored.m3u")
			if _, err := ConvertPlaylist(context.Background(), playlist, output, Options{Format: Raw}); err != nil {
				t.Fatal(err)
			}
			for name, expected := range map[string][]byte{"disc" + extension: originals[image], "disc.sbi": originals[sidecar]} {
				actual, err := os.ReadFile(filepath.Join(dir, "restored", name))
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("restored %s: %v", name, err)
				}
			}
			assertPlaylistOriginals(t, originals)
		})
	}
}
