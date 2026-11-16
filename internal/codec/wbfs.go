package codec

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
)

const (
	wbfsVirtualDiscSize = 2 * wiiSingleLayerSize
	wbfsSectorShift     = 9
	wbfsBlockShift      = 21
	wbfsSectorSize      = 1 << wbfsSectorShift
	wbfsBlockSize       = packedDiscBlockSize
	wbfsHeaderSize      = 12
)

// ValidateWBFS checks the geometry, slot, block map, and disc header of a
// self-contained single-disc WBFS file without expanding it.
func ValidateWBFS(ctx context.Context, src io.ReaderAt, size int64) error {
	_, err := parseWBFS(ctx, src, size)
	return err
}

// WBFSDeclaredSize reports the total container length in the WBFS header.
// A split container records its combined size in the first part.
func WBFSDeclaredSize(src io.ReaderAt) (int64, error) {
	size, _, _, err := wbfsGeometry(src)
	return size, err
}

func encodeWBFS(ctx context.Context, src io.ReaderAt, size int64, dst io.WriteSeeker, opts Options) error {
	if err := validateSparseSize("wbfs", size); err != nil {
		return err
	}
	disc, err := packDisc(ctx, src, size, dst, wbfsBlockSize, opts)
	if err != nil {
		return err
	}
	metadata := make([]byte, wbfsBlockSize)
	copy(metadata, "WBFS")
	binary.BigEndian.PutUint32(metadata[4:], uint32((1+disc.dataBlocks)*wbfsBlockSize/wbfsSectorSize))
	metadata[8], metadata[9] = wbfsSectorShift, wbfsBlockShift
	metadata[10] = 1             // WBFS version used by Wiimms ISO Tools.
	metadata[wbfsHeaderSize] = 1 // Slot zero contains the only disc.
	mapAt := wbfsSectorSize + discHeaderSize
	copy(metadata[wbfsSectorSize:mapAt], disc.header[:])
	for index, block := range disc.blocks {
		binary.BigEndian.PutUint16(metadata[mapAt+index*2:], block)
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writeAll(dst, metadata)
}

func decodeWBFS(ctx context.Context, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	disc, err := parseWBFS(ctx, src, size)
	if err != nil {
		return err
	}
	if disc.size > opts.MaxOutput {
		return errors.New("WBFS disc exceeds expanded size limit")
	}
	return copyStream(ctx, dst, io.NewSectionReader(disc, 0, disc.size), disc.size, opts)
}

func parseWBFS(ctx context.Context, src io.ReaderAt, size int64) (*sparseDisc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src = contextReaderAt{ReaderAt: src, ctx: ctx}
	physicalSize, sectorSize, blockSize, err := wbfsGeometry(src)
	if err != nil {
		return nil, err
	}
	if size != physicalSize {
		return nil, errors.New("WBFS length disagrees with its volume header")
	}
	var slots [1 << 12]byte
	if err := readDiscAt(src, slots[:sectorSize-wbfsHeaderSize], wbfsHeaderSize); err != nil {
		return nil, err
	}
	if slots[0] == 0 || !allDiscZeros(slots[1:]) {
		return nil, errors.New("WBFS requires exactly one disc in slot zero")
	}
	entries := (wbfsVirtualDiscSize + blockSize - 1) / blockSize
	info := make([]byte, discHeaderSize+2*entries)
	if err := readDiscAt(src, info, sectorSize); err != nil {
		return nil, err
	}
	if !hasWiiMagic(info) {
		return nil, errors.New("WBFS disc slot does not contain a Wii header")
	}
	disc := &sparseDisc{source: src, dataAt: blockSize, blockSize: blockSize, blocks: make([]uint16, entries)}
	seen := make([]bool, int(size/blockSize))
	last := 0
	for index := range disc.blocks {
		entry := binary.BigEndian.Uint16(info[discHeaderSize+index*2:])
		if entry == 0 {
			continue
		}
		if int(entry) >= len(seen) || seen[entry] {
			return nil, errors.New("WBFS block map points outside its volume or reuses a block")
		}
		seen[entry] = true
		disc.blocks[index] = entry
		last = index
	}
	if disc.blocks[0] == 0 {
		return nil, errors.New("WBFS disc is missing its first block")
	}
	var discHeader [discHeaderSize]byte
	if err := readDiscAt(src, discHeader[:], disc.physicalOffset(disc.blocks[0])); err != nil {
		return nil, err
	}
	if !hasWiiMagic(discHeader[:]) {
		return nil, errors.New("WBFS first disc block does not contain a Wii header")
	}
	if err := disc.setSize(ctx, last, wiiSingleLayerSize, wiiDualLayerSize); err != nil {
		return nil, err
	}
	return disc, ctx.Err()
}

func wbfsGeometry(src io.ReaderAt) (size, sectorSize, blockSize int64, err error) {
	var header [wbfsHeaderSize]byte
	if err := readDiscAt(src, header[:], 0); err != nil {
		return 0, 0, 0, err
	}
	if string(header[:4]) != "WBFS" {
		return 0, 0, 0, errors.New("invalid WBFS header")
	}
	sectorShift, blockShift := header[8], header[9]
	if header[10] > 1 {
		return 0, 0, 0, errors.New("unsupported WBFS version")
	}
	if sectorShift < 9 || sectorShift > 12 || blockShift < 21 || blockShift > 28 {
		return 0, 0, 0, errors.New("unsupported WBFS sector or block size")
	}
	sectorSize, blockSize = int64(1)<<sectorShift, int64(1)<<blockShift
	size = int64(binary.BigEndian.Uint32(header[4:])) * sectorSize
	if size < 2*blockSize || size%blockSize != 0 || size/blockSize > 65536 {
		return 0, 0, 0, errors.New("invalid WBFS volume size")
	}
	return size, sectorSize, blockSize, nil
}
