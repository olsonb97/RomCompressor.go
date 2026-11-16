package romcompressor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cardtrim "github.com/olsonb97/romcompressor.go/internal/trim"
)

const (
	testXCIRoot   = 0xf000
	testXCIHeader = 0x200
	testXCIPart   = 0x400
	testXCIEnd    = testXCIRoot + testXCIHeader + 3*testXCIPart
)

func TestXCIBoundary(t *testing.T) {
	for _, variant := range []string{"global", "terra", "already trimmed"} {
		t.Run(variant, func(t *testing.T) {
			data := testXCI(variant == "terra")
			if variant == "already trimmed" {
				data = data[:testXCIEnd]
			}
			size, err := cardtrim.XCIBoundary(context.Background(), bytes.NewReader(data), int64(len(data)))
			if err != nil || size != testXCIEnd {
				t.Fatalf("boundary %x, error %v, want %x", size, err, testXCIEnd)
			}
		})
	}
}

func TestXCIBoundaryAlignsHeaderWithUnpaddedNameTable(t *testing.T) {
	data := testXCI(false)
	const rootNamesSize = uint32(len("update\x00normal\x00secure\x00"))
	const childNamesSize = uint32(len("00000000000000000000000000000000.nca\x00"))
	binary.LittleEndian.PutUint32(data[testXCIRoot+8:], rootNamesSize)
	for index := range 3 {
		at := testXCIRoot + testXCIHeader + index*testXCIPart
		binary.LittleEndian.PutUint32(data[at+8:], childNamesSize)
		refreshTestXCIPartHash(data, index)
	}
	ctx := context.Background()
	keep, err := cardtrim.XCIBoundary(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil || keep != testXCIEnd {
		t.Fatalf("XCI boundary with compact name tables = %#x, %v; want %#x", keep, err, testXCIEnd)
	}

	// The final name now ends in page padding, outside its declared table.
	binary.LittleEndian.PutUint32(data[testXCIRoot+8:], rootNamesSize-1)
	refreshTestXCIRootHash(data, false)
	if _, err := cardtrim.XCIBoundary(ctx, bytes.NewReader(data), int64(len(data))); err == nil || !strings.Contains(err.Error(), "invalid HFS0 filename") {
		t.Fatalf("name terminated in alignment padding: %v", err)
	}
}

func TestXCIRejectsAmbiguousBoundary(t *testing.T) {
	rootEntry := testXCIRoot + 0x10
	secureAt := testXCIRoot + testXCIHeader + 2*testXCIPart
	for _, fixture := range []struct {
		name   string
		mutate func([]byte) []byte
		want   string
	}{
		{"unaligned", func(data []byte) []byte { return data[:len(data)-1] }, "length"},
		{"short", func(data []byte) []byte { return data[:testXCIEnd-0x200] }, "outside the image"},
		{"magic only", func(data []byte) []byte {
			clear(data[:0x100])
			return data
		}, "header"},
		{"second gamecard header", func(data []byte) []byte {
			data[0x10f] = 0x80
			return data
		}, "variant"},
		{"second gamecard header with cleared flag", func(data []byte) []byte {
			copy(data[0x300:], "HEAD")
			return data
		}, "variant"},
		{"T2 security selector without other markers", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x180:], 2)
			return data
		}, "variant"},
		{"unknown cartridge capacity", func(data []byte) []byte {
			data[0x10d] = 0
			return data
		}, "capacity"},
		{"unknown upper end bits", func(data []byte) []byte {
			data[0x11c] = 1
			return data
		}, "gamecard header"},
		{"overflowing declared end", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x118:], 0xffffffff)
			return data
		}, "outside the image"},
		{"certificate overlap", func(data []byte) []byte {
			binary.LittleEndian.PutUint64(data[0x130:], 0x7000)
			return data
		}, "cartridge metadata"},
		{"reserved certificate area overlap", func(data []byte) []byte {
			binary.LittleEndian.PutUint64(data[0x130:], testXCIRoot-0x200)
			return data
		}, "cartridge metadata"},
		{"secure storage overlaps root header", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x104:], testXCIRoot/0x200)
			return data
		}, "secure storage start"},
		{"out-of-bounds secure storage", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x104:], testXCIEnd/0x200)
			return data
		}, "secure storage start"},
		{"limit area page in uniform suffix", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x18c:], testXCIEnd/0x200)
			return data
		}, "limit area"},
		{"unused bytes within declared content", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x118:], testXCIEnd/0x200)
			return data
		}, ""},
		{"declared end before partition ends", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[0x118:], testXCIEnd/0x200-2)
			return data
		}, "past its partition"},
		{"wrong root header size", func(data []byte) []byte {
			binary.LittleEndian.PutUint64(data[0x138:], 0x400)
			return data
		}, "root HFS0 layout"},
		{"root hash mismatch", func(data []byte) []byte {
			data[testXCIRoot+0x1ff] = 1
			return data
		}, "root HFS0 hash"},
		{"overlap", func(data []byte) []byte {
			binary.LittleEndian.PutUint64(data[rootEntry+0x40:], 0x200)
			refreshTestXCIRootHash(data, false)
			return data
		}, "overlapping"},
		{"names start outside table", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[rootEntry+16:], 0xffffffff)
			refreshTestXCIRootHash(data, false)
			return data
		}, "outside its name table"},
		{"out-of-bounds nested file", func(data []byte) []byte {
			binary.LittleEndian.PutUint64(data[secureAt+0x18:], uint64(testXCIPart))
			refreshTestXCIPartHash(data, 2)
			return data
		}, "past its partition"},
		{"secure hash mismatch", func(data []byte) []byte {
			data[secureAt+0x1ff] = 1
			return data
		}, "secure partition header hash"},
		{"hash that covers only the partition magic", func(data []byte) []byte {
			entry := data[rootEntry+2*0x40:]
			binary.LittleEndian.PutUint32(entry[20:], 4)
			hash := sha256.Sum256(data[secureAt : secureAt+4])
			copy(entry[32:], hash[:])
			refreshTestXCIRootHash(data, false)
			return data
		}, "secure partition header hash region"},
		{"secure partition with no file data", func(data []byte) []byte {
			clear(data[secureAt+0x18 : secureAt+0x20])
			clear(data[secureAt+testXCIHeader : secureAt+testXCIPart])
			refreshTestXCIPartHash(data, 2)
			return data
		}, "missing its secure game partition"},
		{"unknown partition", func(data []byte) []byte {
			copy(data[testXCIRoot+0x10+3*0x40:], "extras\x00")
			refreshTestXCIRootHash(data, false)
			return data
		}, "unknown XCI root partition"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			data := fixture.mutate(testXCI(false))
			size, err := cardtrim.XCIBoundary(context.Background(), bytes.NewReader(data), int64(len(data)))
			if fixture.want == "" {
				if err != nil || size != testXCIEnd+0x200 {
					t.Fatalf("declared data gap must survive: size %x, error %v", size, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), fixture.want) {
				t.Fatalf("got %v, want error containing %q", err, fixture.want)
			}
		})
	}
}

func TestXCIBoundaryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := testXCI(false)
	if _, err := cardtrim.XCIBoundary(ctx, bytes.NewReader(data), int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
}

func FuzzXCIBoundary(f *testing.F) {
	f.Add(uint8(0), uint8(0), []byte("HEAD"), uint16(0))
	f.Add(uint8(1), uint8(0), []byte("HFS0"), uint16(0))
	f.Add(uint8(2), uint8(16), []byte{255, 255, 255, 255}, uint16(0))
	f.Fuzz(func(t *testing.T, section, offset uint8, patch []byte, truncate uint16) {
		data := testXCI(false)
		secure := testXCIRoot + testXCIHeader + 2*testXCIPart
		locations := [...]int{0x100, testXCIRoot, secure}
		start := locations[int(section)%len(locations)]
		copy(data[start+int(offset):start+testXCIHeader], patch)
		switch start {
		case testXCIRoot:
			refreshTestXCIRootHash(data, false)
		case secure:
			refreshTestXCIPartHash(data, 2)
		}
		data = data[:len(data)-int(truncate)%0x1000]
		end, err := cardtrim.XCIBoundary(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err == nil && (end <= 0 || end > int64(len(data)) || end%0x200 != 0) {
			t.Fatalf("accepted invalid boundary %#x for %#x bytes", end, len(data))
		}
	})
}

func TestTrimXCIKeepsAllCartridgeBytesThroughValidEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "cartridge.xci")
	output := filepath.Join(dir, "copy.xci")
	data := testXCI(true)
	for i := range data[testXCIEnd:] {
		data[testXCIEnd+i] = 0xff
	}
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanTrim(ctx, source)
	if err != nil || plan.Format != "xci" || plan.OutputBytes != testXCIEnd || plan.PaddingByte != 0xff {
		t.Fatalf("trim plan: %+v, %v", plan, err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("planning created an output: %v", err)
	}
	result, err := Trim(ctx, source, output, TrimOptions{})
	if err != nil || result.TrimPlan != plan {
		t.Fatalf("trim: %+v, %v", result, err)
	}
	actual, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(actual, data[:testXCIEnd]) {
		t.Fatalf("output differs from its original prefix: %v", err)
	}
	before, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, data) {
		t.Fatalf("original changed: %v", err)
	}
	if _, err := Trim(ctx, source, source, TrimOptions{}); err == nil {
		t.Fatal("accepted an in-place trim")
	}
}

func TestTrimXCIRejectsUnexpectedSuffixWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "ambiguous.xci")
	output := filepath.Join(dir, "copy.xci")
	data := testXCI(false)
	data[len(data)-1] = 1
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trim(context.Background(), source, output, TrimOptions{}); err == nil || !strings.Contains(err.Error(), "padding") {
		t.Fatalf("expected refusal to trim nonpadding suffix, got %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed trim left an output: %v", err)
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("source changed: %v", err)
	}
}

func TestTrimXCIRejectsConflictingSecurePartitionOffsets(t *testing.T) {
	const (
		secureAt    = testXCIRoot + testXCIHeader + 2*testXCIPart
		alternateAt = secureAt - testXCIHeader
	)
	data := testXCI(false)
	// The card header points to an alternate HFS0 in the normal partition's
	// payload. Its file reaches into the otherwise uniform suffix.
	copy(data[alternateAt:secureAt], data[secureAt:secureAt+testXCIHeader])
	binary.LittleEndian.PutUint64(data[alternateAt+0x18:], uint64(len(data)-secureAt))
	binary.LittleEndian.PutUint32(data[0x104:], alternateAt/0x200)

	dir := t.TempDir()
	source := filepath.Join(dir, "original.xci")
	output := filepath.Join(dir, "trimmed.xci")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := Trim(ctx, source, output, TrimOptions{}); err == nil || !strings.Contains(err.Error(), "secure partition offset") {
		t.Fatalf("Trim error = %v; want refusal for conflicting secure partition offsets", err)
	}
	assertTrimBytes(t, source, data)
	assertTrimAbsent(t, output)
}

func TestTrimXCIRejectsAmbiguousRootPartitionAlignment(t *testing.T) {
	const (
		names         = "update\x00normal\x00secure\x00"
		compactRoot   = 0x10 + 3*0x40 + len(names)
		compactHeader = 0x60
	)
	data := testXCI(false)
	binary.LittleEndian.PutUint32(data[testXCIRoot+8:], uint32(len(names)))
	// Page-aligned readers see the original partitions, including the HEAD
	// secure offset. Readers that use the raw header length see the other set.
	for i := 0; i < 3; i++ {
		at := testXCIRoot + compactRoot + i*testXCIPart
		header := data[at : at+compactHeader]
		clear(header)
		copy(header, "HFS0")
		binary.LittleEndian.PutUint32(header[4:], 1)
		binary.LittleEndian.PutUint32(header[8:], compactHeader-0x10-0x40)
		binary.LittleEndian.PutUint64(header[0x18:], 1)
		copy(header[0x50:], "file.bin\x00")
		if i == 2 {
			// The alternate secure partition references the entire uniform suffix.
			binary.LittleEndian.PutUint64(header[0x10:], uint64(testXCIEnd-at-compactHeader))
			binary.LittleEndian.PutUint64(header[0x18:], uint64(len(data)-testXCIEnd))
		}
	}
	refreshTestXCIRootHash(data, false)

	dir := t.TempDir()
	source := filepath.Join(dir, "original.xci")
	output := filepath.Join(dir, "trimmed.xci")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trim(context.Background(), source, output, TrimOptions{}); err == nil || !strings.Contains(err.Error(), "ambiguous HFS0 partition alignment") {
		t.Fatalf("Trim error = %v; want refusal for competing root partition addresses", err)
	}
	assertTrimBytes(t, source, data)
	assertTrimAbsent(t, output)
}

func TestTrimXCIRejectsChangedHashedFileWithoutOutput(t *testing.T) {
	const (
		secureAt = testXCIRoot + testXCIHeader + 2*testXCIPart
		extra    = 0x10000
		end      = testXCIEnd + extra
		hashAt   = 0x20
		hashSize = extra + 0x180
	)
	data := append(testXCI(false), make([]byte, extra)...)
	binary.LittleEndian.PutUint32(data[0x118:], end/0x200-1)
	binary.LittleEndian.PutUint64(data[testXCIRoot+0x10+2*0x40+8:], testXCIPart+extra)
	entry := data[secureAt+0x10 : secureAt+0x50]
	binary.LittleEndian.PutUint64(entry[8:], testXCIPart+extra-testXCIHeader)
	binary.LittleEndian.PutUint32(entry[20:], hashSize)
	binary.LittleEndian.PutUint64(entry[24:], hashAt)
	fileStart := secureAt + testXCIHeader
	hashed := data[fileStart+hashAt : fileStart+hashAt+hashSize]
	hashed[0], hashed[len(hashed)-1] = 0x7b, 0x3a
	digest := sha256.Sum256(hashed)
	copy(entry[32:], digest[:])
	refreshTestXCIPartHash(data, 2)
	ctx := context.Background()
	if keep, err := cardtrim.XCIBoundary(ctx, bytes.NewReader(data), int64(len(data))); err != nil || keep != end {
		t.Fatalf("valid hashed file: boundary %#x, error %v", keep, err)
	}

	hashed[len(hashed)-1] ^= 1
	dir := t.TempDir()
	source := filepath.Join(dir, "original.xci")
	output := filepath.Join(dir, "trimmed.xci")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trim(ctx, source, output, TrimOptions{}); err == nil || !strings.Contains(err.Error(), "file hash mismatch") {
		t.Fatalf("Trim error = %v; want refusal for changed hashed data", err)
	}
	assertTrimBytes(t, source, data)
	assertTrimAbsent(t, output)
}

func testXCI(terra bool) []byte {
	// A synthetic HEAD and a root HFS0 containing update, normal, and
	// secure HFS0 partitions. No bytes are taken from a retail dump.
	data := make([]byte, testXCIEnd+0x400)
	for i := range data[:0x100] {
		data[i] = byte(i)
	}
	copy(data[0x100:], "HEAD")
	binary.LittleEndian.PutUint32(data[0x104:], (testXCIRoot+testXCIHeader+2*testXCIPart)/0x200)
	binary.LittleEndian.PutUint32(data[0x108:], 0xffffffff)
	data[0x10d] = 0xfa
	binary.LittleEndian.PutUint32(data[0x118:], testXCIEnd/0x200-1)
	binary.LittleEndian.PutUint64(data[0x130:], testXCIRoot)
	binary.LittleEndian.PutUint64(data[0x138:], testXCIHeader)
	binary.LittleEndian.PutUint32(data[0x180:], 1)
	binary.LittleEndian.PutUint32(data[0x18c:], (testXCIRoot+testXCIHeader+2*testXCIPart)/0x200-1)
	copy(data[0x7000:], "synthetic preserved cartridge certificate")
	root := data[testXCIRoot:]
	copy(root, "HFS0")
	binary.LittleEndian.PutUint32(root[4:], 3)
	binary.LittleEndian.PutUint32(root[8:], testXCIHeader-0x10-3*0x40)
	copy(root[0x10+3*0x40:], "update\x00normal\x00secure\x00")
	for index := 0; index < 3; index++ {
		entry := root[0x10+index*0x40:]
		binary.LittleEndian.PutUint64(entry, uint64(index*testXCIPart))
		binary.LittleEndian.PutUint64(entry[8:], testXCIPart)
		binary.LittleEndian.PutUint32(entry[16:], uint32(index*7))
		binary.LittleEndian.PutUint32(entry[20:], testXCIHeader)
		at := testXCIRoot + testXCIHeader + index*testXCIPart
		child := data[at : at+testXCIPart]
		copy(child, "HFS0")
		binary.LittleEndian.PutUint32(child[4:], 1)
		binary.LittleEndian.PutUint32(child[8:], testXCIHeader-0x10-0x40)
		binary.LittleEndian.PutUint64(child[0x18:], testXCIPart-testXCIHeader)
		copy(child[0x50:], "00000000000000000000000000000000.nca\x00")
		child[testXCIHeader] = byte(index + 1)
		refreshTestXCIPartHash(data, index)
	}
	refreshTestXCIRootHash(data, terra)
	return data
}

func refreshTestXCIPartHash(data []byte, index int) {
	at := testXCIRoot + testXCIHeader + index*testXCIPart
	hash := sha256.Sum256(data[at : at+testXCIHeader])
	copy(data[testXCIRoot+0x10+index*0x40+32:], hash[:])
	refreshTestXCIRootHash(data, false)
}

func refreshTestXCIRootHash(data []byte, terra bool) {
	hash := sha256.New()
	hash.Write(data[testXCIRoot : testXCIRoot+testXCIHeader])
	if terra {
		hash.Write([]byte{1})
	}
	copy(data[0x140:], hash.Sum(nil))
}
