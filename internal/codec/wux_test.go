package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

var wuxTestSectors = [3][wuxSectorBytes]byte{
	{},
	{0x3b, 0x9e, 'W', 'U', 'D'},
	{0xc6, 0x32, 'W', 'U', 'D'},
}

func wuxTestSector(index int) []byte {
	switch index {
	case 0, 17, wuxSectors - 1:
		return wuxTestSectors[1][:]
	case 1, 18:
		return wuxTestSectors[2][:]
	default:
		return wuxTestSectors[0][:]
	}
}

type generatedWUD struct{}

func (generatedWUD) ReadAt(dst []byte, offset int64) (int, error) {
	if offset%wuxSectorBytes != 0 || len(dst) != wuxSectorBytes || offset < 0 || offset >= WUDImageBytes {
		return 0, fmt.Errorf("unexpected generated WUD read at %d, %d bytes", offset, len(dst))
	}
	return copy(dst, wuxTestSector(int(offset/wuxSectorBytes))), nil
}

type checkRestoredWUD struct{ sectors int }

func (w *checkRestoredWUD) Write(p []byte) (int, error) {
	if !bytes.Equal(p, wuxTestSector(w.sectors)) {
		return 0, fmt.Errorf("WUX restored incorrect bytes in sector %d", w.sectors)
	}
	w.sectors++
	return len(p), nil
}

func TestNativeWUXFullDiscRoundTrip(t *testing.T) {
	ctx := context.Background()
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "disc.wux"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := Encode(ctx, "wux", generatedWUD{}, WUDImageBytes, file, Options{}); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != wuxDataOffset+3*wuxSectorBytes {
		t.Fatalf("WUX stored repeated sectors: output size %d", info.Size())
	}
	mapBytes, err := readWUXMap(ctx, file, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	for sector, physical := range map[int]uint32{0: 0, 1: 1, 2: 2, 17: 0, 18: 1, wuxSectors - 1: 0} {
		if actual := binary.LittleEndian.Uint32(mapBytes[sector*4:]); actual != physical {
			t.Fatalf("sector %d points to %d, want %d", sector, actual, physical)
		}
	}
	restored := &checkRestoredWUD{}
	if err := Decode(ctx, "wux", file, info.Size(), restored, Options{}); err != nil {
		t.Fatal(err)
	}
	if restored.sectors != wuxSectors {
		t.Fatalf("WUX restored %d sectors, want %d", restored.sectors, wuxSectors)
	}
}

func TestNativeWUXMalformedMapAndLimit(t *testing.T) {
	golden := minimalWUXFixture()
	if err := ValidateWUX(context.Background(), bytes.NewReader(golden), int64(len(golden))); err != nil {
		t.Fatal(err)
	}
	if err := Decode(context.Background(), "wux", bytes.NewReader(golden), int64(len(golden)), io.Discard,
		Options{MaxOutput: WUDImageBytes - 1}); err == nil {
		t.Fatal("WUX decode accepted data larger than its expansion limit")
	}
	golden[32+4*(wuxSectors-1)] = 1
	if err := ValidateWUX(context.Background(), bytes.NewReader(golden), int64(len(golden))); err == nil {
		t.Fatal("invalid final WUX table entry was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateWUX(ctx, bytes.NewReader(golden), int64(len(golden))); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation result: %v", err)
	}
}

func TestNativeWUXChecksFlagsAndStoredSectorCoverage(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
		valid  bool
	}{
		{"alignment padding", func(data []byte) []byte {
			data[12], data[28], data[wuxDataOffset-1] = 0xfe, 0xfd, 0xfc
			return data
		}, true},
		{"unsupported flags", func(data []byte) []byte {
			data[24] = 1
			return data
		}, false},
		{"unreferenced stored sector", func(data []byte) []byte {
			return append(data, bytes.Repeat([]byte{0xa5}, wuxSectorBytes)...)
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := test.mutate(minimalWUXFixture())
			if err := ValidateWUX(context.Background(), bytes.NewReader(data), int64(len(data))); (err == nil) != test.valid {
				t.Fatalf("WUX validation returned %v, want valid=%t", err, test.valid)
			}
		})
	}
}

func TestNativeWUXStopsReadingMetadataAfterCancellation(t *testing.T) {
	data := minimalWUXFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancellingReaderAt{ReaderAt: bytes.NewReader(data), cancel: cancel, cancelAfter: 1}
	if err := ValidateWUX(ctx, reader, int64(len(data))); !errors.Is(err, context.Canceled) || reader.reads != 1 {
		t.Fatalf("expected WUX cancellation after first read, got %v after %d reads", err, reader.reads)
	}
}

func TestNativeWUXHonorsCancellationBeforeFinishing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	file, err := os.CreateTemp(t.TempDir(), "cancelled-*.wux")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = Encode(ctx, "wux", generatedWUD{}, WUDImageBytes, file, Options{Progress: func(done, total int64) {
		if done == total {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation before finishing WUX, got %v", err)
	}
}

func minimalWUXFixture() []byte {
	// Independent one-physical-sector layout matching Cemu's WUX reader.
	data := make([]byte, 0x2f0000+0x8000)
	copy(data[:8], []byte{'W', 'U', 'X', '0', 0x2e, 0xd0, 0x99, 0x10})
	binary.LittleEndian.PutUint32(data[8:], 32768)
	binary.LittleEndian.PutUint64(data[16:], 25025314816)
	return data
}
