package codec

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestCSOAndZSOAcceptPaddedFinalBlock(t *testing.T) {
	const blockSize = 16 << 10
	logical := bytes.Repeat([]byte{0x13, 0x61, 0x62, 0x63}, 3*2048/4)
	for _, format := range []string{"cso", "zso"} {
		for _, plain := range []bool{false, true} {
			block := make([]byte, blockSize)
			copy(block, logical)
			archive := paddedCSOFixture(t, format, int64(len(logical)), block, plain)
			var decoded bytes.Buffer
			if err := Decode(context.Background(), format, bytes.NewReader(archive), int64(len(archive)), &decoded, Options{}); err != nil {
				t.Errorf("%s (stored raw %t): %v", format, plain, err)
			} else if !bytes.Equal(decoded.Bytes(), logical) {
				t.Errorf("%s (stored raw %t) changed the logical source bytes", format, plain)
			}
			block[len(block)-1] = 1
			bad := paddedCSOFixture(t, format, int64(len(logical)), block, plain)
			if err := Verify(context.Background(), format, bytes.NewReader(bad), int64(len(bad)), Options{}); err == nil {
				t.Errorf("%s (stored raw %t) ignored data after the declared logical end", format, plain)
			}
		}
	}
}

func TestRawFinalBlockRetainsExistingAlignmentRules(t *testing.T) {
	const align = 4096
	logical := bytes.Repeat([]byte{4}, 1900)
	for _, format := range []string{"cso", "zso"} {
		archive := paddedCSOFixture(t, format, int64(len(logical)), make([]byte, 2048), true)[:32]
		archive[21] = 12
		binary.LittleEndian.PutUint32(archive[24:28], 0x80000001)
		binary.LittleEndian.PutUint32(archive[28:32], 2)
		archive = append(archive, bytes.Repeat([]byte{0x71}, align*2-len(archive))...)
		copy(archive[align:], logical)
		var decoded bytes.Buffer
		if err := Decode(context.Background(), format, bytes.NewReader(archive), int64(len(archive)), &decoded, Options{}); err != nil || !bytes.Equal(decoded.Bytes(), logical) {
			t.Errorf("%s: nonzero index alignment bytes changed the logical image: %v", format, err)
		}
	}
}

func TestZSORecognizesFullBlockEvenWhenShortPrefixDecodes(t *testing.T) {
	const alignment = 64
	const logical = 16
	encoded := append([]byte{0xf0, 1}, bytes.Repeat([]byte{0x41}, logical)...)
	// The rest of the full frame begins with a four-byte match, another
	// overlapping match, and five final literals. The first 16 bytes also
	// look like a complete literal-only block if the matches are ignored.
	encoded = append(encoded, 1, 0, 0x0f, 1, 0, 255, 255, 255, 255, 255, 255, 255, 219, 0x50, 0, 0, 0, 0, 0)
	encoded = append(encoded, make([]byte, alignment-len(encoded))...)
	out := make([]byte, 2048)
	if err := decodeZSOBlock(encoded, out, logical, alignment); err == nil {
		t.Fatal("full frame's nonzero data was hidden behind a valid short prefix")
	}
	encoded[1+logical] = 0
	if err := decodeZSOBlock(encoded, out, logical, alignment); err != nil {
		t.Fatalf("valid full frame with zero padding rejected: %v", err)
	}
}

func TestDiscLZ4RejectsInvalidTerminalSequence(t *testing.T) {
	for _, test := range []struct {
		name   string
		packed []byte
	}{
		{"one final literal", []byte{0x1f, 0x41, 1, 0, 255, 255, 255, 255, 255, 255, 255, 242, 0x10, 0x42}},
		{"last match starts 11 bytes from end", []byte{0x1f, 0x41, 1, 0, 255, 255, 255, 255, 255, 255, 255, 232, 0, 1, 0, 0x70, 1, 2, 3, 4, 5, 6, 7}},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := make([]byte, 2048)
			if err := decodeZSOBlock(test.packed, out, len(out), 1); err == nil {
				t.Error("ZSO accepted malformed LZ4 framing")
			}
			cci := append([]byte{byte((-(len(test.packed) + 1)) & 3)}, test.packed...)
			cci = append(cci, make([]byte, cci[0])...)
			entries := binary.LittleEndian.AppendUint32(nil, cciCompressed)
			entries = binary.LittleEndian.AppendUint32(entries, uint32(len(cci)/4))
			if err := readCCISector(bytes.NewReader(cci), entries, make([]byte, cciSectorBytes), out); err == nil {
				t.Error("CCI accepted malformed LZ4 framing")
			}
		})
	}
}

func TestZSOSmallLiteralOnlyBlocks(t *testing.T) {
	for length := 1; length < 5; length++ {
		payload := bytes.Repeat([]byte{0x38}, length)
		encoded := append([]byte{byte(length << 4)}, payload...)
		out := make([]byte, 2048)
		if err := decodeZSOBlock(encoded, out, length, 1); err != nil || !bytes.Equal(out[:length], payload) {
			t.Errorf("%d-byte literal block: %v", length, err)
		}
	}
}

func TestCCIChecksOutputLimitBeforeReadingIndex(t *testing.T) {
	var header [cciHeaderBytes]byte
	copy(header[:4], "CCIM")
	binary.LittleEndian.PutUint32(header[4:8], cciHeaderBytes)
	binary.LittleEndian.PutUint64(header[8:16], cciMaxImage)
	binary.LittleEndian.PutUint64(header[16:24], cciHeaderBytes+4)
	binary.LittleEndian.PutUint32(header[24:28], cciSectorBytes)
	header[28], header[29] = 1, 2
	err := Decode(context.Background(), "cci", bytes.NewReader(header[:]), int64(len(header)), io.Discard, Options{MaxOutput: cciSectorBytes})
	if err == nil || !strings.Contains(err.Error(), "expanded size limit") {
		t.Fatalf("expected size rejection before index access, got %v", err)
	}
}

func paddedCSOFixture(t *testing.T, format string, logical int64, block []byte, plain bool) []byte {
	t.Helper()
	header := make([]byte, csoHeaderSize+8)
	copy(header, "CISO")
	if format == "zso" {
		copy(header, "ZISO")
	}
	binary.LittleEndian.PutUint32(header[4:8], csoHeaderSize)
	binary.LittleEndian.PutUint64(header[8:16], uint64(logical))
	binary.LittleEndian.PutUint32(header[16:20], uint32(len(block)))
	header[20] = 1
	entry := uint32(len(header))
	var payload []byte
	switch {
	case plain:
		entry |= 1 << 31
		payload = block
	case format == "cso":
		var output bytes.Buffer
		writer, err := flate.NewWriter(&output, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(block); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		payload = output.Bytes()
	default:
		payload = make([]byte, lz4.CompressBlockBound(len(block)))
		var compressor lz4.Compressor
		n, err := compressor.CompressBlock(block, payload)
		if err != nil || n == 0 {
			t.Fatalf("could not encode a compressible fixture: %d bytes, %v", n, err)
		}
		payload = payload[:n]
	}
	binary.LittleEndian.PutUint32(header[24:28], entry)
	binary.LittleEndian.PutUint32(header[28:32], uint32(len(header)+len(payload)))
	return append(header, payload...)
}
