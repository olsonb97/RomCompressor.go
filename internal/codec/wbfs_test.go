package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type generatedNintendoISO struct {
	size int64
}

const wbfsFixtureMarker = "Preserved nonzero bytes outside the disc header"
const wbfsFixtureMarkerOffset = 4*wbfsBlockSize + 7

func (s generatedNintendoISO) ReadAt(p []byte, offset int64) (int, error) {
	if offset >= s.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), s.size-offset)
	clear(p[:n])
	put := func(data []byte, start int64) {
		first := max(start, offset)
		last := min(start+int64(len(data)), offset+n)
		if first < last {
			copy(p[first-offset:last-offset], data[first-start:last-start])
		}
	}
	if s.size == gameCubeDiscSize {
		put([]byte("GMC0E0"), 0)
		put([]byte{0xc2, 0x33, 0x9f, 0x3d}, 0x1c)
	} else {
		put([]byte("RMC0E0"), 0)
		put([]byte{0x5d, 0x1c, 0x9e, 0xa3}, 0x18)
	}
	put([]byte("RomCompressor synthetic disc"), 0x20)
	put([]byte(wbfsFixtureMarker), wbfsFixtureMarkerOffset)
	if n != int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func encodeGeneratedWBFS(t *testing.T, path string, size int64) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := Encode(context.Background(), "wbfs", generatedNintendoISO{size}, size, f, Options{}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWBFSRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"single layer", wiiSingleLayerSize},
		{"dual layer with no second-layer data", wiiDualLayerSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := encodeGeneratedWBFS(t, filepath.Join(t.TempDir(), "game.wbfs"), tc.size)
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != 4*wbfsBlockSize {
				t.Fatalf("got WBFS size %d, wanted the metadata, two data blocks, and a zero end block", st.Size())
			}
			disc, err := parseWBFS(context.Background(), f, st.Size())
			if err != nil {
				t.Fatal(err)
			}
			if disc.size != tc.size {
				t.Fatalf("restored %d logical bytes, want %d", disc.size, tc.size)
			}
			for _, offset := range []int64{0, wbfsBlockSize - 20, wbfsFixtureMarkerOffset, tc.size - 64} {
				var got, want [64]byte
				if _, err := disc.ReadAt(got[:], offset); err != nil {
					t.Fatal(err)
				}
				if _, err := (generatedNintendoISO{tc.size}).ReadAt(want[:], offset); err != nil || got != want {
					t.Fatalf("restored data differs at 0x%x", offset)
				}
			}
			if err := Decode(context.Background(), "wbfs", f, st.Size(), io.Discard, Options{MaxOutput: tc.size - 1}); err == nil {
				t.Fatal("accepted a WBFS with more logical data than the output limit")
			}
		})
	}
}

func TestWBFSRejectsMalformedVolumes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"split size", func(p []byte) { p[6]++ }},
		{"invalid shift", func(p []byte) { p[9] = 255 }},
		{"unknown version", func(p []byte) { p[10] = 2 }},
		{"multiple discs", func(p []byte) { p[13] = 1 }},
		{"nonzero first slot missing", func(p []byte) { p[12] = 0 }},
		{"duplicate physical block", func(p []byte) { p[771] = 1 }},
		{"missing first block", func(p []byte) { p[769] = 0 }},
		{"bad block offset", func(p []byte) { p[769] = 2 }},
		{"corrupt disc slot", func(p []byte) { p[0x200+0x18] = 0 }},
		{"corrupt disc data", func(p []byte) { p[wbfsBlockSize+0x18] = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume := minimalWBFSFixture()
			if err := ValidateWBFS(context.Background(), bytes.NewReader(volume), int64(len(volume))); err != nil {
				t.Fatal(err)
			}
			tc.mutate(volume)
			if err := ValidateWBFS(context.Background(), bytes.NewReader(volume), int64(len(volume))); err == nil {
				t.Fatal("accepted invalid WBFS metadata")
			}
		})
	}
}

func TestWBFSStopsReadingMetadataAfterCancellation(t *testing.T) {
	volume := minimalWBFSFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancellingReaderAt{ReaderAt: bytes.NewReader(volume), cancel: cancel, cancelAfter: 1}
	if err := ValidateWBFS(ctx, reader, int64(len(volume))); !errors.Is(err, context.Canceled) || reader.reads != 1 {
		t.Fatalf("expected WBFS cancellation after first read, got %v after %d reads", err, reader.reads)
	}
}

// A minimal independent layout with the Wii header mirrored in the catalog
// and one allocated block, followed by unmapped disc sectors.
func minimalWBFSFixture() []byte {
	volume := make([]byte, 2*wbfsBlockSize)
	copy(volume, "WBFS")
	binary.BigEndian.PutUint32(volume[4:], uint32(len(volume)/512))
	volume[8], volume[9], volume[12] = 9, 21, 1
	binary.BigEndian.PutUint32(volume[0x200+0x18:], 0x5d1c9ea3)
	volume[0x301] = 1
	binary.BigEndian.PutUint32(volume[wbfsBlockSize+0x18:], 0x5d1c9ea3)
	return volume
}

// Set ROMCOMPRESSOR_WIT to Wiimms ISO Tool to independently read the Go writer's
// WBFS. The tool may write a full-size temporary ISO.
func TestWBFSWiimmsInteroperability(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_WIT")
	if tool == "" {
		t.Skip("ROMCOMPRESSOR_WIT is not set")
	}
	tool, err := exec.LookPath(tool)
	if err != nil {
		t.Fatal(err)
	}
	tool, err = filepath.Abs(tool)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wbfsFile := encodeGeneratedWBFS(t, filepath.Join(dir, "game.wbfs"), wiiSingleLayerSize)
	if err := wbfsFile.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, "ID6", "-T/", "game.wbfs")
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "WIT_OPT=")
	if result, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(result)) != "RMC0E0" {
		t.Fatalf("Wiimms could not identify the generated disc: %q %v", result, err)
	}
	cmd = exec.CommandContext(ctx, tool, "COPY", "--raw", "--no-split", "--prealloc=off", "-T/", "--iso", "game.wbfs", "restored.iso")
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "WIT_OPT=")
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Wiimms could not restore the generated disc: %q %v", result, err)
	}
	restored, err := os.Open(filepath.Join(dir, "restored.iso"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	generated := generatedNintendoISO{wiiSingleLayerSize}
	got, want := make([]byte, 1<<20), make([]byte, 1<<20)
	for offset := int64(0); offset < generated.size; offset += int64(len(got)) {
		length := min(generated.size-offset, int64(len(got)))
		_, _ = generated.ReadAt(want[:length], offset)
		if _, err := io.ReadFull(restored, got[:length]); err != nil || !bytes.Equal(got[:length], want[:length]) {
			t.Fatalf("Wiimms restored data differs at 0x%x: %v", offset, err)
		}
	}
	// WIT copies complete physical blocks. If the final block straddles the
	// nominal ISO length, only that block's zero padding can follow the disc.
	padding := (wbfsBlockSize - generated.size%wbfsBlockSize) % wbfsBlockSize
	extra, err := io.ReadAll(io.LimitReader(restored, padding+1))
	if err != nil || int64(len(extra)) > padding || !allDiscZeros(extra) {
		t.Fatalf("invalid WIT ISO padding (%d bytes): %v", len(extra), err)
	}
	t.Logf("WIT restored all %d ISO bytes and %d bytes of zero block padding", generated.size, len(extra))
}
