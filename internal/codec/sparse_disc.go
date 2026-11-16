package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	wiiSectorSize       = 0x8000
	gameCubeDiscSize    = 1459978240
	wiiSingleLayerSize  = 143432 * wiiSectorSize
	wiiDualLayerSize    = 259740 * wiiSectorSize
	wiiDiscMagicOffset  = 0x18
	wiiDiscMagic        = 0x5d1c9ea3
	gameCubeMagicOffset = 0x1c
	gameCubeMagic       = 0xc2339f3d
	packedDiscBlockSize = 2 << 20
	discHeaderSize      = 0x100
)

// Both WBFS and CISO omit only zero-filled chunks. The block table used by our
// reader stores one-based physical indices, with zero representing a hole.
type sparseDisc struct {
	source    io.ReaderAt
	dataAt    int64
	blockSize int64
	blocks    []uint16
	size      int64
}

func (d *sparseDisc) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative disc offset")
	}
	var read int
	for len(p) > 0 && offset < d.size {
		block := offset / d.blockSize
		within := offset % d.blockSize
		n := min(int64(len(p)), d.size-offset, d.blockSize-within)
		data := p[:n]
		entry := d.blocks[block]
		if entry == 0 {
			clear(data)
		} else if err := readDiscAt(d.source, data, d.physicalOffset(entry)+within); err != nil {
			return read, err
		}
		read += len(data)
		offset += n
		p = p[n:]
	}
	if len(p) != 0 {
		return read, io.EOF
	}
	return read, nil
}

func (d *sparseDisc) physicalOffset(entry uint16) int64 {
	return d.dataAt + (int64(entry)-1)*d.blockSize
}

// Determine the smallest nominal ISO size covering the mapped disc data. A
// stored block crossing that boundary may contain zero padding only.
func (d *sparseDisc) setSize(ctx context.Context, last int, nominalSizes ...int64) error {
	logical := int64(last) * d.blockSize
	capacity := int64(len(d.blocks)) * d.blockSize
	physical := d.physicalOffset(d.blocks[last])
	for _, size := range nominalSizes {
		if logical >= size || size > capacity {
			continue
		}
		fits, err := zeroDiscPadding(ctx, d.source, physical+size-logical, logical+d.blockSize-size)
		if err != nil {
			return err
		}
		if fits {
			d.size = size
			return nil
		}
	}
	return errors.New("disc map does not fit a supported ISO size")
}

func zeroDiscPadding(ctx context.Context, src io.ReaderAt, offset, remaining int64) (bool, error) {
	var buffer [128 << 10]byte
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n := min(remaining, int64(len(buffer)))
		if err := readDiscAt(src, buffer[:n], offset); err != nil {
			return false, err
		}
		if !allDiscZeros(buffer[:n]) {
			return false, nil
		}
		offset += n
		remaining -= n
	}
	return true, nil
}

type packedDisc struct {
	header     [discHeaderSize]byte
	blocks     []uint16
	dataBlocks int64
}

func packDisc(ctx context.Context, src io.ReaderAt, size int64, dst io.WriteSeeker, dataAt int64, opts Options) (packedDisc, error) {
	disc := packedDisc{blocks: make([]uint16, (size+packedDiscBlockSize-1)/packedDiscBlockSize)}
	if _, err := dst.Seek(dataAt, io.SeekStart); err != nil {
		return packedDisc{}, err
	}
	buffer := make([]byte, packedDiscBlockSize)
	for offset := int64(0); offset < size; offset += packedDiscBlockSize {
		if err := ctx.Err(); err != nil {
			return packedDisc{}, err
		}
		length := min(size-offset, packedDiscBlockSize)
		data := buffer[:length]
		if err := readDiscAt(src, data, offset); err != nil {
			return packedDisc{}, fmt.Errorf("read disc ISO: %w", err)
		}
		if offset == 0 {
			if err := validateSparseHeader(data, size); err != nil {
				return packedDisc{}, err
			}
			copy(disc.header[:], data[:discHeaderSize])
		}
		// Retain a zero final block. Its position makes a generated Wii
		// container's layer count recoverable without a private header field.
		if offset+length == size || !allDiscZeros(data) {
			if dataAt+(disc.dataBlocks+1)*packedDiscBlockSize > opts.MaxOutput {
				return packedDisc{}, errors.New("packed disc exceeds output limit")
			}
			clear(buffer[len(data):])
			if err := writeAll(dst, buffer); err != nil {
				return packedDisc{}, err
			}
			disc.dataBlocks++
			disc.blocks[offset/packedDiscBlockSize] = uint16(disc.dataBlocks)
		}
		if opts.Progress != nil {
			opts.Progress(offset+length, size)
		}
	}
	return disc, ctx.Err()
}

func readDiscAt(src io.ReaderAt, data []byte, offset int64) error {
	_, err := io.ReadFull(io.NewSectionReader(src, offset, int64(len(data))), data)
	return err
}

func allDiscZeros(data []byte) bool {
	return bytes.Count(data, []byte{0}) == len(data)
}

func hasWiiMagic(header []byte) bool {
	return binary.BigEndian.Uint32(header[wiiDiscMagicOffset:]) == wiiDiscMagic
}

func hasGameCubeMagic(header []byte) bool {
	return binary.BigEndian.Uint32(header[gameCubeMagicOffset:]) == gameCubeMagic
}
