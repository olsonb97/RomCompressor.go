package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"
)

const (
	cciHeaderBytes  = 32
	cciSectorBytes  = 2048
	cciPositionMask = 1<<31 - 1
	cciCompressed   = 1 << 31
	cciAlignment    = 4
	cciMaxImage     = 8 << 30
	xisoVolume      = 32
	xisoMagic       = "MICROSOFT*XBOX*MEDIA"
)

// ValidateCCI checks a single-part CCI sector map and its XISO volume descriptor.
func ValidateCCI(ctx context.Context, src io.ReaderAt, size int64) error {
	_, _, err := loadCCI(ctx, src, size, cciMaxImage)
	return err
}

func encodeCCI(ctx context.Context, src io.ReaderAt, size int64, dst io.WriteSeeker, opts Options) error {
	if err := validateCCISize(size); err != nil {
		return err
	}
	if _, err := dst.Seek(cciHeaderBytes, io.SeekStart); err != nil {
		return err
	}
	count := size / cciSectorBytes
	table := make([]byte, 0, (count+1)*4)
	sector := make([]byte, cciSectorBytes)
	packed := make([]byte, lz4.CompressBlockBound(cciSectorBytes)+cciAlignment)
	var compressor lz4.Compressor
	position := int64(cciHeaderBytes)
	for index := int64(0); index < count; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := readDiscAt(src, sector, index*cciSectorBytes); err != nil {
			return fmt.Errorf("read XISO sector: %w", err)
		}
		if index == xisoVolume {
			if err := validateXISOVolume(sector, size); err != nil {
				return err
			}
		}
		n, err := compressor.CompressBlock(sector, packed[1:])
		if err != nil {
			return err
		}
		entry := uint32(position / cciAlignment)
		block := sector
		if n > 0 && n < cciSectorBytes-2*cciAlignment {
			pad := (-(n + 1)) & (cciAlignment - 1)
			packed[0] = byte(pad)
			clear(packed[n+1 : n+1+pad])
			block = packed[:n+1+pad]
			entry |= cciCompressed
		}
		end := position + int64(len(block))
		if end/cciAlignment > cciPositionMask || end+int64(cap(table)) > opts.MaxOutput {
			return errors.New("CCI output exceeds the single-part limit")
		}
		if err := writeAll(dst, block); err != nil {
			return err
		}
		table = binary.LittleEndian.AppendUint32(table, entry)
		position = end
		if opts.Progress != nil {
			opts.Progress((index+1)*cciSectorBytes, size)
		}
	}
	table = binary.LittleEndian.AppendUint32(table, uint32(position/cciAlignment))
	if err := writeAll(dst, table); err != nil {
		return err
	}
	var header [cciHeaderBytes]byte
	copy(header[:], "CCIM")
	binary.LittleEndian.PutUint32(header[4:], cciHeaderBytes)
	binary.LittleEndian.PutUint64(header[8:], uint64(size))
	binary.LittleEndian.PutUint64(header[16:], uint64(position))
	binary.LittleEndian.PutUint32(header[24:], cciSectorBytes)
	header[28], header[29] = 1, 2
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writeAll(dst, header[:])
}

func decodeCCI(ctx context.Context, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	decoded, table, err := loadCCI(ctx, src, size, opts.MaxOutput)
	if err != nil {
		return err
	}
	sector := make([]byte, cciSectorBytes)
	packed := make([]byte, cciSectorBytes)
	for i := 0; i+4 < len(table); i += 4 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := readCCISector(src, table[i:i+8], packed, sector); err != nil {
			return fmt.Errorf("read CCI sector %d: %w", i/4, err)
		}
		if err := writeAll(dst, sector); err != nil {
			return err
		}
		if opts.Progress != nil {
			opts.Progress((int64(i)/4+1)*cciSectorBytes, decoded)
		}
	}
	return ctx.Err()
}

func loadCCI(ctx context.Context, src io.ReaderAt, size, limit int64) (int64, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	var header [cciHeaderBytes]byte
	if err := readDiscAt(src, header[:], 0); err != nil {
		return 0, nil, fmt.Errorf("read CCI header: %w", err)
	}
	if string(header[:4]) != "CCIM" || binary.LittleEndian.Uint32(header[4:8]) != cciHeaderBytes ||
		binary.LittleEndian.Uint32(header[24:28]) != cciSectorBytes ||
		header[28] != 1 || header[29] != 2 || binary.LittleEndian.Uint16(header[30:]) != 0 {
		return 0, nil, errors.New("invalid CCI header")
	}
	decoded := binary.LittleEndian.Uint64(header[8:16])
	tableAt := binary.LittleEndian.Uint64(header[16:24])
	if decoded < (xisoVolume+1)*cciSectorBytes || decoded > cciMaxImage || decoded%cciSectorBytes != 0 ||
		tableAt <= cciHeaderBytes || tableAt > cciPositionMask*cciAlignment || tableAt%cciAlignment != 0 {
		return 0, nil, errors.New("invalid CCI size or index position")
	}
	if decoded > uint64(limit) {
		return 0, nil, errors.New("CCI disc exceeds expanded size limit")
	}
	tableBytes := (decoded/cciSectorBytes + 1) * 4
	if size < 0 || tableAt+tableBytes != uint64(size) {
		return 0, nil, errors.New("CCI index does not end at the file boundary")
	}
	table := make([]byte, tableBytes)
	if err := readDiscAt(src, table, int64(tableAt)); err != nil {
		return 0, nil, err
	}
	if cciOffset(binary.LittleEndian.Uint32(table)) != cciHeaderBytes ||
		cciOffset(binary.LittleEndian.Uint32(table[len(table)-4:])) != int64(tableAt) {
		return 0, nil, errors.New("CCI index does not span the complete data region")
	}
	for i := 0; i+4 < len(table); i += 4 {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		first := binary.LittleEndian.Uint32(table[i:])
		next := binary.LittleEndian.Uint32(table[i+4:])
		length := cciOffset(next) - cciOffset(first)
		if length <= 0 || length > cciSectorBytes || (first&cciCompressed == 0 && length != cciSectorBytes) {
			return 0, nil, errors.New("invalid CCI sector offsets")
		}
	}
	var sector, packed [cciSectorBytes]byte
	if err := readCCISector(src, table[4*xisoVolume:4*xisoVolume+8], packed[:], sector[:]); err != nil {
		return 0, nil, fmt.Errorf("read CCI volume descriptor: %w", err)
	}
	if err := validateXISOVolume(sector[:], int64(decoded)); err != nil {
		return 0, nil, err
	}
	return int64(decoded), table, ctx.Err()
}

func cciOffset(entry uint32) int64 {
	return int64(entry&cciPositionMask) * cciAlignment
}

func readCCISector(src io.ReaderAt, entries, buffer, sector []byte) error {
	entry := binary.LittleEndian.Uint32(entries)
	start := cciOffset(entry)
	if entry&cciCompressed == 0 {
		return readDiscAt(src, sector, start)
	}
	length := cciOffset(binary.LittleEndian.Uint32(entries[4:])) - start
	packed := buffer[:length]
	if err := readDiscAt(src, packed, start); err != nil {
		return err
	}
	padding := int(packed[0])
	if padding >= cciAlignment || len(packed) <= padding+1 || !allDiscZeros(packed[len(packed)-padding:]) {
		return errors.New("invalid CCI compressed sector padding")
	}
	compressed := packed[1 : len(packed)-padding]
	if length, err := lz4PayloadLength(compressed, len(sector)); err != nil || length != len(compressed) {
		return errors.New("invalid CCI LZ4 block framing")
	}
	n, err := lz4.UncompressBlock(compressed, sector)
	if err != nil || n != len(sector) {
		return errors.New("CCI LZ4 block did not restore a complete sector")
	}
	return nil
}

func validateXISOVolume(sector []byte, imageSize int64) error {
	if !bytes.HasPrefix(sector, []byte(xisoMagic)) || !bytes.HasSuffix(sector, []byte(xisoMagic)) {
		return errors.New("CCI requires an XISO volume descriptor at sector 32")
	}
	root := int64(binary.LittleEndian.Uint32(sector[20:24])) * cciSectorBytes
	length := int64(binary.LittleEndian.Uint32(sector[24:28]))
	if root < (xisoVolume+1)*cciSectorBytes || root > imageSize || length == 0 || length > imageSize-root {
		return errors.New("XISO root directory is outside the image")
	}
	return nil
}
