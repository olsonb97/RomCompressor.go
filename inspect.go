package romcompressor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/chd"
	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/engine"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	cardtrim "github.com/olsonb97/romcompressor.go/internal/trim"
)

// Inspect identifies containers using their header. Raw images retain their raw classification.
func Inspect(ctx context.Context, path string) (FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	f, st, err := fileops.OpenRegular(path)
	if err != nil {
		return FileInfo{}, err
	}
	defer f.Close()
	var header [512]byte
	n, err := f.ReadAt(header[:], 0)
	if err != nil && err != io.EOF {
		return FileInfo{}, err
	}
	b := header[:n]
	ext := strings.ToLower(filepath.Ext(path))
	// Nintendo cartridges start with bytes that can look like a short
	// compression signature. Check their named header before those prefixes.
	if ext == ".nds" && cardtrim.HasNDSHeader(b) {
		return FileInfo{Format: Raw, Size: st.Size()}, ctx.Err()
	}
	if len(b) >= 0x104 {
		magic := string(b[0x100:0x104])
		nintendoCCI := ext == ".3ds" || ext == ".cci" && !bytes.HasPrefix(b, []byte("CCIM"))
		if (nintendoCCI && magic == "NCSD") || (ext == ".xci" && magic == "HEAD") {
			return FileInfo{Format: Raw, Size: st.Size()}, ctx.Err()
		}
	}
	format := Raw
	for _, signature := range []struct {
		magic  []byte
		format Format
	}{
		{[]byte("CISO"), CSO}, {[]byte("ZISO"), ZSO}, {[]byte("MComprHD"), CHD},
		{[]byte("ECM\x00"), ECM}, {[]byte("WBFS"), WBFS}, {[]byte("CCIM"), CCI},
		{[]byte{'R', 'V', 'Z', 1}, RVZ}, {[]byte{'W', 'I', 'A', 1}, WIA},
		{[]byte{1, 0xc0, 0x0b, 0xb1}, GCZ}, {[]byte{0x1f, 0x8b}, GZIP},
		{[]byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, SevenZip},
		{[]byte{'W', 'U', 'X', '0', 0x2e, 0xd0, 0x99, 0x10}, WUX},
	} {
		if bytes.HasPrefix(b, signature.magic) {
			format = signature.format
			break
		}
	}
	if bytes.HasPrefix(b, []byte("PK\x03\x04")) || bytes.HasPrefix(b, []byte("PK\x05\x06")) {
		format = ZIP
	}
	// Nintendo Compact ISO shares the PSP CISO magic, but its second word is
	// a large block size. In PSP CSO this word describes the small header.
	if format == CSO && len(b) >= 8 && binary.LittleEndian.Uint32(b[4:8]) >= 32<<10 {
		format = CISO
	}
	// 7-Zip opens numbered companions after it recognizes a .001 volume.
	// Refuse it even when the preceding archive extension was omitted.
	if ext == ".001" && (format == ZIP || format == SevenZip) {
		return FileInfo{}, fmt.Errorf("split %s archives are unsupported", format)
	}
	if len(ext) == 5 && ext[:4] == ".wbf" && ext[4] >= '1' && ext[4] <= '9' {
		return FileInfo{}, fmt.Errorf("choose the primary .wbfs file when opening a split WBFS")
	}
	if ext == ".wua" {
		return inspectWUA(ctx, f, st.Size())
	}
	if bytes.HasPrefix(b, []byte{'\x00', 'P', 'B', 'P'}) {
		if _, err := codec.ReadPBP(ctx, f, st.Size()); err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return FileInfo{}, canceled
			}
			if ext == ".pbp" {
				if errors.Is(err, codec.ErrPBPWithoutDisc) {
					return FileInfo{Format: Raw, Size: st.Size()}, nil
				}
				return FileInfo{}, err
			}
		} else {
			format = PBP
		}
	}
	// Switch compression retains the outer PFS0/HFS0 container. Its extension
	// distinguishes the requested engine; that engine validates the inner NCZ records.
	if ext == ".nsz" && bytes.HasPrefix(b, []byte("PFS0")) {
		format = NSZ
	}
	if ext == ".xcz" && len(b) >= 0x104 && bytes.Equal(b[0x100:0x104], []byte("HEAD")) {
		format = XCZ
	}
	if bytes.HasPrefix(b, []byte("Z3DS")) {
		detected, err := engine.HeaderFormat(b, st.Size())
		if err != nil {
			return FileInfo{}, err
		}
		format = Format(detected)
	}
	if expected, ok := extensionFormat(ext); ok && format != expected &&
		!(expected == CISO && format == CSO && pspCISOAlias(b)) && !(expected == CCI && format == Raw) {
		return FileInfo{}, fmt.Errorf("%s header does not match its extension", ext)
	}
	if format == WBFS {
		input, err := openWBFSInput(ctx, path)
		if err != nil {
			return FileInfo{}, err
		}
		defer input.close()
		if err := codec.ValidateWBFS(ctx, input, input.size); err != nil {
			return FileInfo{}, err
		}
		return FileInfo{Format: WBFS, Size: input.size}, nil
	}
	if format == CISO {
		if err := codec.ValidateCISO(ctx, f, st.Size()); err != nil {
			return FileInfo{}, err
		}
	}
	if format == CCI {
		if splitCCIName(path) {
			return FileInfo{}, fmt.Errorf("split Xbox CCI sets are unsupported")
		}
		if err := codec.ValidateCCI(ctx, f, st.Size()); err != nil {
			return FileInfo{}, err
		}
	}
	info := FileInfo{Format: format, Size: st.Size()}
	if format == CHD {
		info.Media, err = chd.Media(ctx, f, st.Size())
		if err != nil {
			return FileInfo{}, err
		}
	}
	if format == ZIP {
		z, err := codec.OpenZIP(ctx, f, st.Size())
		if err != nil {
			return FileInfo{}, err
		}
		for _, entry := range z.File {
			if err := ctx.Err(); err != nil {
				return FileInfo{}, err
			}
			if !entry.FileInfo().IsDir() {
				info.Entries = append(info.Entries, Entry{Name: entry.Name, Size: entry.UncompressedSize64})
			}
		}
	}
	return info, nil
}

// Xbox CCI reserves a numeric sub-extension for split parts such as .1.cci.
func splitCCIName(path string) bool {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	number := strings.TrimPrefix(filepath.Ext(stem), ".")
	return number != "" && strings.IndexFunc(number, func(r rune) bool {
		return r < '0' || r > '9'
	}) == -1
}

func pspCISOAlias(header []byte) bool {
	if len(header) < 24 {
		return false
	}
	headerSize := binary.LittleEndian.Uint32(header[4:8])
	blockSize := binary.LittleEndian.Uint32(header[16:20])
	return (headerSize == 0 || headerSize == 24) && blockSize != 0 && blockSize <= 1<<20 && blockSize%2048 == 0
}

func extensionFormat(ext string) (Format, bool) {
	for _, item := range formatDescriptors {
		if ext == "."+string(item.Format) {
			return item.Format, true
		}
	}
	return GZIP, ext == ".gz"
}

// InspectWithOptions also lists 7z archive entries using the configured engine.
func InspectWithOptions(ctx context.Context, path string, opts Options) (FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	if err := validateTools(opts.Tools); err != nil {
		return FileInfo{}, err
	}
	info, err := Inspect(ctx, path)
	if err == nil && info.Format == SevenZip {
		info.Entries, err = externalArchiveEntries(ctx, path, opts)
	}
	return info, dependencyError(err)
}
