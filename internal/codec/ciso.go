package codec

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
)

const (
	cisoHeaderSize   = 0x8000
	cisoMapOffset    = 8
	cisoMapEntries   = cisoHeaderSize - cisoMapOffset
	cisoMaxBlockSize = 1 << 31
)

// ValidateCISO checks Nintendo Compact ISO geometry, its presence map, and
// the stored Wii or GameCube header without expanding the image.
func ValidateCISO(ctx context.Context, src io.ReaderAt, size int64) error {
	_, err := parseCISO(ctx, src, size)
	return err
}

func encodeCISO(ctx context.Context, src io.ReaderAt, size int64, dst io.WriteSeeker, opts Options) error {
	if err := validateSparseSize("ciso", size); err != nil {
		return err
	}
	disc, err := packDisc(ctx, src, size, dst, cisoHeaderSize, opts)
	if err != nil {
		return err
	}
	var header [cisoHeaderSize]byte
	copy(header[:], "CISO")
	binary.LittleEndian.PutUint32(header[4:], packedDiscBlockSize)
	for index, block := range disc.blocks {
		if block != 0 {
			header[cisoMapOffset+index] = 1
		}
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writeAll(dst, header[:])
}

func decodeCISO(ctx context.Context, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	disc, err := parseCISO(ctx, src, size)
	if err != nil {
		return err
	}
	if disc.size > opts.MaxOutput {
		return errors.New("CISO disc exceeds expanded size limit")
	}
	return copyStream(ctx, dst, io.NewSectionReader(disc, 0, disc.size), disc.size, opts)
}

func parseCISO(ctx context.Context, src io.ReaderAt, size int64) (*sparseDisc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src = contextReaderAt{ReaderAt: src, ctx: ctx}
	var header [cisoHeaderSize]byte
	if size < cisoHeaderSize {
		return nil, errors.New("truncated CISO header")
	}
	if err := readDiscAt(src, header[:], 0); err != nil {
		return nil, err
	}
	if string(header[:4]) != "CISO" {
		return nil, errors.New("invalid CISO header")
	}
	blockSize := int64(binary.LittleEndian.Uint32(header[4:]))
	if blockSize < wiiSectorSize || blockSize > cisoMaxBlockSize || blockSize%wiiSectorSize != 0 {
		return nil, errors.New("invalid CISO block size")
	}
	if header[cisoMapOffset] != 1 {
		return nil, errors.New("CISO is missing its first disc block")
	}
	disc := &sparseDisc{source: src, dataAt: cisoHeaderSize, blockSize: blockSize, blocks: make([]uint16, cisoMapEntries)}
	var stored uint16
	last := 0
	for index, mapped := range header[cisoMapOffset:] {
		switch mapped {
		case 0:
		case 1:
			stored++
			disc.blocks[index] = stored
			last = index
		default:
			return nil, errors.New("invalid CISO presence map")
		}
	}
	if size != cisoHeaderSize+int64(stored)*blockSize {
		return nil, errors.New("CISO data size disagrees with its map")
	}
	var boot [discHeaderSize]byte
	if err := readDiscAt(src, boot[:], cisoHeaderSize); err != nil {
		return nil, err
	}
	if hasWiiMagic(boot[:]) {
		if err := disc.setSize(ctx, last, wiiSingleLayerSize, wiiDualLayerSize); err != nil {
			return nil, err
		}
	} else if hasGameCubeMagic(boot[:]) {
		if err := disc.setSize(ctx, last, gameCubeDiscSize); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("CISO does not contain a GameCube or Wii disc header")
	}
	return disc, ctx.Err()
}
