package codec

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
)

type sizedReadSeeker interface {
	io.ReadSeeker
	Size() int64
}

// validateZstandardFrame checks one complete frame and bounds its decoded size.
// Entropy-coded payloads and decoded checksums require full decompression.
func ValidateZstandardFrame(ctx context.Context, src sizedReadSeeker, expanded uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var word [4]byte
	if _, err := io.ReadFull(src, word[:]); err != nil {
		return err
	}
	magic := binary.LittleEndian.Uint32(word[:])
	if magic&0xfffffff0 == 0x184d2a50 {
		if _, err := io.ReadFull(src, word[:]); err != nil {
			return err
		}
		if expanded != 0 || int64(binary.LittleEndian.Uint32(word[:])) != src.Size()-8 {
			return fmt.Errorf("invalid skippable Zstandard frame length")
		}
		return nil
	}
	if magic != 0xfd2fb528 {
		return fmt.Errorf("invalid Zstandard frame signature")
	}
	var descriptor [1]byte
	if _, err := io.ReadFull(src, descriptor[:]); err != nil {
		return err
	}
	flags := descriptor[0]
	if flags&8 != 0 {
		return fmt.Errorf("reserved Zstandard frame flag is set")
	}
	dictionarySize := [...]int{0, 1, 2, 4}[flags&3]
	prefix := dictionarySize
	contentSize := [...]int{0, 2, 4, 8}[flags>>6]
	singleSegment := flags&32 != 0
	if !singleSegment {
		prefix++ // Window descriptor.
	} else if contentSize == 0 {
		contentSize = 1
	}
	var fields [13]byte
	if _, err := io.ReadFull(src, fields[:prefix+contentSize]); err != nil {
		return err
	}
	for _, b := range fields[prefix-dictionarySize : prefix] {
		if b != 0 {
			return fmt.Errorf("Zstandard frame requires an unsupported external dictionary")
		}
	}
	window := expanded // Single-segment frames use the frame content size.
	if !singleSegment {
		base := uint64(1) << (10 + (fields[0] >> 3))
		window = base + (base/8)*uint64(fields[0]&7)
	}
	blockLimit := min(window, 128<<10)
	if contentSize != 0 {
		var value [8]byte
		copy(value[:], fields[prefix:prefix+contentSize])
		size := binary.LittleEndian.Uint64(value[:])
		if contentSize == 2 {
			size += 256
		}
		if size != expanded {
			return fmt.Errorf("Zstandard content size disagrees with its declared expansion")
		}
	}
	var minExpansion, maxExpansion uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.ReadFull(src, word[:3]); err != nil {
			return err
		}
		word[3] = 0
		header := binary.LittleEndian.Uint32(word[:])
		kind, size := header>>1&3, uint64(header>>3)
		if kind == 3 || size > blockLimit || kind == 2 && size < 2 {
			return fmt.Errorf("invalid Zstandard block header")
		}
		maxBlockExpansion := size
		if kind == 2 {
			maxBlockExpansion = blockLimit
		} else {
			if size > expanded-minExpansion {
				return fmt.Errorf("Zstandard blocks exceed the declared expansion")
			}
			minExpansion += size
		}
		maxExpansion += min(maxBlockExpansion, expanded-maxExpansion)
		stored := int64(size)
		if kind == 1 {
			stored = 1 // An RLE block stores one byte.
		}
		end, err := src.Seek(stored, io.SeekCurrent)
		if err != nil {
			return err
		}
		if end > src.Size() {
			return fmt.Errorf("Zstandard block exceeds its stored frame")
		}
		if header&1 == 0 {
			continue
		}
		if flags&4 != 0 {
			end += 4 // Optional frame checksum.
		}
		if end != src.Size() {
			return fmt.Errorf("Zstandard frame length disagrees with its stored extent")
		}
		if maxExpansion < expanded {
			return fmt.Errorf("Zstandard blocks cannot produce the declared expansion")
		}
		return ctx.Err()
	}
}
