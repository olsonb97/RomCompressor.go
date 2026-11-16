package testfixture

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"testing"
)

// PBP builds a one-track disc with a compressed block and a raw final block.
func PBP(t testing.TB) (packed, raw []byte) {
	t.Helper()
	const psar, payloadOffset = 0x80, 0x100000
	const sectorSize, blockSize = 2352, 16 * 2352
	raw = make([]byte, 17*sectorSize)
	for i := range raw {
		raw[i] = byte(i / 21)
	}
	copy(raw, "\x00\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\x00")
	raw[15] = 2
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(raw[:blockSize]); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	packed = make([]byte, psar+payloadOffset+compressed.Len()+blockSize)
	copy(packed, "\x00PBP")
	for p := 8; p < 40; p += 4 {
		binary.LittleEndian.PutUint32(packed[p:], psar)
	}
	copy(packed[psar:], "PSISOIMG0000")
	binary.LittleEndian.PutUint32(packed[psar+0xbfc:], payloadOffset)
	toc := packed[psar+0x800:]
	toc[2], toc[7], toc[12], toc[17], toc[22] = 0xa0, 1, 0xa1, 1, 0xa2
	toc[29] = 0x17                         // 17 frames in BCD.
	toc[30], toc[32], toc[38] = 0x41, 1, 2 // Track 1 begins after a two-second lead-in.
	index := packed[psar+0x4000:]
	binary.LittleEndian.PutUint16(index[4:], uint16(compressed.Len()))
	binary.LittleEndian.PutUint16(index[6:], 1)
	binary.LittleEndian.PutUint32(index[32:], uint32(compressed.Len()))
	binary.LittleEndian.PutUint32(index[36:], blockSize)
	copy(packed[psar+payloadOffset:], compressed.Bytes())
	copy(packed[psar+payloadOffset+compressed.Len():], raw[blockSize:])
	return packed, raw
}
