package trim

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
)

type ndsNameTableDirectory struct {
	start     int64
	firstFile int64
}

func validateNDSFileNames(ctx context.Context, src io.ReaderAt, header []byte) error {
	offset, size := ndsWord(header, 0x40), ndsWord(header, 0x44)
	if size == 0 {
		return nil
	}
	const recordSize = 8
	var root [recordSize]byte
	if _, err := src.ReadAt(root[:], offset); err != nil {
		return fmt.Errorf("read DS file name table root: %w", err)
	}
	count := int(binary.LittleEndian.Uint16(root[6:]))
	tableSize := int64(count * recordSize)
	if count == 0 || count > 1<<12 || tableSize >= size {
		return fmt.Errorf("invalid DS file name table directory count")
	}
	table := make([]byte, tableSize)
	if _, err := src.ReadAt(table, offset); err != nil {
		return fmt.Errorf("read DS file name table directory records: %w", err)
	}
	dirs := make([]ndsNameTableDirectory, 0, count)
	for i := range count {
		row := table[i*recordSize:]
		start := ndsWord(row, 0)
		if start < tableSize || start >= size {
			return fmt.Errorf("DS file name table directory %d points outside its name data", i)
		}
		if i != 0 && !validNDSDirectoryID(binary.LittleEndian.Uint16(row[6:]), count) {
			return fmt.Errorf("DS file name table directory %d names an invalid parent", i)
		}
		dirs = append(dirs, ndsNameTableDirectory{start: start, firstFile: int64(binary.LittleEndian.Uint16(row[4:]))})
	}
	// Use the sorted starts to bound name scans. Multiple directory rows
	// can refer to the same stream, which is scanned once.
	slices.SortFunc(dirs, func(a, b ndsNameTableDirectory) int { return cmp.Compare(a.start, b.start) })
	var section io.SectionReader
	names := bufio.NewReader(&section)
	fatCount := ndsWord(header, 0x4c) / 8
	for len(dirs) != 0 {
		start := dirs[0].start
		end := 1
		for end < len(dirs) && dirs[end].start == start {
			end++
		}
		limit := size
		if end < len(dirs) {
			limit = dirs[end].start
		}
		section = *io.NewSectionReader(src, offset+start, limit-start)
		names.Reset(&section)
		files, err := scanNDSFileNames(ctx, names, count, fatCount)
		if err != nil {
			return err
		}
		for _, dir := range dirs[:end] {
			if files != 0 && dir.firstFile+files > fatCount {
				return fmt.Errorf("DS file name table points beyond the file allocation table")
			}
		}
		dirs = dirs[end:]
	}
	return nil
}

func scanNDSFileNames(ctx context.Context, names *bufio.Reader, dirCount int, fatCount int64) (int64, error) {
	var files int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		tag, err := names.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("read DS file name table terminator: %w", err)
		}
		if tag == 0 {
			return files, nil
		}
		length := int(tag & 0x7f)
		if length == 0 {
			return 0, fmt.Errorf("empty name in DS file name table")
		}
		if _, err := names.Discard(length); err != nil {
			return 0, fmt.Errorf("read DS file name table entry: %w", err)
		}
		if tag&0x80 != 0 {
			var child [2]byte
			if _, err := io.ReadFull(names, child[:]); err != nil {
				return 0, fmt.Errorf("read DS file name table child: %w", err)
			}
			if !validNDSDirectoryID(binary.LittleEndian.Uint16(child[:]), dirCount) {
				return 0, fmt.Errorf("DS file name table points outside the directory records")
			}
		} else {
			files++
			if files > fatCount {
				return 0, fmt.Errorf("DS file name table points beyond the file allocation table")
			}
		}
	}
}

func validNDSDirectoryID(id uint16, count int) bool {
	return id&0xf000 == 0xf000 && int(id&0x0fff) < count
}
