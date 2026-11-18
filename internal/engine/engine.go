package engine

import (
	"context"
	"fmt"

	"github.com/olsonb97/romcompressor.go/internal/chd"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

// Convert runs a format engine against private staging paths. It never publishes output.
func Convert(ctx context.Context, source, output, input string, opts Config) error {
	switch {
	case opts.Format == kind.SevenZip:
		return compress7z(ctx, source, output, opts)
	case kind.ThreeDS(opts.Format) || kind.ThreeDS(input):
		return convert3DS(ctx, source, output, input, opts)
	case opts.Format == kind.WUX || input == kind.WUX:
		return convertWUX(ctx, source, output, input, opts)
	case opts.Format == kind.PBP || input == kind.PBP:
		return convertPBP(ctx, source, output, input, opts)
	case opts.Format == kind.CHD || input == kind.CHD:
		tool, err := process.Resolve(opts.Tools, "chdman")
		if err != nil {
			return err
		}
		if input == kind.CHD {
			if opts.Format != kind.Raw || (opts.Media != "dvd" && opts.Media != "cd" && opts.Media != "hdd") {
				return fmt.Errorf("CHD extraction requires raw output and media cd, dvd, or hdd")
			}
		} else if input != kind.Raw || opts.Format != kind.CHD {
			return fmt.Errorf("CHD creation requires an uncompressed disc image")
		}
		return chd.Convert(ctx, tool, source, output, input == kind.CHD, opts.Media, opts.MaxOutput)
	case kind.Dolphin(opts.Format) || kind.Dolphin(input):
		return convertDolphin(ctx, source, output, input, opts)
	case opts.Format == kind.NSZ || opts.Format == kind.XCZ || input == kind.NSZ || input == kind.XCZ:
		return convertSwitch(ctx, source, output, input, opts)
	case input == kind.SevenZip && opts.Format == kind.Raw:
		return extract7z(ctx, source, output, opts)
	default:
		return fmt.Errorf("conversion from %s to %s is not supported", input, opts.Format)
	}
}

func resolveTool(opts Config, name string) (string, error) { return process.Resolve(opts.Tools, name) }

// HeaderFormat validates a Z3DS header and returns its compressed container format.
func HeaderFormat(b []byte, size int64) (string, error) {
	h, err := parse3DSHeader(b, size)
	return h.format, err
}

// Verify applies an engine's structural and integrity contract.
func Verify(ctx context.Context, path string, format string, opts Config) error {
	switch {
	case format == kind.PBP:
		return verifyPBP(ctx, path, opts)
	case kind.ThreeDS(format):
		return verify3DS(ctx, path, opts)
	case format == kind.CHD:
		tool, err := resolveTool(opts, "chdman")
		if err != nil {
			return err
		}
		return chd.Verify(ctx, tool, path, opts.MaxOutput, opts.Media)
	case kind.Dolphin(format):
		if _, err := DolphinDiscSize(path, format, opts.MaxOutput); err != nil {
			return err
		}
		tool, err := resolveTool(opts, "dolphin-tool")
		if err != nil {
			return err
		}
		return withVerificationStage(ctx, opts, "romcompressor-dolphin-", opts.MaxOutput, func(ctx context.Context, stage fileops.Stage) error {
			_, err := dolphinHashWithEnvironment(ctx, tool, path, stage.Path(), temporaryEnvironment(stage.Path()))
			return err
		})
	case format == kind.NSZ || format == kind.XCZ:
		tool, keys, err := resolveSwitch(opts)
		if err != nil {
			return err
		}
		return withVerificationStage(ctx, opts, "romcompressor-nsz-", opts.MaxOutput, func(ctx context.Context, stage fileops.Stage) error {
			engineSource, err := prepareSwitchInput(ctx, path, "."+string(format), stage.Path(), opts.MaxOutput)
			if err != nil {
				return err
			}
			return runSwitchVerify(ctx, tool, stage, keys, engineSource)
		})
	case format == kind.SevenZip:
		return verify7z(ctx, path, opts)
	default:
		return fmt.Errorf("standalone verification is not supported for %s", format)
	}
}
