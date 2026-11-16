// Package testfixture creates synthetic data for API and CLI tests.
package testfixture

import (
	"encoding/binary"
)

const (
	CARD1MainOffset   = 0x4000
	CARD1UpdateOffset = 0x8000
	CARD1TrimmedSize  = 0xa000
)

// CARD1 returns a generated 3DS cartridge with an executable, an update
// partition, and a uniform FF trailer. It contains no game content.
func CARD1() []byte {
	const mainID, updateID = 0x0004000000123400, 0x000400db00016202
	data := make([]byte, 0xb000)
	for i := CARD1TrimmedSize; i < len(data); i++ {
		data[i] = 0xff
	}
	copy(data[0x100:], "NCSD")
	binary.LittleEndian.PutUint32(data[0x104:], (512<<20)/0x200)
	binary.LittleEndian.PutUint64(data[0x108:], mainID)
	data[0x18d] = 1
	binary.LittleEndian.PutUint32(data[0x200:], 0xffffffff)
	main := card1Partition(data, 0, CARD1MainOffset, 0x3000, 0x3000, mainID)
	binary.LittleEndian.PutUint32(main[0x180:], 0x400)
	main[0x18d] = 2
	for at, word := range map[int]uint32{
		0x190: 5, 0x194: 1, 0x198: 6, 0x19c: 1,
		0x1a0: 7, 0x1a4: 4, 0x1a8: 1,
		0x1b0: 11, 0x1b4: 12, 0x1b8: 1,
	} {
		binary.LittleEndian.PutUint32(main[at:], word)
	}
	// The NCCH is shorter than its enclosing partition. The final
	// bytes declared by NCSD are still part of the retained output.
	update := card1Partition(data, 7, CARD1UpdateOffset, 0x2000, 0x1800, updateID)
	update[0x18d] = 1
	binary.LittleEndian.PutUint32(update[0x1b0:], 1)
	binary.LittleEndian.PutUint32(update[0x1b4:], 11)
	binary.LittleEndian.PutUint32(update[0x1b8:], 1)
	data[CARD1TrimmedSize-1] = 0x52
	return data
}

func card1Partition(data []byte, slot int, start, length, content uint32, id uint64) []byte {
	binary.LittleEndian.PutUint32(data[0x120+8*slot:], start/0x200)
	binary.LittleEndian.PutUint32(data[0x124+8*slot:], length/0x200)
	binary.LittleEndian.PutUint64(data[0x190+8*slot:], id)
	header := data[start : start+0x200]
	copy(header[0x100:], "NCCH")
	binary.LittleEndian.PutUint32(header[0x104:], content/0x200)
	binary.LittleEndian.PutUint64(header[0x108:], id)
	return header
}
