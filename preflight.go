package romcompressor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/engine"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// preflightSource is shared by discovery and execution. It checks routes and
// bounded source metadata; codecs and engines still verify payloads during work.
func preflightSource(ctx context.Context, source string, info FileInfo, opts Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if opts.TempDir != "" {
		return fmt.Errorf("temp directory is only used for standalone verification")
	}
	ext := strings.ToLower(filepath.Ext(source))
	if ext == ".cue" && opts.Format != CHD && opts.Format != PBP {
		return fmt.Errorf("CUE sets require CHD or PBP output so their referenced tracks are included")
	}
	if ext == ".gdi" && opts.Format != CHD {
		return fmt.Errorf("GDI sets require CHD output so their referenced tracks are included")
	}
	if info.Format == opts.Format {
		return fmt.Errorf("source is already %s", opts.Format)
	}
	if opts.Entry != "" && info.Format != ZIP && info.Format != SevenZip {
		return fmt.Errorf("entry selection requires ZIP or 7z input")
	}
	if opts.Entry != "" && opts.Format != Raw {
		return fmt.Errorf("entry selection is only available when decompressing")
	}
	if opts.Media != "" && !(opts.Format == CHD || opts.Format == Raw && info.Format == CHD) {
		return fmt.Errorf("media is only used for CHD conversion")
	}
	if opts.Keys != "" && !(opts.Format == NSZ || opts.Format == XCZ || opts.Format == Raw && (info.Format == NSZ || info.Format == XCZ)) {
		return fmt.Errorf("keys are only used for Switch compression")
	}
	if info.Format == WUA && opts.Format == Raw {
		stat, err := fileops.SnapshotStat(source, os.Lstat)
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() > opts.MaxOutput {
			return fmt.Errorf("WUA source must be a regular file within the output limit")
		}
		file, _, err := fileops.OpenRegular(source)
		if err != nil {
			return err
		}
		defer file.Close()
		manifest, _, err := readWUA(ctx, file, stat.Size(), opts.MaxOutput, false)
		if err != nil {
			return err
		}
		if manifest.size > opts.MaxOutput-stat.Size() {
			return fmt.Errorf("WUA source and expanded directory exceed scratch size limit")
		}
		return nil
	}
	if opts.Format == Raw && native(info.Format) {
		_, err := preflightNativeInput(ctx, source, info, opts)
		return err
	}
	if native(opts.Format) {
		switch opts.Format {
		case CSO, ZSO:
			if info.Format != Raw || ext != ".iso" {
				return fmt.Errorf("CSO and ZSO compression requires a raw .iso file")
			}
		case WBFS:
			if info.Format != Raw || ext != ".iso" {
				return fmt.Errorf("WBFS compression requires a raw Wii .iso file")
			}
		case CISO:
			if info.Format != Raw || ext != ".iso" && ext != ".gcm" {
				return fmt.Errorf("Compact ISO compression requires a raw GameCube or Wii .iso or .gcm file")
			}
		case ECM:
			if info.Format != Raw {
				return fmt.Errorf("ECM compression requires an uncompressed disc image file")
			}
		case WUX:
			if info.Format != Raw || ext != ".wud" {
				return fmt.Errorf("WUX compression requires a raw .wud file")
			}
		case CCI:
			if info.Format != Raw || ext != ".iso" && ext != ".xiso" {
				return fmt.Errorf("Xbox CCI compression requires a raw XISO .iso or .xiso file")
			}
		}
		if opts.Format == WUX && opts.Tools["jwudtool"] != "" {
			return engine.Preflight(ctx, source, string(info.Format), engineConfig(opts))
		}
		file, stat, err := fileops.OpenRegular(source)
		if err != nil {
			return err
		}
		defer file.Close()
		if stat.Size() != info.Size {
			return fmt.Errorf("source changed during inspection")
		}
		return codec.PreflightEncode(ctx, string(opts.Format), file, info.Size, opts.MaxOutput)
	}
	if dolphinFormat(opts.Format) && (info.Format == WBFS || info.Format == CISO) {
		_, err := preflightNativeInput(ctx, source, info, opts)
		return err
	}
	return engine.Preflight(ctx, source, string(info.Format), engineConfig(opts))
}

func preflightNativeInput(ctx context.Context, source string, info FileInfo, opts Options) (int64, error) {
	if info.Format == WBFS {
		input, err := openWBFSInput(ctx, source)
		if err != nil {
			return -1, err
		}
		defer input.close()
		return codec.PreflightDecode(ctx, string(info.Format), input, input.size, opts.MaxOutput)
	}
	file, stat, err := fileops.OpenRegular(source)
	if err != nil {
		return -1, err
	}
	defer file.Close()
	if stat.Size() != info.Size {
		return -1, fmt.Errorf("source changed during inspection")
	}
	return codec.PreflightDecode(ctx, string(info.Format), file, info.Size, opts.MaxOutput)
}
