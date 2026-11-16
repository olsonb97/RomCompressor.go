package trim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
)

const (
	xciPageSize       = 0x200
	xciSecondMagic    = xciPageSize + 0x100
	xciCertificateEnd = 0xf000 // Includes the reserved part of the certificate area.
	xciHFSFixedSize   = 0x10
	xciHFSEntrySize   = 0x40
	xciMaxHFSHeader   = 1 << 20
)

// XCIBoundary checks the ordinary Switch gamecard layout and returns the
// declared end of its valid data. The caller verifies all trailing padding.
// The checks cover HFS0 structure and hashes, not header RSA authenticity.
func XCIBoundary(ctx context.Context, src io.ReaderAt, size int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if size < xciCertificateEnd+xciPageSize || size%xciPageSize != 0 {
		return 0, fmt.Errorf("invalid XCI image length")
	}
	var header [xciSecondMagic + 4]byte
	if _, err := src.ReadAt(header[:], 0); err != nil {
		return 0, fmt.Errorf("read XCI header: %w", err)
	}
	if string(header[0x100:0x104]) != "HEAD" || !xciHasSignature(header[:0x100]) ||
		binary.LittleEndian.Uint32(header[0x108:0x10c]) != 0xffffffff ||
		binary.LittleEndian.Uint32(header[0x11c:0x120]) != 0 {
		return 0, fmt.Errorf("invalid XCI gamecard header")
	}
	securityMode := binary.LittleEndian.Uint32(header[0x180:0x184])
	if header[0x10e] > 3 || header[0x10f]&0x80 != 0 || securityMode == 2 || string(header[xciSecondMagic:]) == "HEAD" {
		return 0, fmt.Errorf("unsupported XCI gamecard header variant")
	}
	capacity := xciCapacity(header[0x10d])
	if capacity == 0 || size > capacity {
		return 0, fmt.Errorf("XCI image length disagrees with its cartridge capacity")
	}
	// The end page is inclusive (nxdumptool's GetTrimmedSize).
	end := (int64(binary.LittleEndian.Uint32(header[0x118:0x11c])) + 1) * xciPageSize
	if end > size || end < xciCertificateEnd+xciPageSize {
		return 0, fmt.Errorf("XCI declared data end lies outside the image")
	}
	limitAreaPage := int64(binary.LittleEndian.Uint32(header[0x18c:0x190]))
	if limitAreaPage*xciPageSize >= end {
		return 0, fmt.Errorf("XCI limit area page lies beyond the declared data end")
	}
	rootAt := binary.LittleEndian.Uint64(header[0x130:0x138])
	if rootAt < xciCertificateEnd || rootAt > uint64(end-xciPageSize) || rootAt%xciPageSize != 0 {
		return 0, fmt.Errorf("XCI root HFS0 would overlap cartridge metadata or extend beyond valid data")
	}
	root, rootEntries, err := readXCIHFS0(ctx, src, int64(rootAt), end-int64(rootAt))
	if err != nil {
		return 0, fmt.Errorf("XCI root HFS0: %w", err)
	}
	if len(rootEntries) == 0 || len(rootEntries) > 4 ||
		uint64(len(root)) != binary.LittleEndian.Uint64(header[0x138:0x140]) {
		return 0, fmt.Errorf("invalid XCI root HFS0 layout")
	}
	secureAt := int64(binary.LittleEndian.Uint32(header[0x104:0x108])) * xciPageSize
	if secureAt < int64(rootAt)+int64(len(root)) || secureAt >= end {
		return 0, fmt.Errorf("XCI declares a secure storage start outside the partition data area")
	}
	if !matchesXCIRootHash(root, header[0x140:0x160]) {
		return 0, fmt.Errorf("XCI root HFS0 hash mismatch")
	}
	if err := validateXCIPartitions(ctx, src, rootEntries, secureAt); err != nil {
		return 0, err
	}
	if err := validateXCICompactRoot(ctx, src, root, rootEntries); err != nil {
		return 0, err
	}
	return end, ctx.Err()
}

func xciHasSignature(signature []byte) bool {
	return bytes.Count(signature, []byte{signature[0]}) != len(signature)
}

func xciCapacity(indicator byte) int64 {
	switch indicator {
	case 0xfa:
		return 1 << 30
	case 0xf8:
		return 2 << 30
	case 0xf0:
		return 4 << 30
	case 0xe0:
		return 8 << 30
	case 0xe1:
		return 16 << 30
	case 0xe2:
		return 32 << 30
	default:
		return 0
	}
}

func matchesXCIRootHash(header, expected []byte) bool {
	digest := sha256.Sum256(header)
	if bytes.Equal(digest[:], expected) {
		return true
	}
	// Terra (China) adds compatibility byte 1 to the root digest input.
	hash := sha256.New()
	hash.Write(header)
	hash.Write([]byte{1})
	return bytes.Equal(hash.Sum(nil), expected)
}

func validateXCICompactRoot(ctx context.Context, src io.ReaderAt, root []byte, partitions []xciHFSEntry) error {
	namesSize := int64(binary.LittleEndian.Uint32(root[8:12]))
	compactSize := xciHFSFixedSize + int64(len(partitions))*xciHFSEntrySize + namesSize
	gap := int64(len(root)) - compactSize
	if gap == 0 {
		return nil
	}
	// hactool locates partitions using the unrounded root header size.
	// A second set of HFS0 directories can declare different file extents.
	for _, partition := range partitions {
		if err := ctx.Err(); err != nil {
			return err
		}
		var magic [4]byte
		if _, err := src.ReadAt(magic[:], partition.start-gap); err != nil {
			return fmt.Errorf("read compact XCI partition header: %w", err)
		}
		if string(magic[:]) != "HFS0" {
			return nil
		}
	}
	return fmt.Errorf("ambiguous HFS0 partition alignment")
}

type xciHFSEntry struct {
	name     string
	start    int64
	size     int64
	hashAt   int64
	hashSize int64
	hash     [sha256.Size]byte
}

func readXCIHFS0(ctx context.Context, src io.ReaderAt, start, available int64) ([]byte, []xciHFSEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if available < xciHFSFixedSize {
		return nil, nil, fmt.Errorf("truncated HFS0 header")
	}
	var fixed [xciHFSFixedSize]byte
	if _, err := src.ReadAt(fixed[:], start); err != nil {
		return nil, nil, err
	}
	if string(fixed[:4]) != "HFS0" {
		return nil, nil, fmt.Errorf("missing HFS0 signature")
	}
	count := int64(binary.LittleEndian.Uint32(fixed[4:8]))
	namesSize := int64(binary.LittleEndian.Uint32(fixed[8:12]))
	tableEnd := xciHFSFixedSize + count*xciHFSEntrySize
	namesEnd := tableEnd + namesSize
	// The gamecard payload follows the page-aligned header, even when
	// the declared name table excludes the alignment bytes.
	headerSize := (namesEnd + xciPageSize - 1) &^ (xciPageSize - 1)
	if namesSize == 0 || headerSize > available || headerSize > xciMaxHFSHeader {
		return nil, nil, fmt.Errorf("invalid HFS0 header size")
	}
	header := make([]byte, headerSize)
	if _, err := src.ReadAt(header, start); err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(fixed[:], header[:xciHFSFixedSize]) {
		return nil, nil, fmt.Errorf("HFS0 header changed during read")
	}
	payloadAt, payloadSize := start+headerSize, uint64(available-headerSize)
	entries, err := parseXCIHFSEntries(ctx, header[xciHFSFixedSize:tableEnd], header[tableEnd:namesEnd], payloadAt, payloadSize)
	if err != nil {
		return nil, nil, err
	}
	return header, entries, nil
}

func parseXCIHFSEntries(ctx context.Context, table, names []byte, payloadAt int64, payloadSize uint64) ([]xciHFSEntry, error) {
	entries := make([]xciHFSEntry, 0, len(table)/xciHFSEntrySize)
	usedNames := make(map[string]bool)
	for len(table) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record := table[:xciHFSEntrySize]
		table = table[xciHFSEntrySize:]
		offset := binary.LittleEndian.Uint64(record[:8])
		length := binary.LittleEndian.Uint64(record[8:16])
		hashAt := binary.LittleEndian.Uint64(record[24:32])
		hashSize := uint64(binary.LittleEndian.Uint32(record[20:24]))
		if offset > payloadSize || length > payloadSize-offset || hashAt > length || hashSize > length-hashAt {
			return nil, fmt.Errorf("HFS0 file or hash region extends past its partition")
		}
		nameAt := binary.LittleEndian.Uint32(record[16:20])
		name, err := xciHFSName(names, nameAt)
		if err != nil {
			return nil, err
		}
		if usedNames[name] {
			return nil, fmt.Errorf("duplicate HFS0 filename")
		}
		usedNames[name] = true
		entry := xciHFSEntry{name: name, start: payloadAt + int64(offset), size: int64(length), hashAt: int64(hashAt), hashSize: int64(hashSize)}
		copy(entry.hash[:], record[32:64])
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b xciHFSEntry) int {
		if a.start < b.start {
			return -1
		}
		if a.start > b.start {
			return 1
		}
		return 0
	})
	var lastEnd int64
	for _, entry := range entries {
		if entry.size == 0 {
			continue
		}
		if entry.start < lastEnd {
			return nil, fmt.Errorf("overlapping HFS0 entries")
		}
		lastEnd = entry.start + entry.size
	}
	return entries, nil
}

func xciHFSName(names []byte, at uint32) (string, error) {
	if uint64(at) >= uint64(len(names)) {
		return "", fmt.Errorf("HFS0 filename starts outside its name table")
	}
	name := names[at:]
	// Names inside gamecard partitions are short, and a malformed table
	// should not turn repeated name lookups into unbounded scanning.
	end := bytes.IndexByte(name[:min(256, len(name))], 0)
	if end <= 0 || bytes.ContainsAny(name[:end], "/\\") {
		return "", fmt.Errorf("invalid HFS0 filename")
	}
	return string(name[:end]), nil
}

func validateXCIPartitions(ctx context.Context, src io.ReaderAt, partitions []xciHFSEntry, secureAt int64) error {
	secure := false
	var buffer [32 << 10]byte
	for _, partition := range partitions {
		switch partition.name {
		case "secure":
			if partition.start != secureAt {
				return fmt.Errorf("XCI secure partition offset disagrees with the card header")
			}
		case "update", "normal", "logo":
		default:
			return fmt.Errorf("unknown XCI root partition %q", partition.name)
		}
		if partition.start%xciPageSize != 0 || partition.size%xciPageSize != 0 {
			return fmt.Errorf("unaligned XCI %s partition", partition.name)
		}
		header, files, err := readXCIHFS0(ctx, src, partition.start, partition.size)
		if err != nil {
			return fmt.Errorf("XCI %s partition: %w", partition.name, err)
		}
		if partition.hashAt != 0 || partition.hashSize != int64(len(header)) {
			return fmt.Errorf("XCI %s partition header hash region must cover the full directory", partition.name)
		}
		hash := sha256.Sum256(header)
		if hash != partition.hash {
			return fmt.Errorf("XCI %s partition header hash mismatch", partition.name)
		}
		for _, file := range files {
			if file.hashSize == 0 {
				continue
			}
			if err := validateXCIFileHash(ctx, src, file, buffer[:]); err != nil {
				return fmt.Errorf("XCI %s partition: %w", partition.name, err)
			}
		}
		if partition.name == "secure" {
			secure = slices.ContainsFunc(files, func(file xciHFSEntry) bool { return file.size > 0 })
		}
	}
	if !secure {
		return fmt.Errorf("XCI is missing its secure game partition")
	}
	return nil
}

func validateXCIFileHash(ctx context.Context, src io.ReaderAt, file xciHFSEntry, buffer []byte) error {
	hash := sha256.New()
	for offset := int64(0); offset < file.hashSize; {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := buffer[:min(int64(len(buffer)), file.hashSize-offset)]
		if _, err := src.ReadAt(part, file.start+file.hashAt+offset); err != nil {
			return fmt.Errorf("read HFS0 file %q hash region: %w", file.name, err)
		}
		hash.Write(part)
		offset += int64(len(part))
	}
	if !bytes.Equal(hash.Sum(nil), file.hash[:]) {
		return fmt.Errorf("HFS0 file hash mismatch for %q", file.name)
	}
	return nil
}
