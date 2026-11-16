package codec

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func encodeFixture(t testing.TB, format string, data []byte) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "encoded")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Encode(context.Background(), format, bytes.NewReader(data), int64(len(data)), f, Options{Name: "a/game.iso"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoundTrips(t *testing.T) {
	random := make([]byte, 20791)
	rand.New(rand.NewSource(1)).Read(random)
	for _, format := range []string{"zip", "gzip", "cso", "zso"} {
		t.Run(format, func(t *testing.T) {
			for _, data := range [][]byte{nil, {0}, bytes.Repeat([]byte("game"), 20000), random} {
				encoded := encodeFixture(t, format, data)
				var got bytes.Buffer
				if err := Decode(context.Background(), format, bytes.NewReader(encoded), int64(len(encoded)), &got, Options{}); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got.Bytes(), data) {
					t.Fatal("roundtrip changed payload")
				}
			}
		})
	}
}

func TestGZIPUnicodeFilename(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "gzip")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := []byte("game data")
	if err := Encode(context.Background(), "gzip", bytes.NewReader(data), int64(len(data)), f, Options{Name: "ゲーム.bin"}); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Decode(context.Background(), "gzip", f, st.Size(), &output, Options{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), data) {
		t.Fatal("Unicode filename changed payload")
	}
}

func TestGZIPRejectsReservedFlags(t *testing.T) {
	data := encodeFixture(t, "gzip", []byte("game data"))
	data[3] |= 0xe0
	if err := Verify(context.Background(), "gzip", bytes.NewReader(data), int64(len(data)), Options{}); err == nil {
		t.Fatal("accepted a gzip header with reserved flags set")
	}
}

func TestGZIPConcatenatedMembers(t *testing.T) {
	ctx := context.Background()
	first := []byte("first member")
	second := []byte("second member")
	want := slices.Concat(first, second)
	empty := encodeFixture(t, "gzip", nil)
	encoded := slices.Concat(encodeFixture(t, "gzip", first), empty, encodeFixture(t, "gzip", second), empty)
	limit := int64(len(want))
	var got bytes.Buffer
	var finished int64
	opts := Options{MaxOutput: limit, Progress: func(done, total int64) {
		if done <= finished || total != -1 {
			t.Fatalf("unexpected progress: %d of %d after %d", done, total, finished)
		}
		finished = done
	}}
	if err := Decode(ctx, "gzip", bytes.NewReader(encoded), int64(len(encoded)), &got, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) || finished != limit {
		t.Fatalf("restored %q after progress %d, want %q", got.Bytes(), finished, want)
	}
	if err := Verify(ctx, "gzip", bytes.NewReader(encoded), int64(len(encoded)), Options{MaxOutput: limit}); err != nil {
		t.Fatal(err)
	}
	got.Reset()
	err := Decode(ctx, "gzip", bytes.NewReader(encoded), int64(len(encoded)), &got, Options{MaxOutput: limit - 1})
	if err == nil || !strings.Contains(err.Error(), "output limit") || !bytes.Equal(got.Bytes(), first) {
		t.Fatalf("expected aggregate limit to stop before writing the second member, wrote %q: %v", got.Bytes(), err)
	}
}

func TestGZIPRejectsInvalidLaterMember(t *testing.T) {
	first := encodeFixture(t, "gzip", []byte("first member"))
	second := encodeFixture(t, "gzip", []byte("second member"))
	for _, tc := range []struct {
		name  string
		index int
		mask  byte
		want  error
	}{
		{"reserved flags", 3, 0x20, gzip.ErrHeader},
		{"CRC32", len(second) - 8, 1, gzip.ErrChecksum},
		{"ISIZE", len(second) - 4, 1, gzip.ErrChecksum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := slices.Concat(first, second)
			encoded[len(first)+tc.index] ^= tc.mask
			if err := Verify(context.Background(), "gzip", bytes.NewReader(encoded), int64(len(encoded)), Options{}); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	for _, suffix := range [][]byte{second[:1], second[:9], second[:len(second)-1], slices.Concat(second, []byte{0})} {
		encoded := slices.Concat(first, suffix)
		if err := Verify(context.Background(), "gzip", bytes.NewReader(encoded), int64(len(encoded)), Options{}); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("got %v with %d trailing bytes, want unexpected EOF", err, len(suffix))
		}
	}
}

func TestGZIPChecksCancellationWhileReadingEmptyBlocks(t *testing.T) {
	data := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 255}
	data = append(data, bytes.Repeat([]byte{0, 0, 0, 255, 255}, 5000)...)
	data = append(data, 1, 0, 0, 255, 255)  // Final empty DEFLATE block.
	data = append(data, make([]byte, 8)...) // Empty payload CRC and size.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancellingReaderAt{ReaderAt: bytes.NewReader(data), cancel: cancel, cancelAfter: 2}
	if err := Verify(ctx, "gzip", reader, int64(len(data)), Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation while reading compressed GZIP data, got %v", err)
	}
	if reader.reads != 2 {
		t.Fatalf("continued reading empty blocks after cancellation: %d reads", reader.reads)
	}
}

func TestIndexPageBoundary(t *testing.T) {
	data := bytes.Repeat([]byte{1}, (indexPageEntries+2)*writeBlockSize)
	for _, format := range []string{"cso", "zso"} {
		encoded := encodeFixture(t, format, data)
		var got bytes.Buffer
		if err := Decode(context.Background(), format, bytes.NewReader(encoded), int64(len(encoded)), &got, Options{}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), data) {
			t.Fatal("index page boundary changed payload")
		}
	}
}

func TestDecoderBoundsAndTruncation(t *testing.T) {
	data := bytes.Repeat([]byte("input"), 2000)
	for _, format := range []string{"zip", "gzip", "cso", "zso"} {
		t.Run(format, func(t *testing.T) {
			encoded := encodeFixture(t, format, data)
			if err := Decode(context.Background(), format, bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{MaxOutput: 20}); err == nil {
				t.Fatal("limit accepted")
			}
			for _, length := range []int{0, 10, len(encoded) / 2, len(encoded) - 1} {
				if err := Decode(context.Background(), format, bytes.NewReader(encoded[:length]), int64(length), io.Discard, Options{}); err == nil {
					t.Fatalf("accepted truncated %s at %d", format, length)
				}
			}
			trailing := append(append([]byte(nil), encoded...), 0)
			if err := Decode(context.Background(), format, bytes.NewReader(trailing), int64(len(trailing)), io.Discard, Options{}); err == nil {
				t.Fatal("trailing data accepted")
			}
		})
	}
}

func TestZIPSelectionAndChecksum(t *testing.T) {
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for _, name := range []string{"folder/a.gba", "folder/b.gba"} {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		entry.Write([]byte(name))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	encoded := data.Bytes()
	if err := Decode(context.Background(), "zip", bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{}); err == nil {
		t.Fatal("ambiguous ZIP accepted")
	}
	var got bytes.Buffer
	if err := Decode(context.Background(), "zip", bytes.NewReader(encoded), int64(len(encoded)), &got, Options{Entry: "folder/b.gba"}); err != nil {
		t.Fatal(err)
	}
	if got.String() != "folder/b.gba" {
		t.Fatal("wrong ZIP selected")
	}
	central := bytes.Index(encoded, []byte{'P', 'K', 1, 2})
	encoded[central+16] ^= 0x80
	if err := Decode(context.Background(), "zip", bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{Entry: "folder/a.gba"}); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
}

func TestZIPRejectsLocalHeaderDisagreement(t *testing.T) {
	ctx := context.Background()
	payload := []byte("synthetic game data")
	original := storedZIPFixture(t, payload, nil)
	for _, test := range []struct {
		name   string
		offset int
		bits   byte
	}{
		{"name", 30, 1},
		{"encryption flag", 6, 1},
		{"descriptor flag", 6, 8},
		{"compression method", 8, 8},
		{"checksum", 14, 1},
		{"compressed size", 18, 1},
		{"expanded size", 22, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := bytes.Clone(original)
			data[test.offset] ^= test.bits
			if err := Verify(ctx, "zip", bytes.NewReader(data), int64(len(data)), Options{}); err == nil {
				t.Fatal("verified contradictory ZIP headers")
			}
			var output bytes.Buffer
			if err := Decode(ctx, "zip", bytes.NewReader(data), int64(len(data)), &output, Options{}); err == nil || output.Len() != 0 {
				t.Fatalf("copied %d bytes with contradictory ZIP headers: %v", output.Len(), err)
			}
		})
	}
}

func TestZIPChecksZIP64LocalMemberSizesAndOffset(t *testing.T) {
	ctx := context.Background()
	payload := []byte("small ZIP64 payload")
	// A forced ZIP64 size field can describe even a small member.
	extra := []byte{1, 0, 16, 0}
	extra = binary.LittleEndian.AppendUint64(extra, uint64(len(payload)))
	extra = binary.LittleEndian.AppendUint64(extra, uint64(len(payload)))
	data := storedZIPFixture(t, payload, extra)
	end := data[len(data)-22:]
	centralAt := int(binary.LittleEndian.Uint32(end[16:]))
	binary.LittleEndian.PutUint32(data[18:], 0xffffffff)
	binary.LittleEndian.PutUint32(data[22:], 0xffffffff)

	// Add the central ZIP64 offset and disk while leaving the local extra unchanged.
	central := data[centralAt : len(data)-22]
	binary.LittleEndian.PutUint32(central[20:], 0xffffffff)
	binary.LittleEndian.PutUint32(central[24:], 0xffffffff)
	binary.LittleEndian.PutUint16(central[34:], 0xffff)
	binary.LittleEndian.PutUint32(central[42:], 0xffffffff)
	binary.LittleEndian.PutUint16(central[30:], uint16(len(extra)+12))
	binary.LittleEndian.PutUint16(central[len(central)-len(extra)+2:], 28)
	binary.LittleEndian.PutUint32(end[12:], uint32(len(central)+12))
	data = slices.Concat(data[:len(data)-22], make([]byte, 12), end)

	var restored bytes.Buffer
	opts := Options{Entry: "game.bin"}
	if err := Decode(ctx, "zip", bytes.NewReader(data), int64(len(data)), &restored, opts); err != nil || !bytes.Equal(restored.Bytes(), payload) {
		t.Fatalf("ZIP64 member did not round-trip: %v", err)
	}
	data[30+len("game.bin")+4] ^= 1 // First byte of the local ZIP64 expanded size.
	if err := Verify(ctx, "zip", bytes.NewReader(data), int64(len(data)), opts); err == nil {
		t.Fatal("accepted mismatched ZIP64 member sizes")
	}
}

func TestZIPMemberDiskMustMatchSingleDiskArchive(t *testing.T) {
	ctx := context.Background()
	payload := []byte("cartridge data")
	for _, test := range []struct {
		name  string
		disk  uint16
		extra []byte
		valid bool
	}{
		{"ordinary disk zero", 0, nil, true},
		{"ordinary second disk", 1, nil, false},
		{"ZIP64 disk zero", 0xffff, []byte{1, 0, 4, 0, 0, 0, 0, 0}, true},
		{"ZIP64 second disk", 0xffff, []byte{1, 0, 4, 0, 1, 0, 0, 0}, false},
		{"missing ZIP64 disk", 0xffff, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := storedZIPFixture(t, payload, nil)
			end := data[len(data)-22:]
			centralAt := int(binary.LittleEndian.Uint32(end[16:]))
			central := data[centralAt : len(data)-22]
			binary.LittleEndian.PutUint16(central[34:], test.disk)
			binary.LittleEndian.PutUint16(central[30:], uint16(len(test.extra)))
			binary.LittleEndian.PutUint32(end[12:], uint32(len(central)+len(test.extra)))
			data = slices.Concat(data[:len(data)-22], test.extra, end)
			if err := Verify(ctx, "zip", bytes.NewReader(data), int64(len(data)), Options{}); (err == nil) != test.valid {
				t.Errorf("Verify error = %v, want success = %t", err, test.valid)
			}
			var output bytes.Buffer
			err := Decode(ctx, "zip", bytes.NewReader(data), int64(len(data)), &output, Options{})
			if test.valid && (err != nil || !bytes.Equal(output.Bytes(), payload)) {
				t.Errorf("Decode produced %q, %v", output.Bytes(), err)
			} else if !test.valid && (err == nil || output.Len() != 0) {
				t.Errorf("decoded %d bytes with unsupported member disk: %v", output.Len(), err)
			}
		})
	}
}

func TestZIPChecksMemberBoundsInPhysicalOrder(t *testing.T) {
	ctx := context.Background()
	innerPayload := []byte("second member payload")
	inner := storedZIPFixture(t, innerPayload, nil)
	innerCentralAt := int(binary.LittleEndian.Uint32(inner[len(inner)-6:]))
	innerLocal, innerCentral := inner[:innerCentralAt], inner[innerCentralAt:len(inner)-22]
	copy(innerLocal[30:], "part.bin")
	copy(innerCentral[46:], "part.bin")

	// The first stored member's data is an exact copy of the second local
	// record. The bad directory points into that data instead of after it.
	outer := storedZIPFixture(t, innerLocal, nil)
	outerCentralAt := int(binary.LittleEndian.Uint32(outer[len(outer)-6:]))
	outerLocal, outerCentral := outer[:outerCentralAt], outer[outerCentralAt:len(outer)-22]
	end := outer[len(outer)-22:]
	binary.LittleEndian.PutUint16(end[8:], 2)
	binary.LittleEndian.PutUint16(end[10:], 2)
	binary.LittleEndian.PutUint32(end[12:], uint32(len(innerCentral)+len(outerCentral)))
	for _, overlap := range []bool{false, true} {
		localRecords := outerLocal
		innerStart := len(outerLocal) - len(innerLocal)
		if !overlap {
			gap := []byte("padding between local records")
			innerStart = len(outerLocal) + len(gap)
			localRecords = slices.Concat(outerLocal, gap, innerLocal)
		}
		binary.LittleEndian.PutUint32(innerCentral[42:], uint32(innerStart))
		binary.LittleEndian.PutUint32(end[16:], uint32(len(localRecords)))
		// The central directory has the two member records in reverse order.
		data := slices.Concat(localRecords, innerCentral, outerCentral, end)
		if err := Verify(ctx, "zip", bytes.NewReader(data), int64(len(data)), Options{}); (err != nil) != overlap {
			t.Errorf("Verify error = %v, overlap = %t", err, overlap)
		}
		for _, entry := range []struct {
			name    string
			payload []byte
		}{{"game.bin", innerLocal}, {"part.bin", innerPayload}} {
			opts := Options{Entry: entry.name}
			if err := Verify(ctx, "zip", bytes.NewReader(data), int64(len(data)), opts); (err != nil) != overlap {
				t.Errorf("Verify %s error = %v, overlap = %t", entry.name, err, overlap)
			}
			var output bytes.Buffer
			err := Decode(ctx, "zip", bytes.NewReader(data), int64(len(data)), &output, opts)
			if overlap && (err == nil || output.Len() != 0) {
				t.Errorf("decoded %d bytes from overlapping member %s: %v", output.Len(), entry.name, err)
			} else if !overlap && (err != nil || !bytes.Equal(output.Bytes(), entry.payload)) {
				t.Errorf("copied %d bytes from non-overlapping member %s: %v", output.Len(), entry.name, err)
			}
		}
	}
}

func TestZIPAcceptsClassicMaximumExpandedSizeInHeader(t *testing.T) {
	data := storedZIPFixture(t, []byte{0x03, 0x00}, nil)
	centralAt := int(binary.LittleEndian.Uint32(data[len(data)-6:]))
	binary.LittleEndian.PutUint16(data[8:], zip.Deflate)
	binary.LittleEndian.PutUint16(data[centralAt+10:], zip.Deflate)
	binary.LittleEndian.PutUint32(data[22:], 0xffffffff)
	binary.LittleEndian.PutUint32(data[centralAt+24:], 0xffffffff)
	// The header can express this exact ZIP32 size. Check the output limit
	// before attempting to verify a payload of that size.
	err := Verify(context.Background(), "zip", bytes.NewReader(data), int64(len(data)), Options{MaxOutput: 1024})
	if err == nil || !strings.Contains(err.Error(), "expanded size exceeds output limit") {
		t.Fatalf("expected the output limit to refuse the classic member size: %v", err)
	}
}

func storedZIPFixture(t *testing.T, payload, extra []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.CreateRaw(&zip.FileHeader{
		Name: "game.bin", Method: zip.Store, Extra: extra, CRC32: crc32.ChecksumIEEE(payload),
		CompressedSize64: uint64(len(payload)), UncompressedSize64: uint64(len(payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestZIPDirectoryPayloadValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		method  uint16
		payload []byte
		valid   bool
	}{
		{"stored empty", zip.Store, nil, true},
		{"deflated empty", zip.Deflate, []byte{0x03, 0x00}, true},
		{"stored data", zip.Store, []byte("x"), false},
		{"deflated data", zip.Deflate, []byte{0x01, 0x01, 0x00, 0xfe, 0xff, 'x'}, false},
		{"malformed deflate", zip.Deflate, []byte{0x07}, false},
		{"trailing data", zip.Deflate, []byte{0x03, 0x00, 'x'}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := zip.NewWriter(&archive)
			header := &zip.FileHeader{
				Name:             "dir",
				Method:           test.method,
				CompressedSize64: uint64(len(test.payload)),
			}
			header.SetMode(os.ModeDir | 0755)
			entry, err := writer.CreateRaw(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write(test.payload); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			err = Verify(context.Background(), "zip", bytes.NewReader(archive.Bytes()), int64(archive.Len()), Options{})
			if (err == nil) != test.valid {
				t.Fatalf("Verify error = %v, want success = %v", err, test.valid)
			}
		})
	}
}

func TestZIPRejectsInconsistentDataDescriptor(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"wrong compressed size", func(d []byte) []byte { d[8]++; return d }},
		{"wrong expanded size", func(d []byte) []byte { d[12]++; return d }},
		{"descriptor overlaps directory", func(d []byte) []byte { return d[:12] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded := encodeFixture(t, "zip", []byte("game data"))
			encoded = rewriteZIPFixtureDescriptor(encoded, test.mutate)
			if err := Verify(ctx, "zip", bytes.NewReader(encoded), int64(len(encoded)), Options{}); err == nil {
				t.Fatal("verified inconsistent ZIP data descriptor")
			}
			if err := Decode(ctx, "zip", bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{}); err == nil {
				t.Fatal("decoded ZIP with inconsistent data descriptor")
			}
		})
	}
}

func TestZIPRejectsDescriptorBorrowedFromNextLocalHeader(t *testing.T) {
	// These eight bytes have CRC32 0x04034b50, the local header signature.
	// Encode them in 20 bytes to match the next header's version, flags,
	// compression method, and time when read as a data descriptor.
	payload := []byte{'A', 'B', 'C', 'D', 0xd5, 0x70, 0x2d, 0xdf}
	compressed := append([]byte{0, 0, 0, 0xff, 0xff, 0, 8, 0, 0xf7, 0xff}, payload...)
	compressed = append(compressed, 3, 0)
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	first, err := writer.CreateRaw(&zip.FileHeader{
		Name: "first.bin", Flags: 8, Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(payload),
		CompressedSize64: uint64(len(compressed)), UncompressedSize64: uint64(len(payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write(compressed); err != nil {
		t.Fatal(err)
	}
	second, err := writer.CreateRaw(&zip.FileHeader{
		Name: "second.bin", ReaderVersion: 20, Method: zip.Deflate, CompressedSize64: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Write([]byte{3, 0}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data := output.Bytes()
	if err := Verify(ctx, "zip", bytes.NewReader(data), int64(len(data)), Options{}); err != nil {
		t.Fatalf("intact archive: %v", err)
	}
	const signedDescriptorSize = 16
	end := data[len(data)-22:]
	centralAt := int(binary.LittleEndian.Uint32(end[16:]))
	secondCentral := data[centralAt+46+len("first.bin"):]
	descriptorAt := int(binary.LittleEndian.Uint32(secondCentral[42:])) - signedDescriptorSize
	binary.LittleEndian.PutUint32(secondCentral[42:], uint32(descriptorAt))
	binary.LittleEndian.PutUint32(end[16:], uint32(centralAt-signedDescriptorSize))
	data = slices.Concat(data[:descriptorAt], data[descriptorAt+signedDescriptorSize:])
	err = Verify(ctx, "zip", bytes.NewReader(data), int64(len(data)), Options{})
	if err == nil || !strings.Contains(err.Error(), "data descriptor") {
		t.Fatalf("expected missing data descriptor error, got %v", err)
	}
}

func TestZIPSupportsUnsignedAndWideDataDescriptors(t *testing.T) {
	// Its actual CRC32 is 0x08074b50, which is also the optional descriptor signature.
	payload := []byte{0xac, 0x0a, 0x7a, 0xd5}
	for _, wide := range []bool{false, true} {
		var zipBytes bytes.Buffer
		writer := zip.NewWriter(&zipBytes)
		file, err := writer.CreateHeader(&zip.FileHeader{Name: "game.bin", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		encoded := rewriteZIPFixtureDescriptor(zipBytes.Bytes(), func(d []byte) []byte {
			if !wide {
				return d[4:]
			}
			descriptor := make([]byte, 24)
			copy(descriptor, d[:8])
			binary.LittleEndian.PutUint64(descriptor[8:], uint64(binary.LittleEndian.Uint32(d[8:])))
			binary.LittleEndian.PutUint64(descriptor[16:], uint64(binary.LittleEndian.Uint32(d[12:])))
			return descriptor
		})
		var restored bytes.Buffer
		if err := Decode(context.Background(), "zip", bytes.NewReader(encoded), int64(len(encoded)), &restored, Options{}); err != nil || !bytes.Equal(restored.Bytes(), payload) {
			t.Fatalf("expected ZIP data descriptor to decode, ZIP64 sizes=%t: %v", wide, err)
		}
	}
}

// encodeFixture creates a single-member ZIP with a signed 32-bit descriptor
// immediately before the directory and a classic end record without comment.
func rewriteZIPFixtureDescriptor(archive []byte, rewrite func([]byte) []byte) []byte {
	directory := int(binary.LittleEndian.Uint32(archive[len(archive)-6:]))
	start := directory - 16
	descriptor := rewrite(archive[start:directory])
	result := append(append(append([]byte(nil), archive[:start]...), descriptor...), archive[directory:]...)
	binary.LittleEndian.PutUint32(result[len(result)-6:], uint32(start+len(descriptor)))
	return result
}

func TestCancellation(t *testing.T) {
	data := bytes.Repeat([]byte{3}, 200000)
	for _, format := range []string{"zip", "gzip", "cso", "zso"} {
		encoded := encodeFixture(t, format, data)
		ctx, cancel := context.WithCancel(context.Background())
		err := Decode(ctx, format, bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{Progress: func(int64, int64) { cancel() }})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: %v", format, err)
		}
	}
}

func TestCSOInvalidIndex(t *testing.T) {
	encoded := encodeFixture(t, "cso", make([]byte, 4096))
	binary.LittleEndian.PutUint32(encoded[24:], 1)
	if err := Decode(context.Background(), "cso", bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{}); err == nil {
		t.Fatal("header overlap accepted")
	}
}

func TestCSOVersionZero(t *testing.T) {
	encoded := encodeFixture(t, "cso", make([]byte, 4096))
	encoded[20] = 0
	binary.LittleEndian.PutUint32(encoded[4:], 0)
	if err := Decode(context.Background(), "cso", bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{}); err != nil {
		t.Fatal(err)
	}
	encoded[20] = 2
	if err := Decode(context.Background(), "cso", bytes.NewReader(encoded), int64(len(encoded)), io.Discard, Options{}); err == nil {
		t.Fatal("CSO v2 incorrectly accepted")
	}
}

func TestAlignedBlocks(t *testing.T) {
	for _, format := range []string{"cso", "zso"} {
		original := encodeFixture(t, format, make([]byte, 4096))
		// Repack two compressed blocks at 16-byte boundaries with nonzero legal padding.
		header := append([]byte(nil), original[:36]...)
		header[21] = 4
		aligned := append(header, bytes.Repeat([]byte{0x71}, 12)...)
		for i := 0; i < 2; i++ {
			start := binary.LittleEndian.Uint32(original[24+i*4:])
			end := binary.LittleEndian.Uint32(original[28+i*4:])
			binary.LittleEndian.PutUint32(aligned[24+i*4:], uint32(len(aligned)/16))
			aligned = append(aligned, original[start:end]...)
			aligned = append(aligned, bytes.Repeat([]byte{0x71}, (-len(aligned))&15)...)
		}
		binary.LittleEndian.PutUint32(aligned[32:], uint32(len(aligned)/16))
		var got bytes.Buffer
		if err := Decode(context.Background(), format, bytes.NewReader(aligned), int64(len(aligned)), &got, Options{}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !bytes.Equal(got.Bytes(), make([]byte, 4096)) {
			t.Fatal("aligned data differs")
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, format := range []string{"zip", "gzip", "cso", "zso", "ecm"} {
		f.Add(format, encodeFixture(f, format, make([]byte, 4096)))
	}
	f.Add("ciso", []byte("CISO\x00\x00\x20\x00"))
	f.Add("wbfs", []byte("WBFS\x00\x00\x40\x00\x09\x15"))
	f.Add("wux", []byte("WUX0\x2e\xd0\x99\x10"))
	f.Add("cci", []byte("CCIM"))
	f.Fuzz(func(t *testing.T, format string, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		Decode(context.Background(), format, bytes.NewReader(data), int64(len(data)), io.Discard, Options{MaxOutput: 1 << 20})
	})
}

func BenchmarkEncodeCSO(b *testing.B) {
	data := bytes.Repeat([]byte("disc image payload"), 65536)
	f, err := os.Create(filepath.Join(b.TempDir(), "out"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Encode(context.Background(), "cso", bytes.NewReader(data), int64(len(data)), f, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMaxCSOInteroperability(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_MAXCSO")
	if tool == "" {
		t.Skip("set ROMCOMPRESSOR_MAXCSO to run independent codec interoperability")
	}
	data := make([]byte, 2<<20)
	rand.New(rand.NewSource(42)).Read(data)
	for i := 0; i < len(data); i += 4096 {
		clear(data[i : i+2048])
	}
	for _, format := range []string{"cso", "zso"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			original := filepath.Join(dir, "original.iso")
			if err := os.WriteFile(original, data, 0600); err != nil {
				t.Fatal(err)
			}
			ours := filepath.Join(dir, "ours."+format)
			if err := os.WriteFile(ours, encodeFixture(t, format, data), 0600); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(dir, "restored.iso")
			if output, err := exec.Command(tool, "--decompress", ours, "-o", restored).CombinedOutput(); err != nil {
				t.Fatalf("maxcso decode: %v: %s", err, output)
			}
			got, err := os.ReadFile(restored)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("maxcso decoded different data")
			}
			formatArg := "--format=" + format
			if format == "cso" {
				formatArg = "--format=cso1"
			}
			for _, block := range []string{"2048", "16384"} {
				theirs := filepath.Join(dir, "theirs-"+block+"."+format)
				if output, err := exec.Command(tool, "--fast", formatArg, "--block="+block, original, "-o", theirs).CombinedOutput(); err != nil {
					t.Fatalf("maxcso encode: %v: %s", err, output)
				}
				encoded, err := os.ReadFile(theirs)
				if err != nil {
					t.Fatal(err)
				}
				var decoded bytes.Buffer
				if err := Decode(context.Background(), format, bytes.NewReader(encoded), int64(len(encoded)), &decoded, Options{}); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(decoded.Bytes(), data) {
					t.Fatal("different payload from maxcso compressed image")
				}
			}
		})
	}
}

func TestVerifyZIPAllEntriesAndCombinedLimit(t *testing.T) {
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for _, name := range []string{"a.bin", "b.bin"} {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), "zip", bytes.NewReader(data.Bytes()), int64(data.Len()), Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), "zip", bytes.NewReader(data.Bytes()), int64(data.Len()), Options{MaxOutput: 150}); err == nil {
		t.Fatal("combined expansion limit ignored")
	}
	if err := Verify(context.Background(), "zip", bytes.NewReader(data.Bytes()), int64(data.Len()), Options{MaxOutput: 150, Entry: "a.bin"}); err != nil {
		t.Fatal(err)
	}
}

func TestZIPDirectoryCountCannotUnderstateAllocations(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	for _, name := range []string{"a", "b"} {
		if _, err := w.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	end := data[len(data)-22:]
	binary.LittleEndian.PutUint16(end[8:], 1)
	binary.LittleEndian.PutUint16(end[10:], 1)
	if _, err := OpenZIP(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("understated directory record count accepted")
	}
}

func TestZIPRejectsEndRecordHiddenInComment(t *testing.T) {
	data := encodeFixture(t, "zip", []byte("game data"))
	binary.LittleEndian.PutUint16(data[len(data)-2:], 23)
	var comment [23]byte
	binary.LittleEndian.PutUint32(comment[:], 0x06054b50)
	data = append(data, comment[:]...)
	if _, err := OpenZIP(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("an alternate end record could redirect archive parsing")
	}
}

func TestZIPRejectsRecordsBeyondDeclaredDirectory(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	for _, name := range []string{"a", "b"} {
		if _, err := w.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	end := data[len(data)-22:]
	binary.LittleEndian.PutUint16(end[8:], 1)
	binary.LittleEndian.PutUint16(end[10:], 1)
	binary.LittleEndian.PutUint32(end[12:], 47)
	if _, err := OpenZIP(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("central directory records beyond the declared extent were accepted")
	}
}

func TestZIPDirectoryCancellationBetweenReads(t *testing.T) {
	data := encodeFixture(t, "zip", []byte("game data"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancellingReaderAt{ReaderAt: bytes.NewReader(data), cancel: cancel, cancelAfter: 1}
	if _, err := OpenZIP(ctx, reader, int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation during ZIP metadata scan, got %v", err)
	}
}

type cancellingReaderAt struct {
	io.ReaderAt
	cancel      context.CancelFunc
	cancelAfter int
	reads       int
}

func (reader *cancellingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	n, err := reader.ReaderAt.ReadAt(p, offset)
	reader.reads++
	if reader.reads == reader.cancelAfter {
		reader.cancel()
	}
	return n, err
}

func TestZIPDirectoryExactly65535MembersWithoutZIP64(t *testing.T) {
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for i := range 65535 {
		if _, err := w.CreateRaw(&zip.FileHeader{Name: strconv.Itoa(i) + "/"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	archive := data.Bytes()
	wideAt := len(archive) - 98 // ZIP64 end record, locator, and classic end record.
	wide := archive[wideAt:]
	end := archive[len(archive)-22:]
	binary.LittleEndian.PutUint32(end[12:], uint32(binary.LittleEndian.Uint64(wide[40:])))
	binary.LittleEndian.PutUint32(end[16:], uint32(binary.LittleEndian.Uint64(wide[48:])))
	copy(wide, end)
	archive = archive[:wideAt+22]
	r, err := OpenZIP(context.Background(), bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.File) != 65535 {
		t.Fatalf("read %d members, want 65,535", len(r.File))
	}
}

func TestZIPDirectoryExactly65535Bytes(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	entry, err := w.CreateHeader(&zip.FileHeader{Name: "a", Comment: strings.Repeat("a", 65535-46-1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("input")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	endAt := len(data) - 22
	end := data[endAt:]
	if size := binary.LittleEndian.Uint32(end[12:]); size != 65535 {
		t.Fatalf("unexpected central directory size %d", size)
	}
	if err := Verify(context.Background(), "zip", bytes.NewReader(data), int64(len(data)), Options{}); err != nil {
		t.Fatal(err)
	}

	// A locator can redirect archive/zip when the ordinary directory uses this size.
	var wide [76]byte
	binary.LittleEndian.PutUint32(wide[:], 0x06064b50)
	binary.LittleEndian.PutUint64(wide[4:], 44)
	binary.LittleEndian.PutUint64(wide[48:], uint64(endAt))
	binary.LittleEndian.PutUint32(wide[56:], 0x07064b50)
	binary.LittleEndian.PutUint64(wide[64:], uint64(endAt))
	binary.LittleEndian.PutUint32(wide[72:], 1)
	withLocator := append([]byte(nil), data[:endAt]...)
	withLocator = append(withLocator, wide[:]...)
	withLocator = append(withLocator, end...)
	if _, err := OpenZIP(context.Background(), bytes.NewReader(withLocator), int64(len(withLocator))); err == nil {
		t.Fatal("ZIP64 locator redirected the parser to an empty directory")
	}

	// With matching bounds and ZIP64 sentinels, the same archive is valid ZIP64.
	binary.LittleEndian.PutUint64(withLocator[endAt+24:], 1)
	binary.LittleEndian.PutUint64(withLocator[endAt+32:], 1)
	binary.LittleEndian.PutUint64(withLocator[endAt+40:], 65535)
	binary.LittleEndian.PutUint64(withLocator[endAt+48:], uint64(binary.LittleEndian.Uint32(end[16:])))
	wideEnd := withLocator[len(withLocator)-22:]
	binary.LittleEndian.PutUint16(wideEnd[8:], 0xffff)
	binary.LittleEndian.PutUint16(wideEnd[10:], 0xffff)
	binary.LittleEndian.PutUint32(wideEnd[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(wideEnd[16:], 0xffffffff)
	if err := Verify(context.Background(), "zip", bytes.NewReader(withLocator), int64(len(withLocator)), Options{}); err != nil {
		t.Fatal(err)
	}
}
