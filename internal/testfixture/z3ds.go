package testfixture

import (
	"encoding/binary"
)

// Z3DS builds a synthetic container for a four-byte NCSD, NCCH, CIA, or 3DSX magic.
func Z3DS(magic string) []byte {
	// A single-segment Zstandard frame with a 1024-byte RLE block.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x60, 0x00, 0x03, 0x03, 0x20, 0x00, 0x5a}
	return Z3DSWithFrame(magic, frame, nil)
}

// Z3DSWithFrame builds a synthetic container with explicit frame and metadata bytes.
func Z3DSWithFrame(magic string, frame, metadata []byte) []byte {
	b := make([]byte, 32+len(metadata)+len(frame)+25)
	copy(b, "Z3DS")
	copy(b[4:], magic)
	b[8] = 1
	binary.LittleEndian.PutUint16(b[10:], 32)
	binary.LittleEndian.PutUint32(b[12:], uint32(len(metadata)))
	binary.LittleEndian.PutUint64(b[16:], uint64(len(frame)+25))
	binary.LittleEndian.PutUint64(b[24:], 1024)
	copy(b[32:], metadata)
	copy(b[32+len(metadata):], frame)
	table := b[len(b)-25:]
	binary.LittleEndian.PutUint32(table, 0x184d2a5e)
	binary.LittleEndian.PutUint32(table[4:], 17)
	binary.LittleEndian.PutUint32(table[8:], uint32(len(frame)))
	binary.LittleEndian.PutUint32(table[12:], 1024)
	binary.LittleEndian.PutUint32(table[16:], 1)
	binary.LittleEndian.PutUint32(table[21:], 0x8f92eab1)
	return b
}
