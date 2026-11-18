package chd

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

const chdV5HeaderSize = 124

type rawCHD struct {
	size       int64
	sectorSize uint32
	digest     [sha1.Size]byte
}

func readCHDV5Header(src io.Reader, header *[chdV5HeaderSize]byte) (rawCHD, error) {
	var image rawCHD
	if _, err := io.ReadFull(src, header[:]); err != nil {
		return image, fmt.Errorf("read CHD header: %w", err)
	}
	if string(header[:8]) != "MComprHD" || binary.BigEndian.Uint32(header[8:12]) != chdV5HeaderSize || binary.BigEndian.Uint32(header[12:16]) != 5 {
		return image, fmt.Errorf("expected a CHD v5 image")
	}
	logical := binary.BigEndian.Uint64(header[32:40])
	if logical == 0 || logical > math.MaxInt64 {
		return image, fmt.Errorf("invalid CHD logical size")
	}
	image.size = int64(logical)
	image.sectorSize = binary.BigEndian.Uint32(header[60:64])
	copy(image.digest[:], header[64:84])
	if image.sectorSize == 0 || logical%uint64(image.sectorSize) != 0 || image.digest == ([sha1.Size]byte{}) {
		return image, fmt.Errorf("CHD has an invalid sector size or no raw checksum")
	}
	return image, nil
}

func (image rawCHD) checkRaw(ctx context.Context, path string) error {
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if info.Size() != image.size {
		return fmt.Errorf("raw file does not match the CHD's logical size")
	}
	digest := sha1.New()
	n, err := io.Copy(digest, &fileops.ContextReader{Context: ctx, Reader: io.LimitReader(file, image.size)})
	if err != nil {
		return err
	}
	var actual [sha1.Size]byte
	digest.Sum(actual[:0])
	if n != image.size || actual != image.digest {
		return fmt.Errorf("raw image does not match the CHD's raw SHA-1")
	}
	return ctx.Err()
}

// MetadataFile rejects invalid links before chdman walks the chain to
// verify the overall checksum. CHD v3 and v4 can also read metadata on open.
func MetadataFile(ctx context.Context, path string, limit int64, media string) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	version, _, err := readMetadata(ctx, file, info.Size(), limit, media, nil)
	return version, err
}

// Media reports the media type named by a structurally valid CHD's metadata.
// Untagged raw images remain unidentified; sector size does not establish media.
func Media(ctx context.Context, src io.ReaderAt, size int64) (string, error) {
	_, media, err := readMetadata(ctx, src, size, math.MaxInt64, "", nil)
	return media, err
}

func readMetadata(ctx context.Context, src io.ReaderAt, size, limit int64, media string, visit func(chdMetadata) error) (uint32, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	reader := &fileops.ContextReader{Context: ctx, Reader: io.NewSectionReader(src, 0, size)}
	var header [chdV5HeaderSize]byte
	if _, err := io.ReadFull(reader, header[:16]); err != nil {
		return 0, "", fmt.Errorf("read CHD header: %w", err)
	}
	if string(header[:8]) != "MComprHD" {
		return 0, "", fmt.Errorf("invalid CHD header")
	}
	version := binary.BigEndian.Uint32(header[12:16])
	var headerSize uint32
	logicalAt, metaAt, hunkAt := 28, 36, 76
	switch version {
	case 3:
		headerSize = 120
	case 4:
		headerSize = 108
		hunkAt = 44
	case 5:
		headerSize = chdV5HeaderSize
		logicalAt, metaAt, hunkAt = 32, 48, 56
	default:
		return 0, "", fmt.Errorf("unsupported CHD version %d", version)
	}
	if binary.BigEndian.Uint32(header[8:12]) != headerSize || size < int64(headerSize) {
		return 0, "", fmt.Errorf("invalid CHD header size")
	}
	if _, err := io.ReadFull(reader, header[16:headerSize]); err != nil {
		return 0, "", fmt.Errorf("read CHD header: %w", err)
	}
	logical := binary.BigEndian.Uint64(header[logicalAt:])
	if logical == 0 || logical > math.MaxInt64 {
		return 0, "", fmt.Errorf("invalid CHD logical size")
	}
	if logical > uint64(limit) {
		return 0, "", fmt.Errorf("CHD exceeds the expanded size limit")
	}
	hunk := uint64(binary.BigEndian.Uint32(header[hunkAt:]))
	if hunk == 0 || hunk > 1<<20 { // chdman limits hunks to 1 MiB.
		return 0, "", fmt.Errorf("invalid CHD hunk size")
	}
	hunks := (logical-1)/hunk + 1
	if version < 5 {
		if hunks != uint64(binary.BigEndian.Uint32(header[24:28])) || hunks > math.MaxUint32/16 {
			return 0, "", fmt.Errorf("invalid CHD hunk count")
		}
	} else {
		unit := uint64(binary.BigEndian.Uint32(header[60:64]))
		if unit == 0 || hunk%unit != 0 || hunks > math.MaxUint32/12 {
			return 0, "", fmt.Errorf("invalid CHD hunk or unit geometry")
		}
	}
	if media == "dvd" && (version != 5 || binary.BigEndian.Uint32(header[60:64]) != dvdSectorSize || logical%dvdSectorSize != 0) {
		return 0, "", fmt.Errorf("DVD CHD requires a v5 image with 2048-byte sectors")
	}
	offset := binary.BigEndian.Uint64(header[metaAt:])
	detected := ""
	err := walkCHDMetadata(ctx, src, uint64(size), uint64(headerSize), offset, func(entry chdMetadata) error {
		if tagged := chdMetadataMedia(entry.tag); tagged != "" {
			if detected != "" && detected != tagged {
				return fmt.Errorf("CHD contains conflicting %s and %s media metadata", detected, tagged)
			}
			if media != "" && tagged != media {
				return fmt.Errorf("CHD %s metadata conflicts with requested %s media", tagged, media)
			}
			detected = tagged
		}
		if visit != nil {
			return visit(entry)
		}
		return nil
	})
	if err == nil && media != "" && media != "dvd" && detected == "" {
		err = fmt.Errorf("CHD lacks %s media metadata", media)
	}
	return version, detected, err
}

func chdMetadataMedia(tag string) string {
	switch tag {
	case "CHCD", "CHTR", "CHT2", "CHSE", "CHGT", "CHGD":
		return "cd"
	case "GDDD":
		return "hdd"
	case "DVD ":
		return "dvd"
	case "AVAV", "AVLD":
		return "laserdisc"
	}
	return ""
}

type chdMetadata struct {
	tag    string
	flags  byte
	offset int64
	length uint64
}

func walkCHDMetadata(ctx context.Context, src io.ReaderAt, size, headerSize, offset uint64, visit func(chdMetadata) error) error {
	const maxRecords = 1024
	seen := make(map[uint64]struct{})
	type extent struct{ start, end uint64 }
	var extents []extent
	for offset != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var header [16]byte
		if _, duplicate := seen[offset]; duplicate || len(seen) == maxRecords || offset < headerSize || offset > size || size-offset < uint64(len(header)) {
			return fmt.Errorf("invalid CHD metadata chain")
		}
		seen[offset] = struct{}{}
		if _, err := src.ReadAt(header[:], int64(offset)); err != nil {
			return err
		}
		length := uint64(binary.BigEndian.Uint32(header[4:8]) & 0x00ffffff)
		if length > size-offset-uint64(len(header)) {
			return fmt.Errorf("truncated CHD metadata")
		}
		extents = append(extents, extent{offset, offset + uint64(len(header)) + length})
		if visit != nil {
			entry := chdMetadata{tag: string(header[:4]), flags: header[4], offset: int64(offset) + int64(len(header)), length: length}
			if err := visit(entry); err != nil {
				return err
			}
		}
		offset = binary.BigEndian.Uint64(header[8:16])
	}
	slices.SortFunc(extents, func(a, b extent) int {
		return cmp.Compare(a.start, b.start)
	})
	for i := 1; i < len(extents); i++ {
		if extents[i].start < extents[i-1].end {
			return fmt.Errorf("overlapping CHD metadata records")
		}
	}
	return ctx.Err()
}
