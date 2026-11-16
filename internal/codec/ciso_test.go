package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCISORoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"GameCube", gameCubeDiscSize},
		{"Wii single layer", wiiSingleLayerSize},
		{"Wii dual layer with no second-layer data", wiiDualLayerSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Create(filepath.Join(t.TempDir(), "game.ciso"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			source := generatedNintendoISO{tc.size}
			if err := Encode(ctx, "ciso", source, tc.size, f, Options{}); err != nil {
				t.Fatal(err)
			}
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != cisoHeaderSize+3*packedDiscBlockSize {
				t.Fatalf("got CISO size %d, wanted the header, two data blocks, and the final zero block", st.Size())
			}
			disc, err := parseCISO(ctx, f, st.Size())
			if err != nil {
				t.Fatal(err)
			}
			if disc.size != tc.size {
				t.Fatalf("restored %d logical bytes, want %d", disc.size, tc.size)
			}
			for _, offset := range []int64{0, packedDiscBlockSize - 20, wbfsFixtureMarkerOffset, tc.size - 64} {
				var got, want [64]byte
				if _, err := disc.ReadAt(got[:], offset); err != nil {
					t.Fatal(err)
				}
				if _, err := source.ReadAt(want[:], offset); err != nil || got != want {
					t.Fatalf("restored data differs at 0x%x", offset)
				}
			}
			if err := Decode(ctx, "ciso", f, st.Size(), io.Discard, Options{MaxOutput: tc.size - 1}); err == nil {
				t.Fatal("accepted CISO larger than the expanded byte limit")
			}
			if tc.size == gameCubeDiscSize {
				if err := Verify(ctx, "ciso", f, st.Size(), Options{}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCISONondefaultBlockMap(t *testing.T) {
	// An independently laid out Compact ISO with a 512 KiB block size and a
	// single skipped logical block. Blocks are packed in presence-map order.
	const blockSize = 512 << 10
	volume := make([]byte, 32768+blockSize*2)
	copy(volume, "CISO")
	binary.LittleEndian.PutUint32(volume[4:], blockSize)
	volume[8], volume[10] = 1, 1
	binary.BigEndian.PutUint32(volume[32768+0x1c:], 0xc2339f3d)
	volume[32768+blockSize] = 0xab
	disc, err := parseCISO(context.Background(), bytes.NewReader(volume), int64(len(volume)))
	if err != nil {
		t.Fatal(err)
	}
	var crossing [2]byte
	if n, err := disc.ReadAt(crossing[:], 2*blockSize-1); n != 2 || err != nil || crossing != [2]byte{0, 0xab} {
		t.Fatalf("unexpected CISO gap or stored chunk: %x %d %v", crossing, n, err)
	}
}

func TestCISORejectsInvalidMapAndUnsupportedExtent(t *testing.T) {
	const blockSize = 512 << 10
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"short header", func(p []byte) []byte { return p[:5] }},
		{"PSP CSO header size", func(p []byte) []byte { binary.LittleEndian.PutUint32(p[4:], 24); return p }},
		{"misaligned block size", func(p []byte) []byte { p[4] = 1; return p }},
		{"invalid present flag", func(p []byte) []byte { p[10] = 2; return p }},
		{"data before first map entry", func(p []byte) []byte { p[8] = 0; return p }},
		{"incorrect packed length", func(p []byte) []byte { return p[:len(p)-1] }},
		{"no GameCube or Wii signature", func(p []byte) []byte { p[32768+0x1c] = 0; return p }},
		{"no nominal map capacity", func(p []byte) []byte {
			binary.LittleEndian.PutUint32(p[4:], 32768)
			return p[:32768+2*32768]
		}},
		{"blocks beyond nominal ISO", func(p []byte) []byte {
			p[10] = 0
			p[8+gameCubeDiscSize/blockSize+1] = 1
			return p
		}},
		{"nonzero bytes beyond nominal ISO", func(p []byte) []byte {
			p[10] = 0
			p[8+gameCubeDiscSize/blockSize] = 1
			p[32768+blockSize+gameCubeDiscSize%blockSize] = 1
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume := make([]byte, 32768+blockSize*2)
			copy(volume, "CISO")
			binary.LittleEndian.PutUint32(volume[4:], blockSize)
			volume[8], volume[10] = 1, 1
			binary.BigEndian.PutUint32(volume[32768+0x1c:], 0xc2339f3d)
			volume = tc.mutate(volume)
			if err := ValidateCISO(context.Background(), bytes.NewReader(volume), int64(len(volume))); err == nil {
				t.Fatal("accepted malformed or out-of-range Nintendo CISO")
			}
		})
	}
}

func TestCISOStopsReadingMetadataAfterCancellation(t *testing.T) {
	const blockSize = 512 << 10
	volume := make([]byte, cisoHeaderSize+blockSize)
	copy(volume, "CISO")
	binary.LittleEndian.PutUint32(volume[4:], blockSize)
	volume[cisoMapOffset] = 1
	binary.BigEndian.PutUint32(volume[cisoHeaderSize+gameCubeMagicOffset:], gameCubeMagic)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancellingReaderAt{ReaderAt: bytes.NewReader(volume), cancel: cancel, cancelAfter: 1}
	if err := ValidateCISO(ctx, reader, int64(len(volume))); !errors.Is(err, context.Canceled) || reader.reads != 1 {
		t.Fatalf("expected CISO cancellation after first read, got %v after %d reads", err, reader.reads)
	}
}
