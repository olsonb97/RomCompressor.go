package codec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// WUDImageBytes is the size of a complete Wii U optical disc image.
	WUDImageBytes  = 0x5d3a00000
	wuxMagic       = "WUX0\x2e\xd0\x99\x10"
	wuxHeaderBytes = 32
	wuxSectorBytes = 0x8000
	wuxSectors     = WUDImageBytes / wuxSectorBytes
	wuxDataOffset  = (wuxHeaderBytes + 4*wuxSectors + wuxSectorBytes - 1) &^ (wuxSectorBytes - 1)
)

// ValidateWUX checks the header, sector map, and stored data geometry.
func ValidateWUX(ctx context.Context, src io.ReaderAt, size int64) error {
	_, err := readWUXMap(ctx, src, size)
	return err
}

func readWUXMap(ctx context.Context, src io.ReaderAt, size int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src = contextReaderAt{ReaderAt: src, ctx: ctx}
	if size < wuxDataOffset+wuxSectorBytes || (size-wuxDataOffset)%wuxSectorBytes != 0 {
		return nil, errors.New("invalid WUX data or sector table length")
	}
	storedSectors := (size - wuxDataOffset) / wuxSectorBytes
	if storedSectors > wuxSectors {
		return nil, errors.New("WUX stores more sectors than a full disc")
	}
	var header [wuxHeaderBytes]byte
	if err := readDiscAt(src, header[:], 0); err != nil {
		return nil, err
	}
	if string(header[:8]) != wuxMagic || binary.LittleEndian.Uint32(header[8:12]) != wuxSectorBytes ||
		binary.LittleEndian.Uint64(header[16:24]) != WUDImageBytes {
		return nil, errors.New("invalid full-size WUX header")
	}
	// Header offsets 12–15 and 28–31 are structure alignment padding.
	if binary.LittleEndian.Uint32(header[24:28]) != 0 {
		return nil, errors.New("unsupported WUX header flags")
	}
	table := make([]byte, 4*wuxSectors)
	if err := readDiscAt(src, table, wuxHeaderBytes); err != nil {
		return nil, err
	}
	seen := make([]byte, (storedSectors+7)/8)
	remaining := storedSectors
	for offset := 0; offset < len(table); offset += 4 {
		index := int64(binary.LittleEndian.Uint32(table[offset:]))
		if index >= storedSectors {
			return nil, errors.New("WUX sector points outside the image")
		}
		bit := byte(1) << (index % 8)
		if seen[index/8]&bit == 0 {
			seen[index/8] |= bit
			remaining--
		}
	}
	if remaining != 0 {
		return nil, errors.New("WUX contains unreferenced stored sectors")
	}
	return table, ctx.Err()
}

func encodeWUX(ctx context.Context, src io.ReaderAt, size int64, dst io.WriteSeeker, opts Options) error {
	if err := validateWUXSize(size); err != nil {
		return err
	}
	stored, ok := dst.(io.ReaderAt)
	if !ok {
		return errors.New("WUX compression requires a readable staging file")
	}
	if _, err := dst.Seek(wuxDataOffset, io.SeekStart); err != nil {
		return err
	}
	prefix := make([]byte, wuxDataOffset)
	copy(prefix, wuxMagic)
	binary.LittleEndian.PutUint32(prefix[8:], wuxSectorBytes)
	binary.LittleEndian.PutUint64(prefix[16:], WUDImageBytes)
	table := prefix[wuxHeaderBytes:]
	// The exact disc size bounds the number of distinct fingerprints.
	positions := make(map[[sha256.Size]byte]uint32)
	data := make([]byte, wuxSectorBytes)
	previous := make([]byte, wuxSectorBytes)
	candidate := make([]byte, wuxSectorBytes)
	var unique, index uint32
	for sector := range wuxSectors {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := int64(sector) * wuxSectorBytes
		if err := readDiscAt(src, data, offset); err != nil {
			return fmt.Errorf("read WUD sector: %w", err)
		}
		if sector == 0 && bytes.HasPrefix(data, []byte("Wii ")) {
			return errors.New("WUMAD archives are unsupported for WUD compression")
		}
		if sector == 0 || !bytes.Equal(data, previous) {
			hash := sha256.Sum256(data)
			foundIndex, found := positions[hash]
			if found {
				physical := wuxDataOffset + int64(foundIndex)*wuxSectorBytes
				if err := readDiscAt(stored, candidate, physical); err != nil {
					return fmt.Errorf("check staged WUX sector: %w", err)
				}
				found = bytes.Equal(data, candidate)
			}
			index = foundIndex
			if !found {
				if wuxDataOffset+int64(unique+1)*wuxSectorBytes > opts.MaxOutput {
					return errors.New("WUX output exceeds output limit")
				}
				if err := writeAll(dst, data); err != nil {
					return err
				}
				index = unique
				positions[hash] = index
				unique++
			}
			copy(previous, data)
		}
		binary.LittleEndian.PutUint32(table[4*sector:], index)
		if opts.Progress != nil {
			opts.Progress(offset+wuxSectorBytes, size)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writeAll(dst, prefix)
}

func decodeWUX(ctx context.Context, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	if opts.MaxOutput < WUDImageBytes {
		return errors.New("WUX disc exceeds expanded size limit")
	}
	table, err := readWUXMap(ctx, src, size)
	if err != nil {
		return err
	}
	data := make([]byte, wuxSectorBytes)
	previous := ^uint32(0)
	for sector := range wuxSectors {
		if err := ctx.Err(); err != nil {
			return err
		}
		index := binary.LittleEndian.Uint32(table[4*sector:])
		if index != previous {
			if err := readDiscAt(src, data, wuxDataOffset+int64(index)*wuxSectorBytes); err != nil {
				return err
			}
			previous = index
		}
		if err := writeAll(dst, data); err != nil {
			return err
		}
		if opts.Progress != nil {
			opts.Progress(int64(sector+1)*wuxSectorBytes, WUDImageBytes)
		}
	}
	return ctx.Err()
}
