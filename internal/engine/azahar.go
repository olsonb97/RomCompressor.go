package engine

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

type threeDSHeader struct {
	format     string
	offset     uint64
	compressed uint64
	expanded   uint64
}

// Z3DS v1 wraps a seekable Zstandard stream after its header and metadata.
// Check declared sizes before Azahar can allocate metadata or expand the stream.
func parse3DSHeader(b []byte, size int64) (threeDSHeader, error) {
	var h threeDSHeader
	if len(b) < 32 || string(b[:4]) != "Z3DS" || b[8] != 1 || b[9] != 0 {
		return h, fmt.Errorf("unsupported or truncated Z3DS header")
	}
	switch string(b[4:8]) {
	case "NCSD":
		h.format = kind.ZCCI
	case "NCCH":
		h.format = kind.ZCXI
	case "3DSX":
		h.format = kind.Z3DSX
	case "CIA\x00":
		h.format = kind.ZCIA
	default:
		return h, fmt.Errorf("unknown Z3DS content type")
	}
	headerSize := uint64(binary.LittleEndian.Uint16(b[10:12]))
	metadataSize := uint64(binary.LittleEndian.Uint32(b[12:16]))
	h.offset = headerSize + metadataSize
	h.compressed = binary.LittleEndian.Uint64(b[16:24])
	h.expanded = binary.LittleEndian.Uint64(b[24:32])
	if headerSize < 32 || metadataSize > 4<<20 || size < 32 || h.offset > uint64(size) || h.compressed != uint64(size)-h.offset || h.expanded == 0 || h.expanded > uint64(kind.DefaultMaxOutput) {
		return h, fmt.Errorf("invalid Z3DS sizes or metadata exceeds 4 MiB")
	}
	return h, nil
}

func read3DSHeader(ctx context.Context, path string, limit int64) (threeDSHeader, error) {
	if err := ctx.Err(); err != nil {
		return threeDSHeader{}, err
	}
	f, st, err := fileops.OpenRegular(path)
	if err != nil {
		return threeDSHeader{}, err
	}
	defer f.Close()
	var header [32]byte
	if _, err = io.ReadFull(f, header[:]); err != nil {
		return threeDSHeader{}, err
	}
	h, err := parse3DSHeader(header[:], st.Size())
	if err != nil {
		return h, err
	}
	if limit <= 0 {
		limit = kind.DefaultMaxOutput
	}
	if h.expanded > uint64(limit) {
		return h, fmt.Errorf("Z3DS expanded size exceeds the output limit")
	}
	metadataStart := int64(binary.LittleEndian.Uint16(header[10:12]))
	metadata := io.NewSectionReader(f, metadataStart, int64(h.offset)-metadataStart)
	if err := validate3DSMetadata(ctx, metadata); err != nil {
		return h, err
	}
	// Seek-table lengths must agree with the outer header; otherwise an engine
	// could silently under-read a stream whose declared size was shortened.
	var footer [9]byte
	if h.compressed < 17 {
		return h, fmt.Errorf("truncated Z3DS seek table")
	}
	if _, err = f.ReadAt(footer[:], st.Size()-9); err != nil {
		return h, err
	}
	frames := uint64(binary.LittleEndian.Uint32(footer[:4]))
	// Bits 1 and 0 are unused by the current seekable format and are ignored.
	if binary.LittleEndian.Uint32(footer[5:]) != 0x8f92eab1 || footer[4]&0x7c != 0 || frames == 0 || frames > 1_000_000 {
		return h, fmt.Errorf("invalid or oversized Z3DS seek table")
	}
	entrySize := uint64(8)
	if footer[4]&0x80 != 0 {
		entrySize = 12
	}
	tableSize := frames*entrySize + 17
	if tableSize > h.compressed {
		return h, fmt.Errorf("truncated Z3DS seek table")
	}
	reader := bufio.NewReader(io.NewSectionReader(f, st.Size()-int64(tableSize), int64(tableSize)))
	var frame [12]byte
	if _, err = io.ReadFull(reader, frame[:8]); err != nil {
		return h, err
	}
	if binary.LittleEndian.Uint32(frame[:4]) != 0x184d2a5e || uint64(binary.LittleEndian.Uint32(frame[4:8])) != tableSize-8 {
		return h, fmt.Errorf("invalid Z3DS seek table frame")
	}
	var compressed, expanded uint64
	for i := uint64(0); i < frames; i++ {
		if err = ctx.Err(); err != nil {
			return h, err
		}
		if _, err = io.ReadFull(reader, frame[:entrySize]); err != nil {
			return h, err
		}
		c, d := uint64(binary.LittleEndian.Uint32(frame[:4])), uint64(binary.LittleEndian.Uint32(frame[4:8]))
		if c == 0 || d > 1<<30 || c > h.compressed-tableSize-compressed || d > h.expanded-expanded {
			return h, fmt.Errorf("invalid Z3DS frame sizes")
		}
		data := io.NewSectionReader(f, int64(h.offset+compressed), int64(c))
		if err := codec.ValidateZstandardFrame(ctx, data, d); err != nil {
			return h, fmt.Errorf("Z3DS frame %d: %w", i+1, err)
		}
		compressed += c
		expanded += d
	}
	if compressed != h.compressed-tableSize || expanded != h.expanded {
		return h, fmt.Errorf("Z3DS frame sizes disagree with its header")
	}
	return h, ctx.Err()
}

func validate3DSMetadata(ctx context.Context, metadata *io.SectionReader) error {
	if metadata.Size() == 0 {
		return nil
	}
	var version [1]byte
	if _, err := io.ReadFull(metadata, version[:]); err != nil {
		return err
	}
	if version[0] != 1 {
		return nil // Azahar ignores metadata versions it does not recognize.
	}
	r := bufio.NewReader(metadata)
	remaining := metadata.Size() - 1
	for remaining >= 4 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item [4]byte
		if _, err := io.ReadFull(r, item[:]); err != nil {
			return err
		}
		remaining -= 4
		length := int(item[1]) + int(binary.LittleEndian.Uint16(item[2:]))
		if item[0] == 0 && length == 0 && remaining == 0 {
			return nil
		}
		if item[0] == 0 || int64(length) > remaining {
			return fmt.Errorf("invalid Z3DS metadata item length or terminator")
		}
		if _, err := r.Discard(length); err != nil {
			return err
		}
		remaining -= int64(length)
	}
	return fmt.Errorf("Z3DS metadata is missing its end marker")
}

func convert3DS(ctx context.Context, source, output string, input string, opts Config) error {
	compress := input == kind.Raw && kind.ThreeDS(opts.Format)
	if !compress && !(kind.ThreeDS(input) && opts.Format == kind.Raw) {
		return fmt.Errorf("3DS conversion requires an uncompressed game or raw output")
	}
	if compress {
		ext := strings.ToLower(filepath.Ext(source))
		if ext != kind.ThreeDSRawExtension(opts.Format) && !(opts.Format == kind.ZCCI && ext == ".3ds") && !(opts.Format == kind.ZCXI && ext == ".app") {
			return fmt.Errorf("%s compression requires %s input", opts.Format, kind.ThreeDSRawExtension(opts.Format))
		}
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if opts.MaxOutput > 0 && info.Size() > opts.MaxOutput {
			return fmt.Errorf("3DS source exceeds the expanded size limit")
		}
	}
	tool, err := resolveTool(opts, "azahar")
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), "azahar-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	env := threeDSEnvironment(stage)
	var generated string
	if compress {
		if _, err = process.RunEnv(ctx, env, tool, "-c", source, "-o", stage); err != nil {
			return err
		}
		generated = filepath.Join(stage, strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))+"."+string(opts.Format))
		h, err := read3DSHeader(ctx, generated, opts.MaxOutput)
		if err != nil {
			return err
		}
		if h.format != opts.Format {
			return fmt.Errorf("Azahar output type does not match the requested format")
		}
		roundtrip := filepath.Join(stage, "roundtrip")
		if err = os.Mkdir(roundtrip, 0700); err != nil {
			return err
		}
		restored, err := extract3DS(ctx, tool, generated, roundtrip, env, h)
		if err != nil {
			return err
		}
		if err = fileops.CompareFiles(ctx, source, restored); err != nil {
			return err
		}
	} else {
		h, err := read3DSHeader(ctx, source, opts.MaxOutput)
		if err != nil {
			return err
		}
		generated, err = extract3DS(ctx, tool, source, stage, env, h)
		if err != nil {
			return err
		}
	}
	return fileops.Publish(generated, output)
}

func threeDSEnvironment(stage string) []string {
	// Azahar uses platform known-folder APIs on Windows; these variables isolate
	// its Unix logs and configuration. Windows may still update its normal log.
	return []string{"XDG_DATA_HOME=" + stage, "XDG_CONFIG_HOME=" + stage, "XDG_CACHE_HOME=" + stage, "HOME=" + stage}
}

func extract3DS(ctx context.Context, tool, source, directory string, env []string, h threeDSHeader) (string, error) {
	if _, err := process.RunEnv(ctx, env, tool, "-x", source, "-o", directory); err != nil {
		return "", err
	}
	output := filepath.Join(directory, strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))+kind.ThreeDSRawExtension(h.format))
	info, err := fileops.SnapshotStat(output, os.Lstat)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || uint64(info.Size()) != h.expanded {
		return "", fmt.Errorf("Azahar output length does not match its Z3DS header")
	}
	if h.format == kind.ZCIA {
		// CIA has no signature. Azahar's loader checks its CIA metadata.
		return output, nil
	}
	file, actual, err := fileops.OpenRegular(output)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if !fileops.SameSnapshot(info, actual) {
		return "", fmt.Errorf("Azahar output changed before its signature check")
	}
	if err := checkThreeDSOutputSignature(file, h.format); err != nil {
		return "", err
	}
	return output, nil
}

func checkThreeDSOutputSignature(file io.ReaderAt, format string) error {
	var offset int64
	var want string
	switch format {
	case kind.ZCCI:
		offset, want = 0x100, "NCSD"
	case kind.ZCXI:
		offset, want = 0x100, "NCCH"
	case kind.Z3DSX:
		want = "3DSX"
	default:
		return fmt.Errorf("unsupported Z3DS signature check for %s", format)
	}
	var magic [4]byte
	if _, err := file.ReadAt(magic[:], offset); err != nil {
		return fmt.Errorf("read 3DS image %s signature: %w", want, err)
	}
	if string(magic[:]) != want {
		return fmt.Errorf("3DS image lacks the required %s signature", want)
	}
	return nil
}

func verify3DS(ctx context.Context, source string, opts Config) error {
	h, err := read3DSHeader(ctx, source, opts.MaxOutput)
	if err != nil {
		return err
	}
	tool, err := resolveTool(opts, "azahar")
	if err != nil {
		return err
	}
	limit := opts.MaxOutput
	if limit <= 0 {
		limit = kind.DefaultMaxOutput
	}
	return withVerificationStage(ctx, opts, "romcompressor-azahar-", limit, func(ctx context.Context, stage fileops.Stage) error {
		env := append(threeDSEnvironment(stage.Path()), temporaryEnvironment(stage.Path())...)
		_, err := extract3DS(ctx, tool, source, stage.Path(), env, h)
		return err
	})
}
