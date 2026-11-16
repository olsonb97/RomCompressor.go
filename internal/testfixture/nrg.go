package testfixture

import (
	"bytes"
	"encoding/binary"
)

// NRG wraps cooked sectors in one Nero DAOX track with explicit index offsets.
func NRG(sectors []byte, index0, index1 uint64) []byte {
	var trackTable [22 + 42]byte
	trackTable[20], trackTable[21] = 1, 1
	binary.BigEndian.PutUint16(trackTable[22+12:], 2048)
	binary.BigEndian.PutUint64(trackTable[22+18:], index0)
	binary.BigEndian.PutUint64(trackTable[22+26:], index1)
	binary.BigEndian.PutUint64(trackTable[22+34:], uint64(len(sectors)))
	var metadata bytes.Buffer
	metadata.WriteString("DAOX")
	metadata.Write(binary.BigEndian.AppendUint32(nil, uint32(len(trackTable))))
	metadata.Write(trackTable[:])
	metadata.WriteString("SINF\x00\x00\x00\x04\x00\x00\x00\x01")
	metadata.WriteString("END!\x00\x00\x00\x00NER5")
	metadata.Write(binary.BigEndian.AppendUint64(nil, uint64(len(sectors))))
	return append(bytes.Clone(sectors), metadata.Bytes()...)
}
