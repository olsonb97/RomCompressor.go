package romcompressor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/chd"
	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/progress"
)

const defaultMaxOutput int64 = kind.DefaultMaxOutput

// Convert stages, verifies, and exclusively publishes output. Existing files are
// never replaced. CD extraction publishes BIN companions before its CUE entry point.
func Convert(ctx context.Context, source, output string, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if playlistSource := filepath.Clean(source); isPlaylist(playlistSource) {
		info, err := os.Lstat(playlistSource)
		if err != nil || !info.IsDir() {
			return ConvertPlaylist(ctx, source, output, opts)
		}
	}
	result, err := convertFile(ctx, source, output, opts, nil)
	return result, dependencyError(err)
}

func convertFile(ctx context.Context, source, output string, opts Options, playlistInputs map[string]os.FileInfo) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if source == "" || output == "" {
		return Result{}, fmt.Errorf("source and output are required")
	}
	if opts.TempDir != "" {
		return Result{}, fmt.Errorf("temp directory is only used for standalone verification")
	}
	if !known(opts.Format) {
		return Result{}, fmt.Errorf("unknown output format %q", opts.Format)
	}
	if err := validateTools(opts.Tools); err != nil {
		return Result{}, err
	}
	if opts.MaxOutput < 0 {
		return Result{}, fmt.Errorf("max output must be positive")
	}
	if opts.MaxOutput == 0 {
		opts.MaxOutput = defaultMaxOutput
	}
	if opts.Media != "" && opts.Media != "cd" && opts.Media != "dvd" && opts.Media != "hdd" {
		return Result{}, fmt.Errorf("media must be cd, dvd, or hdd")
	}
	if opts.ExtractAll && (opts.Format != Raw || opts.Entry != "" || opts.Media != "" || opts.Keys != "") {
		return Result{}, fmt.Errorf("extract all requires raw output and excludes entry, media, and keys options")
	}
	var err error
	source, err = filepath.Abs(source)
	if err != nil {
		return Result{}, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return Result{}, err
	}
	if !filepath.IsLocal(filepath.Base(output)) {
		return Result{}, fmt.Errorf("output must name a new file or directory")
	}
	if _, err := os.Lstat(output); err == nil {
		return Result{}, &os.PathError{Op: "create output", Path: output, Err: os.ErrExist}
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	if opts.Format == ZIP {
		sourceInfo, err := os.Lstat(source)
		if err != nil {
			return Result{}, err
		}
		if sourceInfo.IsDir() {
			if opts.Entry != "" || opts.Media != "" || opts.Keys != "" {
				return Result{}, fmt.Errorf("directory ZIP creation does not take entry, media, or keys options")
			}
			if err := validateOutputExtension(Raw, output, opts); err != nil {
				return Result{}, err
			}
			return compressZIPTree(ctx, source, output, opts)
		}
	}
	if opts.Format == WUA {
		if opts.Entry != "" || opts.Media != "" || opts.Keys != "" {
			return Result{}, fmt.Errorf("WUA creation does not take entry, media, or keys options")
		}
		if err := validateOutputExtension(Raw, output, opts); err != nil {
			return Result{}, err
		}
		return convertWUA(ctx, source, output, opts)
	}
	info, err := Inspect(ctx, source)
	if err != nil {
		return Result{}, fmt.Errorf("inspect source: %w", err)
	}
	if opts.ExtractAll {
		if info.Format != ZIP {
			return Result{}, fmt.Errorf("extract all is available for ZIP input")
		}
		return extractZIPTree(ctx, source, output, opts)
	}
	if err := validateOutputExtension(info.Format, output, opts); err != nil {
		return Result{}, err
	}
	if info.Format == WUA && opts.Format == Raw {
		if err := preflightSource(ctx, source, info, opts); err != nil {
			return Result{}, err
		}
		return convertWUA(ctx, source, output, opts)
	}
	var inputs map[string]os.FileInfo
	var wbfs *wbfsInput
	if info.Format == WBFS {
		wbfs, err = openWBFSInput(ctx, source)
		if err != nil {
			return Result{}, err
		}
		defer wbfs.close()
		if wbfs.size != info.Size {
			return Result{}, fmt.Errorf("WBFS source changed during inspection")
		}
		if len(wbfs.parts) > 1 && opts.Format != Raw && !dolphinFormat(opts.Format) {
			return Result{}, fmt.Errorf("split WBFS conversion requires ISO, RVZ, WIA, or GCZ output")
		}
		inputs = make(map[string]os.FileInfo, len(wbfs.parts))
		wbfs.addToSnapshot(inputs)
	} else {
		inputs, err = snapshotInputs(source)
		if err != nil {
			return Result{}, err
		}
		if inputs[source].Size() != info.Size {
			return Result{}, fmt.Errorf("source changed during inspection")
		}
	}
	if err := preflightSource(ctx, source, info, opts); err != nil {
		return Result{}, err
	}
	var sidecar *discSidecar
	if opts.Media != "hdd" {
		sidecar, err = findDiscSidecar(source, output)
		if err != nil {
			return Result{}, err
		}
	}
	if sidecar != nil {
		inputs[sidecar.path] = sidecar.info
	}
	if err := rememberPlaylistInputs(playlistInputs, inputs); err != nil {
		return Result{}, err
	}
	var inputBytes int64
	for _, input := range inputs {
		if input.Size() > defaultMaxOutput-inputBytes {
			return Result{}, fmt.Errorf("source set exceeds 1 TiB")
		}
		inputBytes += input.Size()
	}
	stage, err := fileops.NewStage(filepath.Dir(output), ".romcompressor-*")
	if err != nil {
		return Result{}, fmt.Errorf("create staging directory: %w", err)
	}
	defer stage.Cleanup()
	staged := filepath.Join(stage.Path(), filepath.Base(output))
	notify := progressReporter(opts.Progress)
	notify("converting", 0, info.Size)
	if err := stage.Check(); err != nil {
		return Result{}, err
	}
	if err := checkInputs(inputs); err != nil {
		return Result{}, err
	}
	wuxOverride := (opts.Format == WUX || opts.Format == Raw && info.Format == WUX) &&
		opts.Tools["jwudtool"] != ""
	if !wuxOverride && (native(opts.Format) || opts.Format == Raw && native(info.Format)) {
		err = convertNative(ctx, source, staged, info, opts, wbfs, notify)
	} else {
		err = fileops.RunMonitored(ctx, stage.Path(), opts.MaxOutput, func(ctx context.Context) error {
			return externalConvert(ctx, source, staged, info.Format, opts, wbfs)
		})
	}
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := stage.Check(); err != nil {
		return Result{}, err
	}
	var companions []string
	if opts.Format == Raw && (info.Format == PBP || info.Format == CHD && opts.Media == "cd") {
		companions = append(companions, strings.TrimSuffix(staged, filepath.Ext(staged))+".bin")
	}
	if sidecar != nil {
		path, err := sidecar.stage(ctx, staged, opts.MaxOutput)
		if err != nil {
			return Result{}, err
		}
		if path != "" {
			companions = append(companions, path)
		}
	}
	if err := checkInputs(inputs); err != nil {
		return Result{}, err
	}
	size, err := publish(ctx, stage.Path(), staged, output, opts.MaxOutput, companions...)
	if err != nil {
		return Result{}, err
	}
	notify("complete", size, size)
	return Result{Output: output, Format: opts.Format, InputBytes: inputBytes, OutputBytes: size, Verified: true}, nil
}

func validateOutputExtension(input Format, output string, opts Options) error {
	ext := strings.ToLower(filepath.Ext(output))
	if opts.Format != Raw {
		format, ok := extensionFormat(ext)
		if !ok || format != opts.Format {
			return fmt.Errorf("output extension must match %s", opts.Format)
		}
		if opts.Format == CCI && splitCCIName(output) {
			return fmt.Errorf("Xbox CCI output must use an unnumbered .cci filename")
		}
	} else {
		switch input {
		case CSO, ZSO, RVZ, WIA, GCZ, WBFS, CISO:
			if ext != ".iso" {
				return fmt.Errorf("decompressed output must use .iso")
			}
		case NSZ:
			if ext != ".nsp" {
				return fmt.Errorf("decompressed output must use .nsp")
			}
		case XCZ:
			if ext != ".xci" {
				return fmt.Errorf("decompressed output must use .xci")
			}
		case WUX:
			if ext != ".wud" {
				return fmt.Errorf("decompressed output must use .wud")
			}
		case CCI:
			if ext != ".iso" && ext != ".xiso" {
				return fmt.Errorf("Xbox CCI extraction needs an .iso or .xiso output")
			}
		case PBP:
			if ext != ".cue" {
				return fmt.Errorf("PlayStation PBP extraction needs a .cue and publishes a BIN companion")
			}
		case ZCCI, ZCIA, ZCXI, Z3DSX:
			expected := map[Format]string{ZCCI: ".cci", ZCIA: ".cia", ZCXI: ".cxi", Z3DSX: ".3dsx"}[input]
			if ext != expected && !(input == ZCCI && ext == ".3ds") {
				return fmt.Errorf("decompressed output must use %s", expected)
			}
		case CHD:
			if opts.Media != "cd" && opts.Media != "dvd" && opts.Media != "hdd" {
				return fmt.Errorf("CHD extraction requires media cd, dvd, or hdd")
			}
			if opts.Media == "cd" && ext != ".cue" || opts.Media == "dvd" && ext != ".iso" {
				return fmt.Errorf("CHD CD extraction needs .cue; DVD extraction needs .iso")
			}
			if opts.Media == "hdd" && !chd.IsHardDiskExtension(ext) {
				return fmt.Errorf("hard-drive CHD extraction needs .img, .raw, .hdd, or .bin")
			}
		}
	}
	return nil
}

func convertNative(ctx context.Context, source, output string, info FileInfo, opts Options, wbfs *wbfsInput, notify func(string, int64, int64)) error {
	var src io.ReaderAt = wbfs
	if wbfs == nil {
		file, _, err := fileops.OpenRegular(source)
		if err != nil {
			return err
		}
		defer file.Close()
		src = file
	}
	dst, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer dst.Close()
	options := codec.Options{Entry: opts.Entry, Name: filepath.Base(source), MaxOutput: opts.MaxOutput, Progress: func(n, total int64) { notify("converting", n, total) }}
	expected := sha256.New()
	if opts.Format == Raw {
		var output io.Writer = dst
		if opts.Progress != nil {
			output = io.MultiWriter(dst, expected)
		}
		err = codec.Decode(ctx, string(info.Format), src, info.Size, output, options)
	} else {
		output := fileops.NewBoundedStageFile(dst, opts.MaxOutput)
		err = codec.Encode(ctx, string(opts.Format), &digestingReaderAt{reader: src, digest: expected}, info.Size, output, options)
	}
	if err != nil {
		return err
	}
	if opts.Format == Raw && opts.Progress == nil {
		return nil
	}
	st, err := dst.Stat()
	if err != nil {
		return err
	}
	notify("verifying", 0, info.Size)
	notify("publishing", 0, 0)
	options.Progress = nil
	if opts.Format == Raw {
		return fileops.CheckDigest(ctx, dst, st.Size(), expected.Sum(nil))
	}
	actual := sha256.New()
	if err := codec.Decode(ctx, string(opts.Format), dst, st.Size(), actual, options); err != nil {
		return fmt.Errorf("verify compressed output: %w", err)
	}
	if !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
		return fmt.Errorf("staged output does not reproduce the expected bytes")
	}
	current, err := os.Lstat(output)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(st, current) || st.Size() != current.Size() {
		return fmt.Errorf("staged output was replaced or resized during verification")
	}
	return ctx.Err()
}

// Native encoders read their source sequentially through ReaderAt. Hash those
// reads so output verification needs no additional full source-file scan.
type digestingReaderAt struct {
	reader io.ReaderAt
	digest hash.Hash
	offset int64
}

func (r *digestingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if offset != r.offset {
		return 0, fmt.Errorf("native encoder read source out of order")
	}
	n, err := r.reader.ReadAt(p, offset)
	_, _ = r.digest.Write(p[:n])
	r.offset += int64(n)
	return n, err
}

// Verify fully decodes native containers and runs the relevant external integrity check.
func Verify(ctx context.Context, path string, opts Options) (err error) {
	defer func() { err = dependencyError(err) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTools(opts.Tools); err != nil {
		return err
	}
	opts.TempDir, err = normalizeVerifyTempDir(opts.TempDir)
	if err != nil {
		return err
	}
	if opts.MaxOutput < 0 {
		return fmt.Errorf("max output must be positive")
	}
	if opts.MaxOutput == 0 {
		opts.MaxOutput = defaultMaxOutput
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	before, err := fileops.SnapshotStat(path, os.Stat)
	if err != nil {
		return err
	}
	info, err := Inspect(ctx, path)
	if err != nil {
		return err
	}
	if err := validateVerifyOptions(info.Format, opts); err != nil {
		return err
	}
	if info.Format == Raw {
		return fmt.Errorf("raw files have no container integrity check")
	}
	inputs := map[string]os.FileInfo{path: before}
	if err := checkInputs(inputs); err != nil {
		return err
	}
	if native(info.Format) {
		err = verifyNative(ctx, path, info, before, inputs, opts)
	} else {
		err = externalVerify(ctx, path, info.Format, opts)
	}
	if err != nil {
		return err
	}
	if err := checkInputs(inputs); err != nil {
		return fmt.Errorf("verification: %w", err)
	}
	return ctx.Err()
}

func validateVerifyOptions(format Format, opts Options) error {
	switch {
	case opts.ExtractAll:
		return fmt.Errorf("extract all is only available when decompressing")
	case opts.Entry != "" && format != ZIP:
		return fmt.Errorf("entry selection for verification requires ZIP input")
	case opts.Media != "" && (format != CHD || opts.Media != "cd" && opts.Media != "dvd" && opts.Media != "hdd"):
		return fmt.Errorf("media cd, dvd, or hdd is only used for CHD verification")
	case opts.Keys != "" && format != NSZ && format != XCZ:
		return fmt.Errorf("keys are only used for Switch verification")
	default:
		return nil
	}
}

func verifyNative(ctx context.Context, path string, info FileInfo, before os.FileInfo, inputs map[string]os.FileInfo, opts Options) error {
	notify := progressReporter(opts.Progress)
	options := codec.Options{Entry: opts.Entry, MaxOutput: opts.MaxOutput, Progress: func(n, t int64) { notify("verifying", n, t) }}
	if info.Format == WBFS {
		input, err := openWBFSInput(ctx, path)
		if err != nil {
			return err
		}
		defer input.close()
		if input.size != info.Size || !fileops.SameSnapshot(before, input.parts[0].info) {
			return fmt.Errorf("WBFS source changed during inspection")
		}
		input.addToSnapshot(inputs)
		return codec.Verify(ctx, string(WBFS), input, input.size, options)
	}
	f, opened, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if opened.Size() != info.Size || !fileops.SameSnapshot(before, opened) {
		return fmt.Errorf("source changed during inspection")
	}
	return codec.Verify(ctx, string(info.Format), f, info.Size, options)
}

func progressReporter(fn func(Progress)) func(string, int64, int64) {
	var notify func(string, int64, int64)
	if fn != nil {
		notify = func(phase string, bytes, total int64) { fn(Progress{Phase: phase, Bytes: bytes, Total: total}) }
	}
	return progress.Reporter(notify)
}
