package trim

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

const (
	testMain3DS   = testfixture.CARD1MainOffset
	testUpdate3DS = testfixture.CARD1UpdateOffset
	testEnd3DS    = testfixture.CARD1TrimmedSize
)

func TestN3DSBoundaryPreservesEveryPartition(t *testing.T) {
	data := testfixture.CARD1()
	for _, raw := range [][]byte{data, data[:testEnd3DS]} {
		keep, err := N3DSBoundary(bytes.NewReader(raw), int64(len(raw)))
		if err != nil || keep != testEnd3DS {
			t.Fatalf("N3DSBoundary = %#x, %v; want %#x", keep, err, testEnd3DS)
		}
	}
}

func TestN3DSBoundaryPreservesMasteredExtent(t *testing.T) {
	for _, tt := range []struct {
		name string
		used uint64
		want int64
	}{
		{"partition extends further", testEnd3DS - ncsdMediaUnit, testEnd3DS},
		{"used extent spans an entire sector", testEnd3DS + ncsdMediaUnit, testEnd3DS + ncsdMediaUnit},
		{"used extent spans part of a sector", testEnd3DS + 1, testEnd3DS + ncsdMediaUnit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := testfixture.CARD1()
			binary.LittleEndian.PutUint64(data[ncsdUsedSizeOffset:], tt.used)
			for _, raw := range [][]byte{data, data[:tt.want]} {
				keep, err := N3DSBoundary(bytes.NewReader(raw), int64(len(raw)))
				if err != nil || keep != tt.want {
					t.Fatalf("N3DSBoundary = %#x, %v; want %#x", keep, err, tt.want)
				}
			}
		})
	}
}

func TestN3DSRejectsUnsafeCartridgeLayouts(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func([]byte) []byte
		want string
	}{
		{"partial last sector", func(data []byte) []byte { return data[:len(data)-1] }, "sector layout"},
		{"noncartridge NCSD", func(data []byte) []byte { data[0x110] = 1; return data }, "cartridge NCSD"},
		{"zero media ID", func(data []byte) []byte {
			clear(data[0x108:0x110])
			return data
		}, "cartridge NCSD"},
		{"CARD2 even with blank trailer", func(data []byte) []byte { data[0x18d] = 2; return data }, "requires a CARD1"},
		{"CARD1 header with writable address", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x200:], testEnd3DS/0x200)
			return data
		}, "writable region"},
		{"units other than 512 bytes", func(data []byte) []byte { data[0x18e] = 1; return data }, "media unit"},
		{"implausible capacity", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x104:], 0x30001)
			return data
		}, "capacity"},
		{"used extent crosses image length", func(data []byte) []byte {
			binary.LittleEndian.PutUint64(data[ncsdUsedSizeOffset:], uint64(len(data))+1)
			return data
		}, "used size exceeds"},
		{"used extent uses the high word", func(data []byte) []byte {
			binary.LittleEndian.PutUint64(data[ncsdUsedSizeOffset:], (1<<32)+testEnd3DS)
			return data
		}, "used size exceeds"},
		{"missing main partition", func(data []byte) []byte {
			clear(data[0x120:0x128])
			return data
		}, "incomplete 3DS partition 0"},
		{"unbounded optional partition", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x128:], 0xffffffff)
			return data
		}, "incomplete 3DS partition 1"},
		{"overlapping partitions", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x158:], 0x6000/0x200)
			return data
		}, "partition 7 crosses"},
		{"partition crossing source length", func(data []byte) []byte { return data[:testEnd3DS-0x200] }, "partition 7 crosses"},
		{"missing final NCCH signature", func(data []byte) []byte {
			clear(data[testUpdate3DS+0x100 : testUpdate3DS+0x104])
			return data
		}, "NCCH signature"},
		{"different final partition ID", func(data []byte) []byte {
			data[testUpdate3DS+0x108] ^= 1
			return data
		}, "partition ID"},
		{"NCCH claims more than its enclosing partition", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[testUpdate3DS+0x104:], 0xffffffff)
			return data
		}, "NCCH length"},
		{"missing main executable", func(data []byte) []byte {
			data[testMain3DS+0x18d] = 1
			return data
		}, "lacks a cartridge executable"},
		{"NCCH hash region exceeds its file system", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[testMain3DS+0x1a8:], 5)
			return data
		}, "hash region"},
		{"NCCH overlaps declared sections", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[testMain3DS+0x1b0:], 8)
			return data
		}, "overlaps"},
		{"NCCH RomFS crosses logical content", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[testUpdate3DS+0x1b4:], 12)
			return data
		}, "RomFS extent"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.edit(testfixture.CARD1())
			if _, err := N3DSBoundary(bytes.NewReader(data), int64(len(data))); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("N3DSBoundary error = %v; want %q", err, tt.want)
			}
		})
	}
}

func FuzzCartridgeLayout(f *testing.F) {
	nds, n3ds := testfixture.NDS(true), testfixture.CARD1()
	f.Add(false, uint8(0), []byte("SYNTHETIC DS"))
	f.Add(false, uint8(1), []byte{0, 0, 0, 0})
	f.Add(false, uint8(3), []byte{0x21, 0x06, 0xc0, 0xde})
	f.Add(false, uint8(7), []byte{0xff, 0xff, 0xff, 0xff})
	f.Add(true, uint8(0), []byte("NCSD"))
	f.Add(true, uint8(1), []byte("NCCH"))
	f.Fuzz(func(t *testing.T, card1 bool, slot uint8, patch []byte) {
		if len(patch) > 1024 {
			t.Skip()
		}
		data := nds
		offsets := []int64{0, 0x1c0, ndsAuthPointer, ndsSecureAreaEnd, 0x9000, 0x9400, testNTREnd, 0x9058}
		parse := func(src io.ReaderAt, size int64) (int64, error) {
			return NDSBoundary(context.Background(), src, size)
		}
		if card1 {
			data = n3ds
			offsets = []int64{0x100, testMain3DS + 0x100, testUpdate3DS + 0x100, ncsdUsedSizeOffset}
			parse = N3DSBoundary
		}
		reader := fuzzCardReader{
			Reader: bytes.NewReader(data), start: offsets[int(slot)%len(offsets)], patch: patch,
		}
		keep, err := parse(reader, int64(len(data)))
		if err == nil && (keep <= 0 || keep > int64(len(data))) {
			t.Fatalf("accepted an invalid boundary %#x for %#x bytes", keep, len(data))
		}
	})
}

type fuzzCardReader struct {
	*bytes.Reader
	start int64
	patch []byte
}

func (r fuzzCardReader) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.Reader.ReadAt(p, offset)
	left := max(offset, r.start)
	right := min(offset+int64(n), r.start+int64(len(r.patch)))
	if left < right {
		copy(p[left-offset:right-offset], r.patch[left-r.start:right-r.start])
	}
	return n, err
}
