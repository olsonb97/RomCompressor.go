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
	"github.com/olsonb97/romcompressor.go/internal/disc"
)

func TestPlaylistRestoresTaggedCDAndDVDIntegration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
	if tool == "" {
		t.Skip("set ROMCOMPRESSOR_CHDMAN to verify complete mixed-media playlist restoration")
	}
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	originals := make(map[string][sha256.Size]byte)
	payloads := make(map[string][]byte)
	for _, media := range []string{"cd", "dvd"} {
		folder := filepath.Join(directory, media)
		if err := os.Mkdir(folder, 0700); err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(folder, "disc.iso")
		data := make([]byte, 129*2048)
		if media == "cd" {
			data = make([]byte, 150*2352)
			input = filepath.Join(folder, "disc.cue")
			cue := []byte("FILE \"disc.bin\" BINARY\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n")
			if err := os.WriteFile(input, cue, 0600); err != nil {
				t.Fatal(err)
			}
			originals[input] = sha256.Sum256(cue)
		}
		for index := range data {
			data[index] = byte((index*17 + index/251) % 256)
		}
		payloads[media] = data
		dataPath := input
		if media == "cd" {
			dataPath = filepath.Join(folder, "disc.bin")
		}
		if err := os.WriteFile(dataPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		originals[dataPath] = sha256.Sum256(data)
		archive := filepath.Join(folder, "disc.chd")
		if _, err := romcompressor.Convert(ctx, input, archive, romcompressor.Options{Format: romcompressor.CHD, Media: media, Tools: map[string]string{"chdman": tool}}); err != nil {
			t.Fatal(err)
		}
		compressed, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		originals[archive] = sha256.Sum256(compressed)
	}
	sbi := []byte("SBI\x00generated playlist companion")
	sidecar := filepath.Join(directory, "cd", "disc.sBi")
	if err := os.WriteFile(sidecar, sbi, 0600); err != nil {
		t.Fatal(err)
	}
	originals[sidecar] = sha256.Sum256(sbi)
	source := filepath.Join(directory, "source.m3u")
	contents := []byte("cd/disc.chd\ndvd/disc.chd\n")
	if err := os.WriteFile(source, contents, 0600); err != nil {
		t.Fatal(err)
	}
	originals[source] = sha256.Sum256(contents)
	output := filepath.Join(directory, "restored.m3u")
	result, err := romcompressor.ConvertPlaylist(ctx, source, output, romcompressor.Options{Format: romcompressor.Raw, Tools: map[string]string{"chdman": tool}})
	if err != nil || !result.Verified {
		t.Fatalf("mixed CHD media restoration = %+v, %v", result, err)
	}
	playlist, err := os.ReadFile(output)
	if err != nil || string(playlist) != "./restored/disc.cue\n./restored/disc-2.iso\n" {
		t.Fatalf("replacement playlist = %q, %v", playlist, err)
	}
	if err := disc.VisitReferences(filepath.Join(directory, "restored", "disc.cue"), nil); err != nil {
		t.Fatalf("restored CD descriptor is unusable: %v", err)
	}
	for media, filename := range map[string]string{"cd": "disc.bin", "dvd": "disc-2.iso"} {
		actual, err := os.ReadFile(filepath.Join(directory, "restored", filename))
		if err != nil || !bytes.Equal(actual, payloads[media]) {
			t.Fatalf("%s restoration changed disc bytes: %v", media, err)
		}
	}
	if actual, err := os.ReadFile(filepath.Join(directory, "restored", "disc.sbi")); err != nil || !bytes.Equal(actual, sbi) {
		t.Fatalf("CD companion missing or changed: %v", err)
	}
	for path, digest := range originals {
		actual, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(actual) != digest {
			t.Fatalf("original changed at %s: %v", strings.TrimPrefix(path, directory), err)
		}
	}
}
