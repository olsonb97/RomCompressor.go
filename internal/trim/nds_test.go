package trim

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

const (
	testNTREnd = testfixture.NDSTrimmedSize
	testTWLEnd = testfixture.DSiTrimmedSize
)

func TestHasNDSHeaderRecognizesChecksumsWithoutTrimApproval(t *testing.T) {
	for _, twl := range []bool{false, true} {
		data := testfixture.NDS(twl)
		if !HasNDSHeader(data[:0x160]) {
			t.Fatal("valid cartridge header was not recognized")
		}
		if HasNDSHeader(data[:0x15f]) {
			t.Fatal("truncated cartridge header was recognized")
		}
		data[0x15e] ^= 1
		if HasNDSHeader(data) {
			t.Fatal("invalid header checksum was recognized")
		}
		fixNDSCRC(data)
		data[0xc0] ^= 1
		fixNDSCRC(data)
		if HasNDSHeader(data) {
			t.Fatal("invalid logo checksum was recognized")
		}
		data = testfixture.NDS(twl)
		data[0x15c] ^= 1
		fixNDSCRC(data)
		if HasNDSHeader(data) {
			t.Fatal("invalid stored logo checksum was recognized")
		}
	}
	data := testfixture.NDS(false)
	putNDSWord(data, 0x94, 1)
	fixNDSCRC(data)
	if !HasNDSHeader(data) {
		t.Fatal("writable cartridge identity should still be recognizable")
	}
	if _, err := NDSBoundary(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil || !strings.Contains(err.Error(), "writable NAND") {
		t.Fatalf("header recognition allowed writable cartridge trimming: %v", err)
	}
}

func TestNDSBoundary(t *testing.T) {
	plain := testfixture.NDS(false)
	download := bytes.Clone(plain)
	copy(download[testNTREnd:], "ac")
	for i := 2; i < 0x88; i++ {
		download[testNTREnd+i] = byte(7 * i)
	}
	twl := testfixture.NDS(true)
	exclusive := bytes.Clone(twl)
	exclusive[0x12] = 3
	putNDSWord(exclusive, 0x1d0, 0)
	putNDSWord(exclusive, 0x1dc, 0)
	fixNDSCRC(exclusive)
	for _, tt := range []struct {
		name string
		data []byte
		keep int64
	}{
		{"DS cartridge", plain, testNTREnd},
		{"DS cartridge with Download Play trailer", download, testNTREnd + 0x88},
		{"already trimmed DS cartridge", plain[:testNTREnd], testNTREnd},
		{"already trimmed Download Play cartridge", download[:testNTREnd+0x88], testNTREnd + 0x88},
		{"DSi-enhanced cartridge", twl, testTWLEnd},
		{"DSi-exclusive cartridge", exclusive, testTWLEnd},
		{"already trimmed DSi cartridge", twl[:testTWLEnd], testTWLEnd},
	} {
		t.Run(tt.name, func(t *testing.T) {
			keep, err := NDSBoundary(context.Background(), bytes.NewReader(tt.data), int64(len(tt.data)))
			if err != nil || keep != tt.keep {
				t.Fatalf("NDSBoundary = %#x, %v; want %#x", keep, err, tt.keep)
			}
		})
	}
}

func TestNDSBoundaryPreservesExplicitDownloadAuth(t *testing.T) {
	const start = testNTREnd + 0x200
	for _, tt := range []struct {
		name       string
		headerSize uint32
	}{
		{"full header", 0x4000},
		{"short header", 0x200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := testfixture.NDS(false)
			putNDSWord(data, 0x84, tt.headerSize)
			putNDSWord(data, ndsAuthPointer, start)
			fixNDSCRC(data)
			// The pointer is the only evidence that part of the uniform suffix
			// belongs to Download Play.
			for _, size := range []int{len(data), start + 0x40, start} {
				keep, err := NDSBoundary(context.Background(), bytes.NewReader(data[:size]), int64(size))
				want := int64(min(size, start+ndsDownloadAuth))
				if err != nil || keep != want {
					t.Fatalf("NDSBoundary for %#x input bytes = %#x, %v; want %#x", size, keep, err, want)
				}
			}
		})
	}
}

func TestNDSBoundaryPreservesARM9Footers(t *testing.T) {
	const trimEnd = 0x10000
	const arm9At = 0xb000
	for _, records := range []int{1, 2, 513} {
		data := testfixture.NDS(false)
		footerAt := trimEnd + 8 - records*ndsARM9FooterSize
		putNDSWord(data, 0x20, arm9At)
		putNDSWord(data, 0x2c, uint32(footerAt-arm9At))
		putNDSWord(data, 0x80, trimEnd)
		for record := range records {
			putNDSWord(data, footerAt+ndsARM9FooterSize*record, 0xdec00621)
		}
		fixNDSCRC(data)
		// The declared end includes the final magic word, but its two
		// remaining words are identical to the uniform FF suffix.
		keep, err := NDSBoundary(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil || keep != trimEnd+8 {
			t.Fatalf("NDSBoundary with %d records = %#x, %v; want %#x", records, keep, err, trimEnd+8)
		}

		truncated := data[:trimEnd+4]
		if _, err := NDSBoundary(context.Background(), bytes.NewReader(truncated), int64(len(truncated))); err == nil || !strings.Contains(err.Error(), "truncated DS ARM9 footer") {
			t.Fatalf("truncated layout with %d records returned %v", records, err)
		}
	}
}

func TestNDSBoundaryAcceptsNestedFileNameTable(t *testing.T) {
	data := testfixture.NDS(false)
	putNDSWord(data, 0x44, 26)
	putNDSWord(data, 0x9000, 16)
	putNDSWord(data, 0x9004, 0x00020000)
	putNDSWord(data, 0x9008, 21)
	putNDSWord(data, 0x900c, 0xf0000000)
	copy(data[0x9010:], []byte{0x81, 's', 1, 0xf0, 0, 1, 'a', 1, 'b', 0})
	fixNDSCRC(data)
	keep, err := NDSBoundary(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil || keep != testNTREnd {
		t.Fatalf("NDSBoundary with a nested FNT = %#x, %v; want %#x", keep, err, testNTREnd)
	}
}

func TestNDSOverlayAuthenticationSharesSectorBudget(t *testing.T) {
	var header [ndsHeaderReadSize]byte
	var fat [24]byte
	binary.LittleEndian.PutUint32(header[0x4c:], uint32(len(fat)))
	binary.LittleEndian.PutUint32(header[0x54:], 2*ndsOverlaySize)
	for i, span := range [][2]uint32{
		{0x2000, 0x42400},  // Its sector coverage is capped at 0x40000.
		{0x80000, 0xc0080}, // The remaining budget is also 0x40000.
		{0xc0080, 0xc0180}, // Beyond the two ARM9 overlay FAT entries.
	} {
		binary.LittleEndian.PutUint32(fat[i*8:], span[0])
		binary.LittleEndian.PutUint32(fat[i*8+4:], span[1])
	}
	end, err := validateNDSFAT(context.Background(), bytes.NewReader(fat[:]), header[:], 0x2000, 0xc0180)
	if err != nil || end != 0xc0000 {
		t.Fatalf("overlay authentication end = %#x, %v; want 0xc0000", end, err)
	}
}

func TestNDSRejectsOverlayReferencesOutsideDeclaredFAT(t *testing.T) {
	for _, cpu := range []string{"ARM9", "ARM7"} {
		t.Run(cpu, func(t *testing.T) {
			data := testfixture.NDS(false)
			const overlayAt = 0x9040
			if cpu == "ARM7" {
				clear(data[0x50:0x58])
				putNDSWord(data, 0x58, overlayAt)
				putNDSWord(data, 0x5c, ndsOverlaySize)
				fixNDSCRC(data)
			}
			ctx := context.Background()
			putNDSWord(data, overlayAt+0x18, 1)
			if keep, err := NDSBoundary(ctx, bytes.NewReader(data), int64(len(data))); err != nil || keep != testNTREnd {
				t.Fatalf("valid overlay returned boundary %#x, %v", keep, err)
			}
			// ID 2 reaches a record just outside the two declared FAT entries.
			// That record points to an overlay made of uniform suffix bytes.
			putNDSWord(data, overlayAt+0x18, 2)
			putNDSWord(data, 0x9030, testNTREnd)
			putNDSWord(data, 0x9034, testNTREnd+0x100)
			clear(data[testNTREnd:])
			if _, err := NDSBoundary(ctx, bytes.NewReader(data), int64(len(data))); err == nil || !strings.Contains(err.Error(), cpu+" overlay 0 points beyond the file allocation table") {
				t.Fatalf("invalid overlay reference returned %v", err)
			}
		})
	}
}

func TestNDSRejectsUnsafeCartridgeLayouts(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func([]byte) []byte
		want string
	}{
		{"truncated header", func(data []byte) []byte { return data[:0x160] }, "shorter"},
		{"bad header CRC", func(data []byte) []byte { data[0] ^= 1; return data }, "checksum"},
		{"bad logo with valid header CRC", func(data []byte) []byte {
			data[0xc0] ^= 1
			fixNDSCRC(data)
			return data
		}, "checksum"},
		{"invalid game code", func(data []byte) []byte {
			data[0x0c] = '.'
			fixNDSCRC(data)
			return data
		}, "game code"},
		{"unknown unit code", func(data []byte) []byte {
			data[0x12] = 1
			fixNDSCRC(data)
			return data
		}, "unit code"},
		{"invalid capacity exponent", func(data []byte) []byte {
			data[0x14] = 0xff
			fixNDSCRC(data)
			return data
		}, "capacity"},
		{"overdump", func(data []byte) []byte { return append(data, 0xff) }, "size"},
		{"file shorter than declared extent", func(data []byte) []byte { return data[:testNTREnd-1] }, "size"},
		{"zero size", func(data []byte) []byte {
			putNDSWord(data, 0x80, 0)
			fixNDSCRC(data)
			return data
		}, "size"},
		{"header overlaps boot region", func(data []byte) []byte {
			putNDSWord(data, 0x84, 0x8000)
			fixNDSCRC(data)
			return data
		}, "header extent"},
		{"blank NAND save region", func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[0x94:], 1)
			binary.LittleEndian.PutUint16(data[0x96:], 1)
			fixNDSCRC(data)
			return data
		}, "writable NAND"},
		{"legacy NAND cartridge with blank extent fields", func(data []byte) []byte {
			copy(data[0x0c:0x10], "AXBJ")
			fixNDSCRC(data)
			return data
		}, "writable NAND"},
		{"missing ARM7 executable", func(data []byte) []byte {
			putNDSWord(data, 0x3c, 0)
			fixNDSCRC(data)
			return data
		}, "ARM7"},
		{"ARM9 beyond declared end", func(data []byte) []byte {
			putNDSWord(data, 0x2c, 0xffffffff)
			fixNDSCRC(data)
			return data
		}, "ARM9"},
		{"ARM7 overlaps the secure area", func(data []byte) []byte {
			putNDSWord(data, 0x30, 0x7000)
			fixNDSCRC(data)
			return data
		}, "boot layout"},
		{"ARM binaries overlap", func(data []byte) []byte {
			putNDSWord(data, 0x2c, 0x5000)
			fixNDSCRC(data)
			return data
		}, "binaries overlap"},
		{"bad file name table", func(data []byte) []byte {
			putNDSWord(data, 0x44, 4)
			fixNDSCRC(data)
			return data
		}, "file name table"},
		{"directory name list starts in uniform suffix", func(data []byte) []byte {
			putNDSWord(data, 0x9000, testNTREnd-0x9000)
			clear(data[testNTREnd:])
			return data
		}, "outside its name data"},
		{"directory terminator falls in uniform suffix", func(data []byte) []byte {
			putNDSWord(data, 0x40, testNTREnd-12)
			putNDSWord(data, 0x44, 12)
			copy(data[testNTREnd-12:], data[0x9000:0x900c])
			clear(data[testNTREnd:])
			fixNDSCRC(data)
			return data
		}, "file name table terminator"},
		{"directory name points outside its table", func(data []byte) []byte {
			copy(data[0x9008:], []byte{0x81, 's', 1, 0xf0, 0})
			return data
		}, "outside the directory records"},
		{"file name points beyond FAT", func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[0x9004:], 1)
			return data
		}, "beyond the file allocation table"},
		{"partial FAT record", func(data []byte) []byte {
			putNDSWord(data, 0x4c, 13)
			fixNDSCRC(data)
			return data
		}, "allocation table"},
		{"FAT points past trim point", func(data []byte) []byte {
			putNDSWord(data, 0x9024, testNTREnd+0x100)
			return data
		}, "file 0"},
		{"FAT range reversed", func(data []byte) []byte {
			putNDSWord(data, 0x9024, 0x9f00)
			return data
		}, "file 0"},
		{"banner points to trailer", func(data []byte) []byte {
			putNDSWord(data, 0x68, testNTREnd-4)
			binary.LittleEndian.PutUint16(data[testNTREnd-4:], 1)
			fixNDSCRC(data)
			return data
		}, "banner extends"},
		{"truncated Download Play signature", func(data []byte) []byte {
			copy(data[testNTREnd:], "ac")
			return data[:testNTREnd+7]
		}, "truncated DS Download Play"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.edit(testfixture.NDS(false))
			if _, err := NDSBoundary(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NDSBoundary error = %v; want %q", err, tt.want)
			}
		})
	}
}

func TestNDSRejectsUnsafeDSiLayouts(t *testing.T) {
	for _, tt := range []struct {
		name string
		at   int
		word uint32
		want string
	}{
		{"missing total ROM size", 0x210, 0, "DSi cartridge extent"},
		{"TWL total exceeds input", 0x210, 0x110000, "DSi cartridge extent"},
		{"DSiWare title", 0x234, 0x00030004, "cartridge title"},
		{"title code mismatch", 0x230, 0x11223344, "cartridge title"},
		{"ARM9i overlaps NTR", 0x1c0, testNTREnd - 4, "ARM9i"},
		{"ARM7i goes past TWL", 0x1dc, 0x8000, "ARM7i"},
		{"TWL digest coverage out of bounds", 0x1ec, 0xffffffff, "TWL digest"},
		{"hash table out of bounds", 0x1f0, testTWLEnd, "digest sector hash table"},
		{"modcrypt region out of bounds", 0x224, 0x20000, "first modcrypt region"},
		{"banner smaller than version", 0x208, 0x400, "banner extends"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := testfixture.NDS(true)
			putNDSWord(data, tt.at, tt.word)
			if _, err := NDSBoundary(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NDSBoundary error = %v; want %q", err, tt.want)
			}
		})
	}
}

func TestNDSRejectsDSiBootAreaBeyondDeclaredTotal(t *testing.T) {
	data := testfixture.NDS(true)
	putNDSWord(data, 0x1c0, testTWLEnd-0x2000)
	putNDSWord(data, 0x1cc, 0x1000)
	if _, err := NDSBoundary(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil || !strings.Contains(err.Error(), "ARM9i boot area") {
		t.Fatalf("NDSBoundary error = %v; want an ARM9i boot area error", err)
	}
}

func TestNDSCRC16Vector(t *testing.T) {
	if got := ndsCRC16([]byte("123456789")); got != 0x4b37 {
		t.Fatalf("CRC-16 = %#x; want 0x4b37", got)
	}
}

func putNDSWord(data []byte, at int, value uint32) {
	binary.LittleEndian.PutUint32(data[at:], value)
}

func fixNDSCRC(data []byte) {
	binary.LittleEndian.PutUint16(data[0x15e:], ndsCRC16(data[:0x15e]))
}
