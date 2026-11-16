package codec

import (
	"archive/zip"
	"bufio"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

const defaultLimit int64 = 1 << 40

func normalize(ctx context.Context, size int64, opts Options) (Options, error) {
	if err := ctx.Err(); err != nil {
		return opts, err
	}
	if size < 0 || opts.MaxOutput < 0 {
		return opts, errors.New("negative input size or output limit")
	}
	if opts.MaxOutput == 0 {
		opts.MaxOutput = defaultLimit
	}
	return opts, nil
}

func encode(ctx context.Context, format string, src io.ReaderAt, size int64, dst io.WriteSeeker, opts Options) error {
	var err error
	opts, err = normalize(ctx, size, opts)
	if err != nil {
		return err
	}
	if size > opts.MaxOutput {
		return errors.New("input exceeds expanded size limit")
	}
	name := path.Base(strings.ReplaceAll(opts.Name, "\\", "/"))
	if name == "." || name == ".." || name == "/" || name == "" {
		name = "game.bin"
	}
	switch format {
	case "zip":
		w := zip.NewWriter(dst)
		entry, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			return err
		}
		err = copyStream(ctx, entry, io.NewSectionReader(src, 0, size), size, opts)
		return errors.Join(err, w.Close())
	case "gzip":
		w := gzip.NewWriter(dst)
		// GZIP's optional filename is Latin-1. Omit it for Unicode names rather
		// than changing the user's filename or failing a valid compression job.
		w.Name = gzipName(name)
		err := copyStream(ctx, w, io.NewSectionReader(src, 0, size), size, opts)
		return errors.Join(err, w.Close())
	case "cso", "zso":
		return encodeCSO(ctx, format, src, size, dst, opts)
	case "ecm":
		return encodeECM(ctx, src, size, dst, opts)
	case "wbfs":
		return encodeWBFS(ctx, src, size, dst, opts)
	case "ciso":
		return encodeCISO(ctx, src, size, dst, opts)
	case "wux":
		return encodeWUX(ctx, src, size, dst, opts)
	case "cci":
		return encodeCCI(ctx, src, size, dst, opts)
	default:
		return fmt.Errorf("unsupported native output format %q", format)
	}
}

func gzipName(name string) string {
	for _, char := range name {
		if char == 0 || char > 255 {
			return ""
		}
	}
	return name
}

func decode(ctx context.Context, format string, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	var err error
	opts, err = normalize(ctx, size, opts)
	if err != nil {
		return err
	}
	switch format {
	case "zip":
		archive, err := OpenZIP(ctx, src, size)
		if err != nil {
			return err
		}
		selected := -1
		for index, f := range archive.File {
			if err := ctx.Err(); err != nil {
				return err
			}
			if f.Mode().IsDir() {
				continue
			}
			if opts.Entry != "" && f.Name != opts.Entry {
				continue
			}
			if selected >= 0 {
				return errors.New("ZIP contains multiple matching files; select one entry")
			}
			selected = index
		}
		if selected < 0 {
			return errors.New("ZIP entry was not found")
		}
		if err := archive.ValidateMember(selected); err != nil {
			return err
		}
		entry := archive.File[selected]
		if entry.UncompressedSize64 > uint64(opts.MaxOutput) {
			return errors.New("ZIP entry exceeds expanded size limit")
		}
		return CopyZIPMember(ctx, dst, entry, nil, opts.Progress)
	case "gzip":
		reader := contextReaderAt{ReaderAt: src, ctx: ctx}
		stream := &gzipMembers{ctx: ctx, source: bufio.NewReader(io.NewSectionReader(reader, 0, size))}
		if err := stream.next(); err != nil {
			return fmt.Errorf("read GZIP: %w", err)
		}
		defer stream.member.Close()
		return copyStream(ctx, dst, stream, -1, opts)
	case "cso", "zso":
		return decodeCSO(ctx, format, src, size, dst, opts)
	case "ecm":
		return decodeECM(ctx, src, size, dst, opts)
	case "wbfs":
		return decodeWBFS(ctx, src, size, dst, opts)
	case "ciso":
		return decodeCISO(ctx, src, size, dst, opts)
	case "wux":
		return decodeWUX(ctx, src, size, dst, opts)
	case "cci":
		return decodeCCI(ctx, src, size, dst, opts)
	default:
		return fmt.Errorf("unsupported native input format %q", format)
	}
}

type gzipMembers struct {
	ctx    context.Context
	source *bufio.Reader
	member gzip.Reader
}

func (r *gzipMembers) next() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	header, err := r.source.Peek(4)
	if err == io.EOF && len(header) > 0 {
		return io.ErrUnexpectedEOF
	}
	if err != nil {
		return err
	}
	// compress/gzip ignores reserved bits. Check each header before handing
	// the member to it for DEFLATE and trailer validation.
	if header[3]&0xe0 != 0 {
		return gzip.ErrHeader
	}
	if err := r.member.Reset(r.source); err != nil {
		return err
	}
	r.member.Multistream(false)
	return nil
}

func (r *gzipMembers) Read(p []byte) (int, error) {
	for {
		n, err := r.member.Read(p)
		if err != io.EOF {
			return n, err
		}
		if err := r.next(); n != 0 || err != nil {
			return n, err
		}
	}
}

// OpenZIP bounds central directory metadata before archive/zip parses it.
func OpenZIP(ctx context.Context, src io.ReaderAt, size int64) (*ZIPArchive, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader := contextReaderAt{ReaderAt: src, ctx: ctx}
	directory, err := validateZIPDirectory(reader, size)
	if err != nil {
		return nil, err
	}
	dataEnd := int64(directory.offset)
	offsets, err := readZIPLocalOffsets(reader, dataEnd, int64(directory.size), directory.entries)
	if err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("read ZIP: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(archive.File) != len(offsets) {
		return nil, errors.New("ZIP directory changed while opening")
	}
	result := &ZIPArchive{Reader: archive, source: reader, members: make([]zipMember, len(offsets))}
	if err := result.boundMembers(offsets, dataEnd); err != nil {
		return nil, err
	}
	return result, nil
}

// ZIPArchive holds the validated physical layout of the members.
type ZIPArchive struct {
	*zip.Reader
	source  contextReaderAt
	members []zipMember
}

type zipMember struct {
	localAt int64
	dataAt  int64
}

const zipLocalHeaderSize = 30

type contextReaderAt struct {
	io.ReaderAt
	ctx context.Context
}

func (reader contextReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.ReaderAt.ReadAt(p, offset)
}

// Bound the central directory before archive/zip allocates entries. ZIP64 is
// supported; split archives and self-extracting offsets are not accepted.
func validateZIPDirectory(src io.ReaderAt, size int64) (zipDirectory, error) {
	if size < 22 {
		return zipDirectory{}, errors.New("truncated ZIP directory")
	}
	tailSize := min(size, 65557)
	tail := make([]byte, tailSize)
	if _, err := src.ReadAt(tail, size-tailSize); err != nil {
		return zipDirectory{}, err
	}
	for pos := len(tail) - 22; pos >= 0; pos-- {
		end := tail[pos:]
		if binary.LittleEndian.Uint32(end) != 0x06054b50 {
			continue
		}
		// archive/zip uses the last end signature. Reject an inconsistent
		// comment length instead of validating an earlier record it won't use.
		if int(binary.LittleEndian.Uint16(end[20:])) != len(end)-22 {
			return zipDirectory{}, errors.New("ZIP end record has an invalid comment length or trailing data")
		}
		if binary.LittleEndian.Uint32(end[4:]) != 0 {
			return zipDirectory{}, errors.New("split ZIP archives are unsupported")
		}
		directory := zipDirectory{
			entries: uint64(binary.LittleEndian.Uint16(end[10:])),
			size:    uint64(binary.LittleEndian.Uint32(end[12:])),
			offset:  uint64(binary.LittleEndian.Uint32(end[16:])),
			end:     size - tailSize + int64(pos),
		}
		if uint64(binary.LittleEndian.Uint16(end[8:])) != directory.entries {
			return zipDirectory{}, errors.New("ZIP directory counts disagree")
		}
		zip64Sentinel := directory.entries == 0xffff || directory.offset == 0xffffffff
		// Go also checks for a locator when the actual directory size is 65,535.
		if zip64Sentinel || directory.size == 0xffff {
			wide, err := readZIP64Directory(src, directory.end)
			if err != nil {
				return zipDirectory{}, err
			}
			// A classic archive can contain exactly 65,535 entries.
			if wide == nil && directory.offset == 0xffffffff {
				return zipDirectory{}, errors.New("missing ZIP64 locator")
			}
			if wide != nil {
				if !zip64Sentinel && (wide.entries != directory.entries || wide.size != directory.size || wide.offset != directory.offset) {
					return zipDirectory{}, errors.New("ZIP and ZIP64 directory fields disagree")
				}
				directory = *wide
			}
		}
		if directory.entries > 100000 || directory.size > 64<<20 {
			return zipDirectory{}, errors.New("ZIP directory exceeds supported bounds")
		}
		// Using a different end offset would let archive/zip read beyond the
		// validated directory or infer a different start offset.
		if directory.offset > uint64(directory.end) || directory.size != uint64(directory.end)-directory.offset {
			return zipDirectory{}, errors.New("ZIP directory extent does not match its end record")
		}
		return directory, nil
	}
	return zipDirectory{}, errors.New("ZIP end record was not found")
}

type zipDirectory struct {
	entries uint64
	size    uint64
	offset  uint64
	end     int64
}

func readZIP64Directory(src io.ReaderAt, end int64) (*zipDirectory, error) {
	var locator [20]byte
	locatorAt := end - int64(len(locator))
	if locatorAt < 0 {
		return nil, nil
	}
	if _, err := src.ReadAt(locator[:], locatorAt); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(locator[:]) != 0x07064b50 || binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
		return nil, nil
	}
	var header [56]byte
	headerAt := binary.LittleEndian.Uint64(locator[8:])
	if headerAt > uint64(locatorAt) || uint64(locatorAt)-headerAt < uint64(len(header)) {
		return nil, errors.New("invalid ZIP64 directory offset")
	}
	if _, err := src.ReadAt(header[:], int64(headerAt)); err != nil {
		return nil, err
	}
	entries := binary.LittleEndian.Uint64(header[32:])
	if binary.LittleEndian.Uint32(header[:]) != 0x06064b50 ||
		binary.LittleEndian.Uint64(header[4:]) != uint64(locatorAt)-headerAt-12 ||
		binary.LittleEndian.Uint64(header[16:]) != 0 ||
		binary.LittleEndian.Uint64(header[24:]) != entries {
		return nil, errors.New("invalid ZIP64 directory")
	}
	return &zipDirectory{
		entries: entries,
		size:    binary.LittleEndian.Uint64(header[40:]),
		offset:  binary.LittleEndian.Uint64(header[48:]),
		end:     int64(headerAt),
	}, nil
}

func readZIPLocalOffsets(src io.ReaderAt, offset, size int64, expected uint64) ([]int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(src, offset, size), 64<<10)
	offsets := make([]int64, 0, expected)
	var consumed int64
	var header [46]byte
	for consumed < size {
		if uint64(len(offsets)) >= expected {
			return nil, errors.New("ZIP directory count is inconsistent")
		}
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, fmt.Errorf("read ZIP directory record: %w", err)
		}
		if binary.LittleEndian.Uint32(header[:]) != 0x02014b50 {
			return nil, errors.New("invalid ZIP central directory record")
		}
		nameLength := int(binary.LittleEndian.Uint16(header[28:]))
		extraLength := int(binary.LittleEndian.Uint16(header[30:]))
		commentLength := int(binary.LittleEndian.Uint16(header[32:]))
		if _, err := r.Discard(nameLength); err != nil {
			return nil, fmt.Errorf("read ZIP directory fields: %w", err)
		}
		localAt, err := zipLocalOffset(r, header[:], extraLength)
		if err != nil {
			return nil, err
		}
		if localAt > uint64(offset) {
			return nil, errors.New("ZIP local member header is outside the member data area")
		}
		if _, err := r.Discard(extraLength + commentLength); err != nil {
			return nil, fmt.Errorf("read ZIP directory fields: %w", err)
		}
		consumed += int64(len(header) + nameLength + extraLength + commentLength)
		offsets = append(offsets, int64(localAt))
	}
	if consumed != size || uint64(len(offsets)) != expected {
		return nil, errors.New("ZIP directory count or size is inconsistent")
	}
	return offsets, nil
}

func zipLocalOffset(r *bufio.Reader, central []byte, extraLength int) (uint64, error) {
	offset := uint64(binary.LittleEndian.Uint32(central[42:]))
	disk := uint32(binary.LittleEndian.Uint16(central[34:]))
	if offset == 0xffffffff || disk == 0xffff {
		extra, err := r.Peek(extraLength)
		if err != nil {
			return 0, fmt.Errorf("read ZIP64 member fields: %w", err)
		}
		wide := zip64Extra(extra)
		// Only fields with sentinel values appear in the ZIP64 extra.
		var skip int
		if binary.LittleEndian.Uint32(central[24:]) == 0xffffffff {
			skip += 8
		}
		if binary.LittleEndian.Uint32(central[20:]) == 0xffffffff {
			skip += 8
		}
		if offset == 0xffffffff {
			if len(wide) < skip+8 {
				return 0, errors.New("missing ZIP64 member offset")
			}
			offset = binary.LittleEndian.Uint64(wide[skip:])
			skip += 8
		}
		if disk == 0xffff {
			if len(wide) < skip+4 {
				return 0, errors.New("missing ZIP64 member disk number")
			}
			disk = binary.LittleEndian.Uint32(wide[skip:])
		}
	}
	if disk != 0 {
		return 0, errors.New("ZIP member starts on another disk")
	}
	return offset, nil
}

func (archive *ZIPArchive) boundMembers(offsets []int64, directoryAt int64) error {
	order := make([]int, len(offsets))
	for index := range order {
		order[index] = index
	}
	// Central directory order can differ from the physical file order.
	sort.Slice(order, func(i, j int) bool { return offsets[order[i]] < offsets[order[j]] })
	for position, index := range order {
		entry, localAt := archive.File[index], offsets[index]
		end := directoryAt
		if position+1 < len(order) {
			end = offsets[order[position+1]]
		}
		if end-localAt < zipLocalHeaderSize {
			return fmt.Errorf("ZIP local header for %q overlaps the next record", entry.Name)
		}
		dataAt, err := entry.DataOffset()
		if err != nil {
			return fmt.Errorf("read ZIP member header %q: %w", entry.Name, err)
		}
		if dataAt < localAt+zipLocalHeaderSize || dataAt > end || entry.CompressedSize64 > uint64(end-dataAt) {
			return fmt.Errorf("ZIP member extent for %q overlaps the next record", entry.Name)
		}
		if entry.Flags&8 != 0 {
			if err := archive.validateDescriptor(entry, dataAt+int64(entry.CompressedSize64), end); err != nil {
				return err
			}
		}
		archive.members[index] = zipMember{localAt: localAt, dataAt: dataAt}
	}
	return archive.source.ctx.Err()
}

func verifyZIP(ctx context.Context, src io.ReaderAt, size int64, opts Options) error {
	var err error
	opts, err = normalize(ctx, size, opts)
	if err != nil {
		return err
	}
	archive, err := OpenZIP(ctx, src, size)
	if err != nil {
		return err
	}
	var total int64
	var selected []*zip.File
	for index, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory := entry.Mode().IsDir()
		if opts.Entry != "" && (directory || entry.Name != opts.Entry) {
			continue
		}
		if err := archive.ValidateMember(index); err != nil {
			return err
		}
		if directory {
			continue
		}
		if entry.UncompressedSize64 > uint64(opts.MaxOutput-total) {
			return errors.New("ZIP expanded size exceeds output limit")
		}
		total += int64(entry.UncompressedSize64)
		selected = append(selected, entry)
	}
	if opts.Entry != "" && len(selected) != 1 {
		return errors.New("ZIP entry selection must match exactly one file")
	}
	buffer := make([]byte, 128<<10)
	var done int64
	for _, entry := range selected {
		var progress func(int64, int64)
		if opts.Progress != nil {
			progress = func(n, _ int64) {
				opts.Progress(done+n, total)
			}
		}
		if err := CopyZIPMember(ctx, io.Discard, entry, buffer, progress); err != nil {
			return fmt.Errorf("verify ZIP entry %q: %w", entry.Name, err)
		}
		done += int64(entry.UncompressedSize64)
	}
	return ctx.Err()
}

// ValidateMember checks the local header of the indexed member, its type,
// flags and compressed extent. It verifies directory entries have no payload.
func (archive *ZIPArchive) ValidateMember(index int) error {
	entry := archive.File[index]
	if entry.Flags&1 != 0 {
		return fmt.Errorf("ZIP entry %q is encrypted", entry.Name)
	}
	typ := entry.Mode().Type()
	switch typ {
	case fs.ModeDir:
		if entry.UncompressedSize64 != 0 || entry.CRC32 != 0 {
			return fmt.Errorf("invalid ZIP directory %q", entry.Name)
		}
	case 0:
	default:
		return fmt.Errorf("ZIP entry %q is a link or special file", entry.Name)
	}
	if entry.Method != zip.Store && entry.Method != zip.Deflate {
		return fmt.Errorf("ZIP entry %q: %w", entry.Name, zip.ErrAlgorithm)
	}
	if entry.Method == zip.Store && entry.CompressedSize64 != entry.UncompressedSize64 {
		return fmt.Errorf("ZIP stored entry %q: %w", entry.Name, zip.ErrFormat)
	}
	if err := archive.validateLocalHeader(entry, archive.members[index]); err != nil {
		return fmt.Errorf("ZIP local header for %q: %w", entry.Name, err)
	}
	// JAR writers can represent an empty directory as a DEFLATE stream.
	if typ == fs.ModeDir && entry.CompressedSize64 != 0 {
		var probe [1]byte
		if err := CopyZIPMember(archive.source.ctx, io.Discard, entry, probe[:], nil); err != nil {
			return fmt.Errorf("invalid ZIP directory %q: %w", entry.Name, err)
		}
	}
	return nil
}

func (archive *ZIPArchive) validateLocalHeader(entry *zip.File, member zipMember) error {
	var header [zipLocalHeaderSize]byte
	if _, err := archive.source.ReadAt(header[:], member.localAt); err != nil {
		return err
	}
	flags := binary.LittleEndian.Uint16(header[6:])
	const layoutFlags = 1 | 1<<3 | 1<<6 | 1<<13
	if binary.LittleEndian.Uint32(header[:]) != 0x04034b50 ||
		flags&layoutFlags != entry.Flags&layoutFlags ||
		binary.LittleEndian.Uint16(header[8:]) != entry.Method {
		return errors.New("flags or compression method disagree with the directory")
	}
	nameLength := int(binary.LittleEndian.Uint16(header[26:]))
	extraLength := int(binary.LittleEndian.Uint16(header[28:]))
	nameAt := member.localAt + int64(len(header))
	if nameLength != len(entry.Name) || member.dataAt-nameAt != int64(nameLength+extraLength) {
		return errors.New("name length or data offset disagrees with the directory")
	}
	var buf [256]byte
	for done := 0; done < nameLength; {
		n := min(len(buf), nameLength-done)
		if _, err := archive.source.ReadAt(buf[:n], nameAt+int64(done)); err != nil {
			return err
		}
		if string(buf[:n]) != entry.Name[done:done+n] {
			return errors.New("name disagrees with the directory")
		}
		done += n
	}
	// Streaming writers can leave placeholder sizes and CRC in this header.
	// The validated data descriptor covers those fields instead.
	if flags&(1<<3) != 0 {
		return nil
	}
	if binary.LittleEndian.Uint32(header[14:]) != entry.CRC32 {
		return errors.New("checksum disagrees with the directory")
	}
	compressed := uint64(binary.LittleEndian.Uint32(header[18:]))
	expanded := uint64(binary.LittleEndian.Uint32(header[22:]))
	if compressed == entry.CompressedSize64 && expanded == entry.UncompressedSize64 {
		return nil
	}
	if compressed == 0xffffffff || expanded == 0xffffffff {
		extra := make([]byte, extraLength)
		if _, err := archive.source.ReadAt(extra, nameAt+int64(nameLength)); err != nil {
			return err
		}
		wide := zip64Extra(extra)
		if len(wide) < 16 {
			return errors.New("missing ZIP64 member sizes")
		}
		if expanded == 0xffffffff {
			expanded = binary.LittleEndian.Uint64(wide)
		}
		if compressed == 0xffffffff {
			compressed = binary.LittleEndian.Uint64(wide[8:])
		}
	}
	if compressed != entry.CompressedSize64 || expanded != entry.UncompressedSize64 {
		return errors.New("sizes disagree with the directory")
	}
	return nil
}

func zip64Extra(extra []byte) []byte {
	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra)
		length := int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if length > len(extra) {
			return nil
		}
		if tag == 1 {
			return extra[:length]
		}
		extra = extra[length:]
	}
	return nil
}

func (archive *ZIPArchive) validateDescriptor(entry *zip.File, offset, end int64) error {
	var raw [24]byte
	length := min(end-offset, int64(len(raw)))
	if length < 12 {
		return fmt.Errorf("ZIP data descriptor for %q exceeds the member data area", entry.Name)
	}
	if _, err := archive.source.ReadAt(raw[:length], offset); err != nil {
		return fmt.Errorf("read ZIP data descriptor for %q: %w", entry.Name, err)
	}
	descriptor := raw[:length]
	// Both widths are used with small files. The optional signature can also
	// be the CRC32 of an unsigned descriptor, so consider both layouts.
	if matchesZIPDescriptor(descriptor, entry) || binary.LittleEndian.Uint32(descriptor) == 0x08074b50 &&
		matchesZIPDescriptor(descriptor[4:], entry) {
		return nil
	}
	return fmt.Errorf("ZIP data descriptor for %q disagrees with its directory", entry.Name)
}

func matchesZIPDescriptor(data []byte, entry *zip.File) bool {
	if len(data) < 12 || binary.LittleEndian.Uint32(data) != entry.CRC32 {
		return false
	}
	if uint64(binary.LittleEndian.Uint32(data[4:])) == entry.CompressedSize64 &&
		uint64(binary.LittleEndian.Uint32(data[8:])) == entry.UncompressedSize64 {
		return true
	}
	return len(data) >= 20 && binary.LittleEndian.Uint64(data[4:]) == entry.CompressedSize64 &&
		binary.LittleEndian.Uint64(data[12:]) == entry.UncompressedSize64
}

// Keep one buffer and inflater per active reader. A byte reader keeps flate
// from consuming bytes after the final block of the compressed stream.
var zipInflaters sync.Pool

type zipDeflateStream struct {
	compressed *bufio.Reader
	inflater   io.ReadCloser
}

func openZIPDeflate(src io.Reader) io.ReadCloser {
	if reader, ok := zipInflaters.Get().(*zipDeflateStream); ok {
		reader.compressed.Reset(src)
		_ = reader.inflater.(flate.Resetter).Reset(reader.compressed, nil)
		return reader
	}
	compressed := bufio.NewReader(src)
	return &zipDeflateStream{compressed: compressed, inflater: flate.NewReader(compressed)}
}

func (stream *zipDeflateStream) Read(p []byte) (int, error) {
	n, err := stream.inflater.Read(p)
	if err == io.EOF {
		if _, extra := stream.compressed.ReadByte(); extra == nil {
			return n, errors.New("ZIP compressed member contains trailing data")
		} else if extra != io.EOF {
			return n, extra
		}
	}
	return n, err
}

func (stream *zipDeflateStream) Close() error {
	err := stream.inflater.Close()
	stream.compressed.Reset(nil)
	zipInflaters.Put(stream)
	return err
}

// CopyZIPMember streams one ZIP entry to a caller-selected destination.
// Callers validate the member and enforce aggregate expansion limits first.
func CopyZIPMember(ctx context.Context, dst io.Writer, entry *zip.File, buffer []byte, progress func(int64, int64)) error {
	if entry.UncompressedSize64 > 1<<63-1 || entry.CompressedSize64 > 1<<63-1 {
		return errors.New("ZIP entry exceeds supported size")
	}
	if len(buffer) == 0 {
		buffer = make([]byte, 128<<10)
	}
	raw, err := entry.OpenRaw()
	if err != nil {
		return err
	}
	var r io.ReadCloser
	switch entry.Method {
	case zip.Store:
		if entry.CompressedSize64 != entry.UncompressedSize64 {
			return zip.ErrFormat
		}
		r = io.NopCloser(raw)
	case zip.Deflate:
		r = openZIPDeflate(raw)
	default:
		return zip.ErrAlgorithm
	}
	crc := crc32.NewIEEE()
	size := int64(entry.UncompressedSize64)
	err = copyStreamBuffer(ctx, io.MultiWriter(dst, crc), r, size, Options{MaxOutput: size, Progress: progress}, buffer)
	if err == nil && crc.Sum32() != entry.CRC32 {
		err = zip.ErrChecksum
	}
	return errors.Join(err, r.Close())
}

// copyStream enforces limits before writing, and reads to EOF to validate checksums.
func copyStream(ctx context.Context, dst io.Writer, src io.Reader, expected int64, opts Options) error {
	return copyStreamBuffer(ctx, dst, src, expected, opts, make([]byte, 128<<10))
}

func copyStreamBuffer(ctx context.Context, dst io.Writer, src io.Reader, expected int64, opts Options, buf []byte) error {
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if int64(n) > opts.MaxOutput-count {
				return errors.New("expanded data exceeds output limit")
			}
			if expected >= 0 && int64(n) > expected-count {
				return errors.New("expanded data exceeds declared size")
			}
			written, err := dst.Write(buf[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
			count += int64(n)
			if opts.Progress != nil {
				opts.Progress(count, expected)
			}
		}
		if readErr == io.EOF {
			if expected >= 0 && count != expected {
				return io.ErrUnexpectedEOF
			}
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}
