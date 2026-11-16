package romcompressor

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/engine"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

func engineConfig(opts Options) engine.Config {
	var notify func(string, int64, int64)
	if opts.Progress != nil {
		notify = func(phase string, bytes, total int64) {
			opts.Progress(Progress{Phase: phase, Bytes: bytes, Total: total})
		}
	}
	return engine.Config{Format: string(opts.Format), Entry: opts.Entry, Media: opts.Media, Keys: opts.Keys, MaxOutput: opts.MaxOutput, TempDir: opts.TempDir, Tools: opts.Tools, Progress: notify,
		Inspect: func(ctx context.Context, path string) (string, int64, error) {
			info, err := Inspect(ctx, path)
			return string(info.Format), info.Size, err
		},
	}
}

// External engines write to the lifecycle owner's private staging directory.
func externalConvert(ctx context.Context, source, output string, input Format, opts Options, wbfs *wbfsInput) error {
	if (dolphinFormat(opts.Format) || dolphinFormat(input)) && (input == WBFS || input == CISO) {
		return convertSparseISOForDolphin(ctx, source, output, input, opts, wbfs)
	}
	return engine.Convert(ctx, source, output, string(input), engineConfig(opts))
}

func externalVerify(ctx context.Context, path string, format Format, opts Options) error {
	if format == WUA {
		return verifyWUA(ctx, path, opts)
	}
	return engine.Verify(ctx, path, string(format), engineConfig(opts))
}

func externalArchiveEntries(ctx context.Context, source string, opts Options) ([]Entry, error) {
	entries, err := engine.ArchiveEntries(ctx, source, engineConfig(opts))
	if err != nil {
		return nil, err
	}
	var result []Entry
	for _, entry := range entries {
		result = append(result, Entry{Name: entry.Name, Size: entry.Size})
	}
	return result, nil
}

func convertSparseISOForDolphin(ctx context.Context, source, output string, input Format, opts Options, wbfs *wbfsInput) error {
	if _, err := process.Resolve(opts.Tools, "dolphin-tool"); err != nil {
		return err
	}
	if opts.Progress != nil {
		parent := filepath.Dir(output)
		info, err := fileops.SnapshotStat(parent, os.Lstat)
		if err != nil {
			return err
		}
		opts.Progress(Progress{Phase: "restoring disc image"})
		if err := fileops.CheckDirectory(parent, info); err != nil {
			return err
		}
	}
	var src io.ReaderAt
	var size int64
	if wbfs == nil {
		file, info, err := fileops.OpenRegular(source)
		if err != nil {
			return err
		}
		defer file.Close()
		src, size = file, info.Size()
	} else {
		src, size = wbfs, wbfs.size
	}
	iso, err := os.CreateTemp(filepath.Dir(output), "restored-*.iso")
	if err != nil {
		return err
	}
	defer os.Remove(iso.Name())
	err = codec.Decode(ctx, string(input), src, size, iso, codec.Options{MaxOutput: opts.MaxOutput})
	closeErr := iso.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return engine.Convert(ctx, iso.Name(), output, string(Raw), engineConfig(opts))
}
