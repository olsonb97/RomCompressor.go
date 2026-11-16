package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/chd"
	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

// Preflight checks conversion routes and bounded source metadata. It never
// resolves or runs an engine, reads keys, or creates files. Payload compatibility
// remains the responsibility of conversion and its verification contract.
func Preflight(ctx context.Context, source, input string, opts Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(source))
	switch {
	case opts.Format == kind.SevenZip:
		if strings.HasPrefix(filepath.Base(source), "@") {
			return fmt.Errorf("7z input filename cannot begin with @")
		}
		return nil
	case kind.ThreeDS(opts.Format) || kind.ThreeDS(input):
		if input == kind.Raw && kind.ThreeDS(opts.Format) {
			if ext != kind.ThreeDSRawExtension(opts.Format) && !(opts.Format == kind.ZCCI && ext == ".3ds") && !(opts.Format == kind.ZCXI && ext == ".app") {
				return fmt.Errorf("%s compression requires %s input", opts.Format, kind.ThreeDSRawExtension(opts.Format))
			}
			file, info, err := fileops.OpenRegular(source)
			if err != nil {
				return err
			}
			defer file.Close()
			if info.Size() > opts.MaxOutput {
				return fmt.Errorf("3DS source exceeds the expanded size limit")
			}
			if opts.Format != kind.ZCIA {
				if err := checkThreeDSOutputSignature(file, opts.Format); err != nil {
					return fmt.Errorf("3DS source signature: %w", err)
				}
			}
			return nil
		}
		if kind.ThreeDS(input) && opts.Format == kind.Raw {
			file, info, err := fileops.OpenRegular(source)
			if err != nil {
				return err
			}
			defer file.Close()
			var header [32]byte
			if _, err = file.ReadAt(header[:], 0); err != nil {
				return err
			}
			h, err := parse3DSHeader(header[:], info.Size())
			if err != nil {
				return err
			}
			if h.expanded > uint64(opts.MaxOutput) {
				return fmt.Errorf("Z3DS expanded size exceeds the output limit")
			}
			return nil
		}
	case opts.Format == kind.WUX || input == kind.WUX:
		if input == kind.Raw && opts.Format == kind.WUX && ext == ".wud" || input == kind.WUX && opts.Format == kind.Raw {
			return ValidateWUXSource(ctx, source, input, opts)
		}
	case opts.Format == kind.PBP || input == kind.PBP:
		if input == kind.PBP && opts.Format == kind.Raw {
			file, _, err := fileops.OpenRegular(source)
			if err != nil {
				return err
			}
			defer file.Close()
			disc, err := openPBPDisc(ctx, file)
			if err != nil {
				return err
			}
			cue, err := pbpCueText(".bin", disc.Tracks)
			if err != nil {
				return err
			}
			if int64(disc.Sectors)*codec.PBPBytesPerSector > opts.MaxOutput-int64(len(cue)) {
				return fmt.Errorf("expanded PBP disc and CUE exceed output size limit")
			}
			return nil
		}
		if input == kind.Raw && opts.Format == kind.PBP && ext == ".cue" {
			_, _, sectors, err := readPBPCue(ctx, source)
			if err != nil {
				return err
			}
			if int64(sectors)*codec.PBPBytesPerSector > opts.MaxOutput {
				return fmt.Errorf("PBP source disc exceeds output size limit")
			}
			return nil
		}
	case opts.Format == kind.CHD || input == kind.CHD:
		if input == kind.CHD && opts.Format == kind.Raw {
			if opts.Media != "cd" && opts.Media != "dvd" && opts.Media != "hdd" {
				return fmt.Errorf("CHD extraction requires media cd, dvd, or hdd")
			}
			_, err := chd.MetadataFile(source, opts.MaxOutput, opts.Media)
			return err
		}
		if input == kind.Raw && opts.Format == kind.CHD {
			return preflightCHD(ctx, source, ext, opts)
		}
	case kind.Dolphin(opts.Format) || kind.Dolphin(input):
		if (kind.Dolphin(opts.Format) || opts.Format == kind.Raw) && (input == kind.Raw || kind.Dolphin(input)) {
			if input == kind.Raw {
				return checkDolphinISO(source)
			}
			return nil
		}
	case opts.Format == kind.NSZ || opts.Format == kind.XCZ || input == kind.NSZ || input == kind.XCZ:
		if (input == kind.NSZ || input == kind.XCZ) && opts.Format == kind.Raw {
			return nil
		}
		if input == kind.Raw && (opts.Format == kind.NSZ && ext == ".nsp" || opts.Format == kind.XCZ && ext == ".xci") {
			file, _, err := fileops.OpenRegular(source)
			if err != nil {
				return err
			}
			defer file.Close()
			var magic [4]byte
			offset, want := int64(0), "PFS0"
			if opts.Format == kind.XCZ {
				offset, want = 0x100, "HEAD"
			}
			if _, err := file.ReadAt(magic[:], offset); err != nil {
				return err
			}
			if string(magic[:]) != want {
				return fmt.Errorf("Switch input lacks its %s signature", want)
			}
			return nil
		}
	case input == kind.SevenZip && opts.Format == kind.Raw:
		return nil // Listing requires the engine and is deferred to conversion.
	}
	return fmt.Errorf("conversion from %s to %s is not supported", input, opts.Format)
}

func preflightCHD(ctx context.Context, source, ext string, opts Config) error {
	media := opts.Media
	if media == "" && (ext == ".cue" || ext == ".gdi") {
		media = "cd"
	}
	switch media {
	case "cd":
		switch ext {
		case ".cue":
			return chd.ValidateCUE(source, "", opts.MaxOutput)
		case ".gdi":
			return chd.ValidateGDI(source, opts.MaxOutput)
		case ".nrg":
			return chd.ValidateNRG(ctx, source, opts.MaxOutput)
		case ".iso", ".cdr", ".toast":
			return nil
		}
		return fmt.Errorf("CD CHD input must be a CUE/GDI descriptor or an ISO, CDR, Toast, or NRG image")
	case "dvd", "hdd":
		if media == "dvd" && ext != ".iso" || media == "hdd" && !chd.IsHardDiskExtension(ext) {
			break
		}
		file, info, err := fileops.OpenRegular(source)
		if err != nil {
			return err
		}
		defer file.Close()
		sector := int64(2048)
		if media == "hdd" {
			sector = 512
		}
		if info.Size() == 0 || info.Size()%sector != 0 || info.Size() > opts.MaxOutput {
			return fmt.Errorf("%s CHD requires complete %d-byte sectors within the output limit", media, sector)
		}
		return nil
	}
	return fmt.Errorf("input extension %s is unavailable for CHD media %q", ext, media)
}
