package trim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	ncsdMediaUnit       = 0x200
	ncsdUsedSizeOffset  = 0x300
	ncsdCardInfoEnd     = ncsdUsedSizeOffset + 8
	ncsdFirstContent    = 0x4000
	ncchHeaderSize      = 0x200
	ncchExtHeaderSize   = 0x400
	ncchExtHeaderOnCard = 0x800
)

// N3DSBoundary validates a standard CARD1 cartridge and returns the end of
// its used data. The caller verifies the entire suffix for padding.
func N3DSBoundary(src io.ReaderAt, size int64) (int64, error) {
	if size < ncsdFirstContent || size%ncsdMediaUnit != 0 {
		return 0, errors.New("3DS cartridge has an incomplete sector layout")
	}
	var header [ncsdCardInfoEnd]byte
	if _, err := src.ReadAt(header[:], 0); err != nil {
		return 0, fmt.Errorf("read 3DS cartridge header: %w", err)
	}
	if err := validateNCSDCard(header[:], size); err != nil {
		return 0, err
	}
	end := int64(ncsdFirstContent)
	for slot := range 8 {
		entry := 0x120 + 8*slot
		start := n3dsSectors(header[:], entry)
		length := n3dsSectors(header[:], entry+4)
		id := binary.LittleEndian.Uint64(header[0x190+8*slot:])
		if length == 0 {
			if slot == 0 || start != 0 || id != 0 {
				return 0, fmt.Errorf("incomplete 3DS partition %d", slot)
			}
			continue
		}
		if start < end || start > size || length > size-start {
			return 0, fmt.Errorf("3DS partition %d crosses another partition or the end of the image", slot)
		}
		if err := validateNCCH(src, start, length, id, slot == 0); err != nil {
			return 0, fmt.Errorf("3DS partition %d: %w", slot, err)
		}
		end = start + length
	}
	// Mastering metadata can also declare a used byte count. Honor both
	// boundaries and keep the entire sector containing the last used byte.
	used := binary.LittleEndian.Uint64(header[ncsdUsedSizeOffset:])
	if used > uint64(size) {
		return 0, errors.New("3DS used size exceeds the image length")
	}
	usedEnd := (int64(used) + ncsdMediaUnit - 1) / ncsdMediaUnit * ncsdMediaUnit
	return max(end, usedEnd), nil
}

func validateNCSDCard(header []byte, size int64) error {
	if string(header[0x100:0x104]) != "NCSD" ||
		binary.LittleEndian.Uint64(header[0x108:]) == 0 ||
		binary.LittleEndian.Uint64(header[0x110:]) != 0 {
		return errors.New("invalid 3DS cartridge NCSD header")
	}
	// CARD2 can use the end of the image for save storage. Refuse any
	// writable offset even if the media type claims to be CARD1.
	if header[0x18d] != 1 || binary.LittleEndian.Uint32(header[0x200:]) != 0xffffffff {
		return errors.New("3DS trimming requires a CARD1 cartridge with no writable region")
	}
	if header[0x18e] != 0 {
		return errors.New("unsupported 3DS cartridge media unit")
	}
	capacity := n3dsSectors(header, 0x104)
	if capacity < 128<<20 || capacity > 8<<30 || capacity&(capacity-1) != 0 || size > capacity {
		return errors.New("invalid 3DS cartridge capacity")
	}
	return nil
}

func validateNCCH(src io.ReaderAt, start, length int64, id uint64, executable bool) error {
	var header [ncchHeaderSize]byte
	if _, err := src.ReadAt(header[:], start); err != nil {
		return fmt.Errorf("read NCCH header: %w", err)
	}
	actualID := binary.LittleEndian.Uint64(header[0x108:])
	if string(header[0x100:0x104]) != "NCCH" || actualID == 0 || (id != 0 && id != actualID) {
		return errors.New("invalid NCCH signature or partition ID")
	}
	if header[0x18e] != 0 {
		return errors.New("unsupported NCCH media unit")
	}
	size := n3dsSectors(header[:], 0x104)
	if size < int64(len(header)) || size > length {
		return errors.New("NCCH length exceeds its partition")
	}
	extHeader := binary.LittleEndian.Uint32(header[0x180:])
	exeFS := n3dsSectors(header[:], 0x1a4)
	if executable && (header[0x18d]&2 == 0 || extHeader != ncchExtHeaderSize || exeFS == 0) {
		return errors.New("main partition lacks a cartridge executable")
	}
	minimum := int64(len(header))
	if extHeader != 0 {
		if extHeader != ncchExtHeaderSize {
			return errors.New("unsupported NCCH extended header size")
		}
		minimum += ncchExtHeaderOnCard
	}
	if minimum > size {
		return errors.New("NCCH extended header exceeds its content")
	}
	if n3dsSectors(header[:], 0x1a8) > exeFS || n3dsSectors(header[:], 0x1b8) > n3dsSectors(header[:], 0x1b4) {
		return errors.New("NCCH hash region exceeds its file system")
	}
	return validateNCCHSections(header[:], minimum, size)
}

type ncchSpan struct {
	start, end int64
}

func validateNCCHSections(header []byte, minimum, size int64) error {
	sections := [...]struct {
		name string
		at   int
	}{
		{"plain region", 0x190},
		{"logo", 0x198},
		{"ExeFS", 0x1a0},
		{"RomFS", 0x1b0},
	}
	var seen [len(sections)]ncchSpan
	for i, section := range sections {
		start, count := n3dsSectors(header, section.at), n3dsSectors(header, section.at+4)
		if start == 0 && count == 0 {
			continue
		}
		if count == 0 || start < minimum || start > size || count > size-start {
			return fmt.Errorf("invalid NCCH %s extent", section.name)
		}
		end := start + count
		for _, earlier := range seen[:i] {
			if start < earlier.end && earlier.start < end {
				return fmt.Errorf("NCCH %s overlaps another section", section.name)
			}
		}
		seen[i] = ncchSpan{start: start, end: end}
	}
	return nil
}

func n3dsSectors(header []byte, offset int) int64 {
	return int64(binary.LittleEndian.Uint32(header[offset:])) * ncsdMediaUnit
}
