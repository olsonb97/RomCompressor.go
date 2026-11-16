package trim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	ndsHeaderReadSize = 0x240
	ndsSecureAreaEnd  = 0x8000
	twlARM9iBootSize  = 0x4000
	ndsDownloadAuth   = 0x88
	ndsAuthPointer    = 0x1000
	ndsLogoCRC        = 0xcf56
	ndsARM9FooterSize = 12
	ndsOverlaySize    = 32
)

// HasNDSHeader recognizes DS/DSi header and logo checksums. Recognition does not
// validate the cartridge layout or establish that trimming is safe.
func HasNDSHeader(header []byte) bool {
	return len(header) >= 0x160 &&
		binary.LittleEndian.Uint16(header[0x15c:]) == ndsLogoCRC &&
		ndsCRC16(header[0xc0:0x15c]) == ndsLogoCRC &&
		binary.LittleEndian.Uint16(header[0x15e:]) == ndsCRC16(header[:0x15e])
}

// NDSBoundary validates a DS or DSi cartridge layout and returns the prefix
// to retain. The caller checks the entire discarded suffix for padding.
func NDSBoundary(ctx context.Context, src io.ReaderAt, size int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var header [ndsHeaderReadSize]byte
	if size < ndsSecureAreaEnd {
		return 0, errors.New("DS cartridge image is shorter than its boot region")
	}
	if _, err := src.ReadAt(header[:], 0); err != nil {
		return 0, fmt.Errorf("read DS header: %w", err)
	}
	if err := validateNDSHeader(header[:]); err != nil {
		return 0, err
	}
	capacity := int64(128*1024) << header[0x14]
	headerEnd := ndsWord(header[:], 0x84)
	ntrEnd := ndsWord(header[:], 0x80)
	if size > capacity || ntrEnd > size || ntrEnd < ndsSecureAreaEnd || ntrEnd%4 != 0 ||
		headerEnd < 0x200 || headerEnd > 0x4000 || headerEnd%4 != 0 {
		return 0, errors.New("invalid DS cartridge size or header extent")
	}
	if err := validateNTRSections(header[:], headerEnd, ntrEnd); err != nil {
		return 0, err
	}
	keep := ntrEnd
	if header[0x12] != 0 {
		keep = ndsWord(header[:], 0x210)
		if err := validateTWLSections(header[:], headerEnd, ntrEnd, keep, size); err != nil {
			return 0, err
		}
	}
	if err := validateNDSBanner(src, header[:], headerEnd, keep); err != nil {
		return 0, err
	}
	overlayAuthEnd, err := validateNDSFAT(ctx, src, header[:], headerEnd, keep)
	if err != nil {
		return 0, err
	}
	if err := validateNDSFileNames(ctx, src, header[:]); err != nil {
		return 0, err
	}
	if err := validateNDSOverlays(ctx, src, header[:]); err != nil {
		return 0, err
	}
	if header[0x12] == 0 {
		keep, err = keepNDSDownloadAuth(src, keep, size)
		if err != nil {
			return 0, err
		}
		keep = max(keep, min(size, overlayAuthEnd))
	}
	arm9End := ndsWord(header[:], 0x20) + ndsWord(header[:], 0x2c)
	return keepNDSARM9Footers(ctx, src, arm9End, keep, size)
}

func validateNDSHeader(header []byte) error {
	unit := header[0x12]
	if unit != 0 && unit != 2 && unit != 3 {
		return errors.New("unsupported DS cartridge unit code")
	}
	if header[0x14] >= 15 {
		return errors.New("invalid DS cartridge capacity")
	}
	for _, b := range header[0x0c:0x10] {
		if (b < 'A' || b > 'Z') && (b < '0' || b > '9') {
			return errors.New("invalid DS cartridge game code")
		}
	}
	if !HasNDSHeader(header) {
		return errors.New("invalid DS logo or header checksum")
	}
	// NAND storage may look like padding. The Japanese Band Brothers DX
	// cartridge (AXBJ) has NAND even though its extent fields are zero.
	if ndsWord(header, 0x94) != 0 || string(header[0x0c:0x10]) == "AXBJ" {
		return errors.New("DS cartridge has a writable NAND region")
	}
	return nil
}

type ndsSection struct {
	name             string
	offsetAt, sizeAt int
	sizeAlignment    int64
	required         bool
}

func validateNTRSections(header []byte, headerEnd, ntrEnd int64) error {
	sections := [...]ndsSection{
		{"ARM9", 0x20, 0x2c, 4, true},
		{"ARM7", 0x30, 0x3c, 4, true},
		{"file name table", 0x40, 0x44, 1, false},
		{"file allocation table", 0x48, 0x4c, 8, false},
		{"ARM9 overlays", 0x50, 0x54, ndsOverlaySize, false},
		{"ARM7 overlays", 0x58, 0x5c, ndsOverlaySize, false},
		{"debug ROM", 0x160, 0x164, 1, false},
	}
	for _, section := range sections {
		if err := section.validate(header, headerEnd, ntrEnd); err != nil {
			return err
		}
	}
	arm9, arm7 := ndsWord(header, 0x20), ndsWord(header, 0x30)
	if arm9 < 0x4000 || arm9%0x1000 != 0 || arm7 < ndsSecureAreaEnd {
		return errors.New("ARM binaries do not match the DS cartridge boot layout")
	}
	if arm9 < arm7+ndsWord(header, 0x3c) && arm7 < arm9+ndsWord(header, 0x2c) {
		return errors.New("DS ARM9 and ARM7 binaries overlap")
	}
	fntSize := ndsWord(header, 0x44)
	if fntSize != 0 && fntSize < 8 {
		return errors.New("DS file name table is too short for its root entry")
	}
	return nil
}

func validateTWLSections(header []byte, headerEnd, ntrEnd, total, size int64) error {
	const twlHeaderEnd = 0x1000
	if headerEnd < twlHeaderEnd || total <= ntrEnd || total > size || total%4 != 0 {
		return errors.New("invalid or missing DSi cartridge extent")
	}
	// The high title ID selects cartridge images, and the low title ID
	// holds the reversed game code. DSiWare may share the header layout.
	if ndsWord(header, 0x234) != 0x00030000 ||
		binary.BigEndian.Uint32(header[0x0c:]) != binary.LittleEndian.Uint32(header[0x230:]) {
		return errors.New("DSi header does not identify a cartridge title")
	}
	binaries := [...]ndsSection{
		{"ARM9i", 0x1c0, 0x1cc, 4, false},
		{"ARM7i", 0x1d0, 0x1dc, 4, false},
	}
	if ndsWord(header, 0x1cc) == 0 && ndsWord(header, 0x1dc) == 0 {
		return errors.New("DSi cartridge has no declared DSi executable data")
	}
	for _, section := range binaries {
		if err := section.validate(header, ntrEnd, total); err != nil {
			return err
		}
	}
	// The cartridge exposes a fixed ARM9i boot window even when the
	// declared executable is smaller. GodMode9 reads the whole window.
	arm9i := ndsWord(header, 0x1c0)
	if arm9i != 0 && !ndsSpanFits(arm9i, twlARM9iBootSize, ntrEnd, total) {
		return errors.New("DSi ARM9i boot area crosses the cartridge extent")
	}
	sections := [...]ndsSection{
		{"NTR digest region", 0x1e0, 0x1e4, 1, false},
		{"TWL digest region", 0x1e8, 0x1ec, 1, false},
		{"digest sector hash table", 0x1f0, 0x1f4, 1, false},
		{"digest block hash table", 0x1f8, 0x1fc, 1, false},
		{"first modcrypt region", 0x220, 0x224, 1, false},
		{"second modcrypt region", 0x228, 0x22c, 1, false},
	}
	for _, section := range sections {
		if err := section.validate(header, headerEnd, total); err != nil {
			return err
		}
	}
	// Block authentication reads complete groups of SHA-1 digests from
	// the sector table, including bytes past its recorded length.
	const digestSize = 20
	sectorTableAt := ndsWord(header, 0x1f0)
	blockCount := ndsWord(header, 0x1fc) / digestSize
	blockBytes := ndsWord(header, 0x204) * digestSize
	if blockCount != 0 && blockBytes != 0 && (sectorTableAt == 0 || blockCount > (total-sectorTableAt)/blockBytes) {
		return errors.New("DSi block digest input crosses the cartridge extent or lacks a sector hash table")
	}
	if int64(binary.LittleEndian.Uint16(header[0x90:]))*0x80000 > total ||
		int64(binary.LittleEndian.Uint16(header[0x92:]))*0x80000 > total {
		return errors.New("DSi region declaration crosses the cartridge extent")
	}
	return nil
}

func (s ndsSection) validate(header []byte, minimum, maximum int64) error {
	offset, size := ndsWord(header, s.offsetAt), ndsWord(header, s.sizeAt)
	if offset == 0 && size == 0 && !s.required {
		return nil
	}
	if size == 0 || !ndsSpanFits(offset, size, minimum, maximum) || size%s.sizeAlignment != 0 {
		return fmt.Errorf("invalid DS %s extent", s.name)
	}
	return nil
}

func validateNDSBanner(src io.ReaderAt, header []byte, minimum, maximum int64) error {
	offset := ndsWord(header, 0x68)
	var declared int64
	if header[0x12] != 0 {
		declared = ndsWord(header, 0x208)
	}
	if offset == 0 && declared == 0 {
		return nil
	}
	var version [2]byte
	minimum = max(minimum, ndsSecureAreaEnd)
	if !ndsSpanFits(offset, int64(len(version)), minimum, maximum) {
		return errors.New("DS banner points outside the cartridge")
	}
	if _, err := src.ReadAt(version[:], offset); err != nil {
		return fmt.Errorf("read DS banner version: %w", err)
	}
	var size int64
	switch binary.LittleEndian.Uint16(version[:]) {
	case 1:
		size = 0x840
	case 2:
		size = 0x940
	case 3:
		size = 0xa40
	case 0x103:
		size = 0x23c0
	default:
		return errors.New("unsupported DS banner version")
	}
	if (declared != 0 && declared < size) || !ndsSpanFits(offset, max(size, declared), minimum, maximum) {
		return errors.New("DS banner extends past the cartridge boundary")
	}
	return nil
}

func validateNDSFAT(ctx context.Context, src io.ReaderAt, header []byte, minimum, maximum int64) (int64, error) {
	offset, size := ndsWord(header, 0x48), ndsWord(header, 0x4c)
	const maxFATSize = (1 << 16) * 8 // NitroFS uses 16-bit file IDs.
	if size > maxFATSize {
		return 0, errors.New("DS file allocation table exceeds the file ID range")
	}
	// DSi firmware authenticates classic DS ARM9 overlays through its
	// whitelist or the late DS header. Both use the leading FAT entries and
	// include sector padding, with a shared 512 KiB budget (Ekona phase 2).
	const authSectorSize = 0x200
	authSectorsLeft := int64(1024)
	var authFiles, authEnd int64
	if header[0x12] == 0 {
		authFiles = ndsWord(header, 0x54) / ndsOverlaySize
		if authFiles > size/8 {
			return 0, errors.New("DS overlay authentication exceeds the file allocation table")
		}
	}
	var block [4096]byte
	for consumed := int64(0); consumed < size; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		part := block[:min(int64(len(block)), size-consumed)]
		if _, err := src.ReadAt(part, offset+consumed); err != nil {
			return 0, fmt.Errorf("read DS file allocation table: %w", err)
		}
		for i := 0; i < len(part); i += 8 {
			start, end := ndsWord(part, i), ndsWord(part, i+4)
			if start == 0 && end == 0 {
				continue
			}
			id := (consumed + int64(i)) / 8
			if end < start || !ndsSpanFits(start, end-start, minimum, maximum) {
				return 0, fmt.Errorf("DS file %d exceeds cartridge bounds", id)
			}
			if id < authFiles {
				sectors := (end - start + authSectorSize - 1) / authSectorSize
				sectors = min(sectors, authSectorsLeft/(authFiles-id))
				authSectorsLeft -= sectors
				authEnd = max(authEnd, start+sectors*authSectorSize)
			}
		}
		consumed += int64(len(part))
	}
	return authEnd, nil
}

func validateNDSOverlays(ctx context.Context, src io.ReaderAt, header []byte) error {
	// Overlay records refer to the FAT independently of the filename table.
	fatCount := ndsWord(header, 0x4c) / 8
	var block [4096]byte
	for _, table := range [...]struct {
		cpu string
		at  int
	}{{"ARM9", 0x50}, {"ARM7", 0x58}} {
		offset, size := ndsWord(header, table.at), ndsWord(header, table.at+4)
		for consumed := int64(0); consumed < size; {
			if err := ctx.Err(); err != nil {
				return err
			}
			part := block[:min(int64(len(block)), size-consumed)]
			if _, err := src.ReadAt(part, offset+consumed); err != nil {
				return fmt.Errorf("read DS %s overlays: %w", table.cpu, err)
			}
			for i := 0; i < len(part); i += ndsOverlaySize {
				if ndsWord(part, i+0x18) >= fatCount {
					return fmt.Errorf("DS %s overlay %d points beyond the file allocation table", table.cpu, (consumed+int64(i))/ndsOverlaySize)
				}
			}
			consumed += int64(len(part))
		}
	}
	return nil
}

func keepNDSARM9Footers(ctx context.Context, src io.ReaderAt, at, keep, size int64) (int64, error) {
	// ndstool stores the Nitro footer outside the declared ARM9 length.
	// ndspy also recognizes consecutive records with the same magic.
	const magic = 0xdec00621
	var block [512 * ndsARM9FooterSize]byte
	for size-at >= 4 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		part := block[:min(int64(len(block)), size-at)]
		if _, err := src.ReadAt(part, at); err != nil {
			return 0, fmt.Errorf("read DS ARM9 footer: %w", err)
		}
		for len(part) >= 4 {
			if binary.LittleEndian.Uint32(part) != magic {
				return max(keep, at), ctx.Err()
			}
			if len(part) < ndsARM9FooterSize {
				return 0, errors.New("truncated DS ARM9 footer")
			}
			at += ndsARM9FooterSize
			part = part[ndsARM9FooterSize:]
		}
	}
	return max(keep, at), ctx.Err()
}

func keepNDSDownloadAuth(src io.ReaderAt, end, size int64) (int64, error) {
	keep := end
	// NSMB Editor and ndspy use a fixed authenticator pointer before ARM9,
	// including in images that declare a shorter header.
	var pointer [4]byte
	if _, err := src.ReadAt(pointer[:], ndsAuthPointer); err != nil {
		return 0, fmt.Errorf("read DS Download Play offset: %w", err)
	}
	start := ndsWord(pointer[:], 0)
	if start >= ndsSecureAreaEnd && start <= size {
		keep = max(keep, min(size, start+ndsDownloadAuth))
	}
	// GodMode9 recognizes the auth marker immediately past the NTR ROM size.
	var marker [2]byte
	if size-end < int64(len(marker)) {
		return keep, nil
	}
	if _, err := src.ReadAt(marker[:], end); err != nil {
		return 0, fmt.Errorf("read DS Download Play marker: %w", err)
	}
	if marker != [2]byte{'a', 'c'} {
		return keep, nil
	}
	if size-end < ndsDownloadAuth {
		return 0, errors.New("truncated DS Download Play authentication block")
	}
	return max(keep, end+ndsDownloadAuth), nil
}

func ndsSpanFits(start, size, minimum, maximum int64) bool {
	return start >= minimum && start <= maximum && size <= maximum-start
}

func ndsWord(data []byte, offset int) int64 {
	return int64(binary.LittleEndian.Uint32(data[offset:]))
}

func ndsCRC16(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, b := range data {
		crc ^= uint16(b)
		for bit := 0; bit < 8; bit++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xa001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
