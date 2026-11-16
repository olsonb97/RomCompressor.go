package romcompressor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

const (
	wuaFooterSize = 144
	wuaBlockSize  = 65536
	wuaMaxNodes   = 100000
	wuaMaxPath    = 1024
	wuaDirectory  = -1
)

type wuaManifest struct {
	entries map[string]int64 // Slash-separated paths. Directories use wuaDirectory.
	size    int64
}

func inspectWUA(ctx context.Context, src io.ReaderAt, size int64) (FileInfo, error) {
	manifest, _, err := readWUA(ctx, src, size, defaultMaxOutput, false)
	if err != nil {
		return FileInfo{}, err
	}
	info := FileInfo{Format: WUA, Size: size}
	for _, path := range wuaPaths(manifest.entries) {
		if size := manifest.entries[path]; size >= 0 {
			info.Entries = append(info.Entries, Entry{Name: path, Size: uint64(size)})
		}
	}
	return info, nil
}

// ZArchive v1 places its signature and six section ranges in a big-endian footer.
// Validate the directory and block geometry before invoking the external reader.
func readWUA(ctx context.Context, src io.ReaderAt, size, limit int64, checkFrames bool) (wuaManifest, [wuaFooterSize]byte, error) {
	var footer [wuaFooterSize]byte
	var manifest wuaManifest
	if size <= wuaFooterSize {
		return manifest, footer, fmt.Errorf("truncated WUA footer")
	}
	footerAt := size - wuaFooterSize
	if _, err := src.ReadAt(footer[:], footerAt); err != nil {
		return manifest, footer, err
	}
	if binary.BigEndian.Uint64(footer[136:]) != 0x61bf3a01169f52d6 || binary.BigEndian.Uint64(footer[128:]) != uint64(size) {
		return manifest, footer, fmt.Errorf("invalid WUA signature, version, or file size")
	}
	type section struct{ offset, length int64 }
	var sections [6]section
	var end uint64
	for i := range sections {
		offset := binary.BigEndian.Uint64(footer[16*i:])
		length := binary.BigEndian.Uint64(footer[16*i+8:])
		if offset < end || offset > uint64(footerAt) || length > uint64(footerAt)-offset {
			return manifest, footer, fmt.Errorf("WUA section is outside the archive or overlaps another section")
		}
		sections[i] = section{int64(offset), int64(length)}
		end = offset + length
	}
	data, blocks, names, nodes := sections[0], sections[1], sections[2], sections[3]
	if blocks.length == 0 || blocks.length%40 != 0 || blocks.length > (defaultMaxOutput/wuaBlockSize/16)*40 ||
		names.length == 0 || names.length > 128*wuaMaxNodes || nodes.length == 0 || nodes.length%16 != 0 || nodes.length/16 > wuaMaxNodes {
		return manifest, footer, fmt.Errorf("WUA index exceeds supported bounds or has an invalid size")
	}
	var compressed *io.SectionReader
	if checkFrames {
		compressed = io.NewSectionReader(src, data.offset, data.length)
	}
	expanded, finalStored, err := wuaDataLength(ctx, io.NewSectionReader(src, blocks.offset, blocks.length), compressed, data.length, limit)
	if err != nil {
		return manifest, footer, err
	}
	nameTable := make([]byte, names.length)
	if _, err := src.ReadAt(nameTable, names.offset); err != nil {
		return manifest, footer, err
	}
	nodeTable := make([]byte, nodes.length)
	if _, err := src.ReadAt(nodeTable, nodes.offset); err != nil {
		return manifest, footer, err
	}
	manifest, err = readWUAManifest(ctx, nameTable, nodeTable, expanded, limit)
	if err != nil {
		return manifest, footer, err
	}
	if checkFrames && finalStored && manifest.size < expanded {
		if err := ctx.Err(); err != nil {
			return manifest, footer, err
		}
		// ZArchive fills the final partial block with zeros. A stored
		// block exposes that padding directly without decompression.
		padding := make([]byte, expanded-manifest.size)
		if _, err := src.ReadAt(padding, data.offset+data.length-int64(len(padding))); err != nil {
			return manifest, footer, err
		}
		for _, value := range padding {
			if value != 0 {
				return manifest, footer, fmt.Errorf("WUA final stored block contains nonzero padding")
			}
		}
	}
	return manifest, footer, ctx.Err()
}

// Each 40-byte block record holds a base offset and the lengths minus one of
// 16 consecutive data blocks. An unused tail of the last record is zeroed.
func wuaDataLength(ctx context.Context, index io.Reader, compressed *io.SectionReader, dataSize, limit int64) (expanded int64, finalStored bool, err error) {
	reader := bufio.NewReaderSize(index, 64<<10)
	var record [40]byte
	var offset, blocks, finalLength int64
	var buffer []byte
	var frame bytes.Reader
	if compressed != nil {
		buffer = make([]byte, wuaBlockSize)
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if _, err := io.ReadFull(reader, record[:]); err == io.EOF {
			break
		} else if err != nil {
			return 0, false, err
		}
		if offset >= dataSize || binary.BigEndian.Uint64(record[:]) != uint64(offset) {
			return 0, false, fmt.Errorf("invalid WUA block offset")
		}
		for i := 8; i < len(record); i += 2 {
			stored := int64(binary.BigEndian.Uint16(record[i:])) + 1
			if offset == dataSize && stored == 1 {
				continue
			}
			if stored > dataSize-offset {
				return 0, false, fmt.Errorf("WUA block exceeds the data section")
			}
			if limit <= 0 || blocks > (limit-1)/wuaBlockSize {
				return 0, false, fmt.Errorf("WUA data blocks exceed the expansion limit")
			}
			if compressed != nil && stored != wuaBlockSize {
				if _, err := compressed.ReadAt(buffer[:stored], offset); err != nil {
					return 0, false, fmt.Errorf("read WUA compressed block %d: %w", blocks+1, err)
				}
				frame.Reset(buffer[:stored])
				if err := codec.ValidateZstandardFrame(ctx, &frame, wuaBlockSize); err != nil {
					return 0, false, fmt.Errorf("WUA compressed block %d: %w", blocks+1, err)
				}
			}
			offset += stored
			blocks++
			finalLength = stored
		}
	}
	if offset != dataSize {
		return 0, false, fmt.Errorf("WUA block index does not cover the data section")
	}
	return blocks * wuaBlockSize, finalLength == wuaBlockSize, nil
}

func readWUAManifest(ctx context.Context, names, nodes []byte, expanded, limit int64) (wuaManifest, error) {
	manifest := wuaManifest{entries: make(map[string]int64)}
	type extent struct{ start, end int64 }
	var files []extent
	count := len(nodes) / 16
	if binary.BigEndian.Uint32(nodes) != 0x7fffffff {
		return manifest, fmt.Errorf("invalid WUA root directory")
	}
	seen := make([]bool, count)
	seen[0] = true
	var visit func(int, string) error
	visit = func(i int, path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := nodes[i*16 : (i+1)*16]
		kind := binary.BigEndian.Uint32(node)
		first := binary.BigEndian.Uint32(node[4:])
		second := binary.BigEndian.Uint32(node[8:])
		high := binary.BigEndian.Uint32(node[12:])
		if kind>>31 != 0 {
			offset := int64(first) | int64(high&0xffff)<<32
			size := int64(second) | int64(high>>16)<<32
			if offset > expanded || size > expanded-offset || size > limit-manifest.size {
				return fmt.Errorf("WUA file data exceeds archive or expansion bounds")
			}
			if size != 0 {
				files = append(files, extent{start: offset, end: offset + size})
			}
			manifest.entries[path] = size
			manifest.size += size
			return nil
		}
		if uint64(first)+uint64(second) > uint64(count) {
			return fmt.Errorf("WUA directory has an invalid child range")
		}
		if path != "" {
			manifest.entries[path] = wuaDirectory
			path += "/"
		}
		siblings := make(map[string]struct{})
		for child := int(first); child < int(first+second); child++ {
			if seen[child] {
				return fmt.Errorf("WUA directory contains a cycle or a shared node")
			}
			seen[child] = true
			nameOffset := int(binary.BigEndian.Uint32(nodes[child*16:]) & 0x7fffffff)
			if nameOffset >= len(names) {
				return fmt.Errorf("invalid WUA name offset")
			}
			n := int(names[nameOffset])
			if n > 127 || n > len(names)-nameOffset-1 {
				return fmt.Errorf("WUA path component is truncated or unsupported by ZArchive")
			}
			name := string(names[nameOffset+1 : nameOffset+1+n])
			if !safeWUANodeName(name) || len(path)+len(name) > wuaMaxPath {
				return fmt.Errorf("WUA contains an unsafe or unsupported path")
			}
			folded := strings.ToLower(name)
			if _, duplicate := siblings[folded]; duplicate {
				return fmt.Errorf("WUA directory contains duplicate names")
			}
			siblings[folded] = struct{}{}
			if err := visit(child, path+name); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(0, ""); err != nil {
		return manifest, err
	}
	if len(manifest.entries) != count-1 {
		return manifest, fmt.Errorf("unreachable WUA directory nodes")
	}
	// The writer appends file bytes in order, with padding only at the end
	// of the final block. Tree node order can differ from data order.
	sort.Slice(files, func(i, j int) bool { return files[i].start < files[j].start })
	var end int64
	for _, file := range files {
		if file.start != end {
			return manifest, fmt.Errorf("WUA file extents overlap or leave an unmapped gap")
		}
		end = file.end
	}
	if expanded-end >= wuaBlockSize {
		return manifest, fmt.Errorf("WUA file extents leave unused data blocks")
	}
	return manifest, validateWUALayout(manifest)
}

func safeWUANodeName(name string) bool {
	if len(name) == 0 || len(name) > 127 || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || strings.ContainsAny(name, `<>:"/\|?*`) {
		return false
	}
	for i := range len(name) {
		if name[i] < 0x20 || name[i] > 0x7e {
			return false
		}
	}
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "AUX", "CON", "CONIN$", "CONOUT$", "NUL", "PRN":
		return false
	}
	return len(base) != 4 || (base[:3] != "COM" && base[:3] != "LPT") || base[3] < '0' || base[3] > '9'
}

func wuaTitleFolder(name string) bool {
	id, version, found := strings.Cut(name, "_v")
	if !found || len(id) != 16 || len(version) == 0 || len(version) > 5 || len(version) > 1 && version[0] == '0' {
		return false
	}
	return unsignedWUAValue(id, 16, 64) && unsignedWUAValue(version, 10, 16)
}

func unsignedWUAValue(text string, base, bits int) bool {
	if strings.HasPrefix(text, "+") {
		return false
	}
	_, err := strconv.ParseUint(text, base, bits)
	return err == nil
}

func validateWUALayout(manifest wuaManifest) error {
	if len(manifest.entries) == 0 {
		return fmt.Errorf("WUA needs at least one decrypted Wii U title")
	}
	for name, size := range manifest.entries {
		if strings.Contains(name, "/") {
			continue
		}
		if size != wuaDirectory || !wuaTitleFolder(name) {
			return fmt.Errorf("WUA root must contain title directories named <title ID>_v<version>")
		}
		// Cemu validates these XML files. Its writer preserves whatever other
		// directories the mounted title contains, including absent content.
		for _, xml := range []string{"code/app.xml", "code/cos.xml", "meta/meta.xml"} {
			if manifest.entries[name+"/"+xml] <= 0 {
				return fmt.Errorf("WUA title %s is missing %s", name, xml)
			}
		}
	}
	return nil
}

func checkWUAChecksum(ctx context.Context, src io.ReaderAt, size int64, footer [wuaFooterSize]byte) error {
	hash := sha256.New()
	data := io.NewSectionReader(src, 0, size-wuaFooterSize)
	n, err := io.Copy(hash, &fileops.ContextReader{Context: ctx, Reader: data})
	if err != nil {
		return err
	}
	if n != size-wuaFooterSize {
		return io.ErrUnexpectedEOF
	}
	var expected [32]byte
	copy(expected[:], footer[96:128])
	clear(footer[96:128])
	_, _ = hash.Write(footer[:])
	if !bytes.Equal(expected[:], hash.Sum(nil)) {
		return fmt.Errorf("WUA archive checksum mismatch")
	}
	return ctx.Err()
}
