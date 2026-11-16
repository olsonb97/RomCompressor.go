package codec

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"
)

const (
	csoHeaderSize    = 24
	writeBlockSize   = 2048
	maxBlockSize     = 1 << 20
	indexPageEntries = 4096
)

// CSO and ZSO use the same indexed container. Index pages stay bounded even for large images.
func encodeCSO(ctx context.Context, format string, src io.ReaderAt, size int64, dst io.WriteSeeker, opts Options) error {
	if err := validateCSOSize(size); err != nil {
		return err
	}
	blocks := (size + writeBlockSize - 1) / writeBlockSize
	dataOffset := int64(csoHeaderSize) + 4*(blocks+1)
	var shift uint8
	for (dataOffset+size+blocks*(int64(1)<<shift))>>shift >= 1<<31 {
		shift++
	}
	align := int64(1) << shift
	header := make([]byte, csoHeaderSize)
	copy(header, "CISO")
	if format == "zso" {
		copy(header, "ZISO")
	}
	binary.LittleEndian.PutUint32(header[4:], csoHeaderSize)
	binary.LittleEndian.PutUint64(header[8:], uint64(size))
	binary.LittleEndian.PutUint32(header[16:], writeBlockSize)
	header[20], header[21] = 1, shift
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := writeAll(dst, header); err != nil {
		return err
	}
	if _, err := dst.Seek(dataOffset, io.SeekStart); err != nil {
		return err
	}
	writer := bufio.NewWriterSize(dst, 256<<10)
	position := dataOffset
	index := make([]byte, 0, indexPageEntries*4)
	indexStart := int64(csoHeaderSize)
	flushIndex := func() error {
		if err := writer.Flush(); err != nil {
			return err
		}
		if _, err := dst.Seek(indexStart, io.SeekStart); err != nil {
			return err
		}
		if err := writeAll(dst, index); err != nil {
			return err
		}
		indexStart += int64(len(index))
		index = index[:0]
		_, err := dst.Seek(position, io.SeekStart)
		return err
	}
	plain := make([]byte, writeBlockSize)
	packed := make([]byte, lz4.CompressBlockBound(writeBlockSize))
	padding := make([]byte, align)
	var compressed bytes.Buffer
	deflater, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		return err
	}
	defer deflater.Close()
	var compressor lz4.Compressor
	for block := int64(0); block < blocks; block++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		length := min(int64(writeBlockSize), size-block*writeBlockSize)
		data := plain[:length]
		if _, err := src.ReadAt(data, block*writeBlockSize); err != nil {
			return err
		}
		var encoded []byte
		if format == "cso" {
			compressed.Reset()
			deflater.Reset(&compressed)
			if _, err := deflater.Write(data); err != nil {
				return err
			}
			if err := deflater.Close(); err != nil {
				return err
			}
			encoded = compressed.Bytes()
		} else {
			n, err := compressor.CompressBlock(data, packed)
			if err != nil {
				return err
			}
			encoded = packed[:n]
		}
		pad := (-position) & (align - 1)
		if err := writeAll(writer, padding[:pad]); err != nil {
			return err
		}
		position += pad
		entry := uint32(position >> shift)
		if len(encoded) == 0 || len(encoded) >= len(data) {
			encoded = data
			entry |= 1 << 31
		}
		index = binary.LittleEndian.AppendUint32(index, entry)
		if err := writeAll(writer, encoded); err != nil {
			return err
		}
		position += int64(len(encoded))
		if len(index) == cap(index) {
			if err := flushIndex(); err != nil {
				return err
			}
		}
		if opts.Progress != nil {
			opts.Progress(block*writeBlockSize+length, size)
		}
	}
	pad := (-position) & (align - 1)
	if err := writeAll(writer, padding[:pad]); err != nil {
		return err
	}
	position += pad
	index = binary.LittleEndian.AppendUint32(index, uint32(position>>shift))
	if err := flushIndex(); err != nil {
		return err
	}
	return ctx.Err()
}

type csoGeometry struct {
	total, blockSize uint64
	shift            uint8
	indexEnd         int64
}

func readCSOGeometry(format string, src io.ReaderAt, size, limit int64) (csoGeometry, error) {
	var geometry csoGeometry
	header := make([]byte, csoHeaderSize)
	if _, err := src.ReadAt(header, 0); err != nil {
		return geometry, fmt.Errorf("read disc header: %w", err)
	}
	magic := "CISO"
	if format == "zso" {
		magic = "ZISO"
	}
	if string(header[:4]) != magic {
		return geometry, errors.New("disc header does not match format")
	}
	if (format == "cso" && header[20] > 1) || (format == "zso" && header[20] != 1) {
		return geometry, fmt.Errorf("unsupported %s version %d", format, header[20])
	}
	if format == "zso" && (binary.LittleEndian.Uint32(header[4:]) != csoHeaderSize || header[22] != 0 || header[23] != 0) {
		return geometry, errors.New("invalid ZSO header")
	}
	// CSO v0/v1 header_size is unreliable in existing encoders; the index always starts at byte 24.
	total := binary.LittleEndian.Uint64(header[8:])
	blockSize := uint64(binary.LittleEndian.Uint32(header[16:]))
	shift := header[21]
	if total > uint64(limit) {
		return geometry, errors.New("disc exceeds expanded size limit")
	}
	if blockSize == 0 || blockSize > maxBlockSize || blockSize%2048 != 0 {
		return geometry, errors.New("invalid disc block size; expected a multiple of 2048 up to 1 MiB")
	}
	if shift > 20 {
		return geometry, errors.New("disc index alignment exceeds 1 MiB")
	}
	blocks := (total + blockSize - 1) / blockSize
	indexEnd := int64(csoHeaderSize + 4*(blocks+1))
	if indexEnd > size {
		return geometry, errors.New("truncated disc index")
	}
	return csoGeometry{total: total, blockSize: blockSize, shift: shift, indexEnd: indexEnd}, nil
}

func decodeCSO(ctx context.Context, format string, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	geometry, err := readCSOGeometry(format, src, size, opts.MaxOutput)
	if err != nil {
		return err
	}
	total, blockSize, shift, indexEnd := geometry.total, geometry.blockSize, geometry.shift, geometry.indexEnd
	align := int64(1) << shift
	blocks := (total + blockSize - 1) / blockSize
	indices := bufio.NewReaderSize(io.NewSectionReader(src, csoHeaderSize, indexEnd-csoHeaderSize), indexPageEntries*4)
	readIndex := func() (uint32, error) {
		var b [4]byte
		_, err := io.ReadFull(indices, b[:])
		return binary.LittleEndian.Uint32(b[:]), err
	}
	entry, err := readIndex()
	if err != nil {
		return err
	}
	start := int64(entry&0x7fffffff) << shift
	if start < indexEnd || start-indexEnd >= align {
		return errors.New("invalid first disc block offset")
	}
	plain := make([]byte, blockSize)
	packed := make([]byte, lz4.CompressBlockBound(int(blockSize))+int(align)-1)
	var inflater io.ReadCloser
	defer func() {
		if inflater != nil {
			inflater.Close()
		}
	}()
	for block := uint64(0); block < blocks; block++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := readIndex()
		if err != nil {
			return err
		}
		end := int64(next&0x7fffffff) << shift
		length := end - start
		if length <= 0 || end > size || length > int64(len(packed)) {
			return errors.New("invalid disc block offsets")
		}
		outSize := min(blockSize, total-block*blockSize)
		out := plain[:outSize]
		if entry&0x80000000 != 0 {
			storedSize := int64(outSize)
			// The index only gives aligned offsets. If short and full raw
			// frames align to the same end, retain the usual alignment rules.
			if length-storedSize >= align && outSize < blockSize {
				storedSize = int64(blockSize)
			}
			if length < storedSize || length-storedSize >= align {
				return errors.New("invalid uncompressed disc block length")
			}
			if _, err := src.ReadAt(plain[:storedSize], start); err != nil {
				return err
			}
			if !allDiscZeros(plain[outSize:storedSize]) {
				return errors.New("final disc block contains data after the logical end")
			}
		} else {
			encoded := packed[:length]
			if _, err := src.ReadAt(encoded, start); err != nil {
				return err
			}
			if format == "cso" {
				r := bytes.NewReader(encoded)
				if inflater == nil {
					inflater = flate.NewReader(r)
				} else if err := inflater.(flate.Resetter).Reset(r, nil); err != nil {
					return err
				}
				if err := inflateCSOBlock(inflater, plain, int(outSize)); err != nil {
					return err
				}
				if int64(r.Len()) >= align {
					return errors.New("excess trailing data in disc block")
				}
			} else {
				if err := decodeZSOBlock(encoded, plain, int(outSize), int(align)); err != nil {
					return err
				}
			}
		}
		if err := writeAll(dst, out); err != nil {
			return err
		}
		if opts.Progress != nil {
			opts.Progress(int64(block*blockSize+outSize), int64(total))
		}
		entry, start = next, end
	}
	if entry&0x80000000 != 0 || start != size {
		return errors.New("disc final index does not match file size")
	}
	return ctx.Err()
}

// maxcso pads the last partial frame of a larger-block image to a full frame.
func inflateCSOBlock(inflater io.Reader, block []byte, logical int) error {
	if _, err := io.ReadFull(inflater, block[:logical]); err != nil {
		return fmt.Errorf("decode disc block: %w", err)
	}
	padding := block[logical:]
	n, err := io.ReadFull(inflater, padding)
	if n == 0 && err == io.EOF {
		return nil // Short final block without sector padding.
	}
	if err != nil || !allDiscZeros(padding) {
		return errors.New("final disc block contains data after the logical end or invalid padding")
	}
	var extra [1]byte
	if n, err := inflater.Read(extra[:]); n != 0 || err != io.EOF {
		return errors.New("disc block exceeds its frame size or has invalid termination")
	}
	return nil
}

func decodeZSOBlock(encoded, block []byte, logical, alignment int) error {
	// A valid full stream may also have a prefix that looks like a short
	// block. Check its decoded padding before trying the short form.
	for _, length := range []int{len(block), logical} {
		consumed, err := lz4PayloadLength(encoded, length)
		if err == nil && len(encoded)-consumed < alignment {
			n, err := lz4.UncompressBlock(encoded[:consumed], block[:length])
			if err == nil && n == length {
				if !allDiscZeros(block[logical:length]) {
					return errors.New("ZSO final block contains data after the logical end")
				}
				return nil
			}
		}
		if length == logical {
			break
		}
	}
	return errors.New("invalid ZSO block or nonzero final frame padding")
}

// LZ4 has no encoded-length field. Locate its terminal literal sequence before
// alignment padding, then pass exactly that block to the upstream decoder.
func lz4PayloadLength(data []byte, expected int) (int, error) {
	position, produced := 0, 0
	readLength := func(base int) (int, bool) {
		length := base
		if base != 15 {
			return length, true
		}
		for position < len(data) {
			n := int(data[position])
			position++
			length += n
			if length > expected {
				return 0, false
			}
			if n != 255 {
				return length, true
			}
		}
		return 0, false
	}
	for position < len(data) {
		token := data[position]
		position++
		literals, ok := readLength(int(token >> 4))
		if !ok || literals > len(data)-position || literals > expected-produced {
			break
		}
		position += literals
		produced += literals
		if produced == expected {
			if literals < min(5, expected) {
				break
			}
			return position, nil
		}
		// A match must begin at least 12 bytes from the end of the block.
		if expected-produced < 12 || len(data)-position < 2 {
			break
		}
		distance := int(binary.LittleEndian.Uint16(data[position:]))
		position += 2
		if distance == 0 || distance > produced {
			break
		}
		match, ok := readLength(int(token & 15))
		if !ok || match+4 >= expected-produced {
			break
		}
		produced += match + 4
	}
	return 0, errors.New("invalid or truncated LZ4 disc block")
}

func writeAll(dst io.Writer, data []byte) error {
	n, err := dst.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}
