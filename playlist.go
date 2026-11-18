package romcompressor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/olsonb97/romcompressor.go/internal/chd"
	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

const (
	maxPlaylistSize  = 1 << 20
	maxPlaylistDiscs = 1000
)

// ConvertPlaylist converts every disc into a new companion folder and publishes
// the playlist after the entire set succeeds. Existing sets are never replaced.
func ConvertPlaylist(ctx context.Context, source, output string, opts Options) (result Result, err error) {
	defer func() { err = dependencyError(err) }()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateTools(opts.Tools); err != nil {
		return Result{}, err
	}
	switch opts.Format {
	case Raw, CSO, ZSO, CHD, PBP, RVZ, WIA, GCZ, WBFS, CISO:
	default:
		return Result{}, fmt.Errorf("playlist conversion requires raw, cso, zso, chd, pbp, rvz, wia, gcz, wbfs, or ciso output")
	}
	if opts.MaxOutput < 0 {
		return Result{}, fmt.Errorf("max output must be positive")
	}
	if opts.MaxOutput == 0 {
		opts.MaxOutput = defaultMaxOutput
	}
	if opts.Entry != "" || opts.Keys != "" || opts.ExtractAll {
		return Result{}, fmt.Errorf("entry, extract all, and keys are unavailable for disc playlists")
	}
	if opts.TempDir != "" {
		return Result{}, fmt.Errorf("temp dir is only available for standalone verification")
	}
	if opts.Media != "" && (opts.Format != CHD && opts.Format != Raw || opts.Media != "cd" && opts.Media != "dvd") {
		return Result{}, fmt.Errorf("media cd or dvd is only available when creating or restoring CHD playlists")
	}
	if source == "" || output == "" {
		return Result{}, fmt.Errorf("source and output are required")
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return Result{}, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return Result{}, err
	}
	if !isPlaylist(output) || !filepath.IsLocal(filepath.Base(output)) {
		return Result{}, fmt.Errorf("playlist output must use .m3u or .m3u8")
	}
	folderName := strings.TrimSuffix(filepath.Base(output), filepath.Ext(output))
	if runtime.GOOS == "windows" {
		// Win32 can remove trailing spaces and dots during directory creation.
		// Use the name that will exist both on disk and in the playlist.
		folderName = strings.TrimRight(folderName, " .")
	}
	if !validPlaylistComponent(folderName) {
		return Result{}, fmt.Errorf("companion folder name cannot be represented in a playlist")
	}
	companion := filepath.Join(filepath.Dir(output), folderName)
	for _, destination := range []string{output, companion} {
		if _, err := os.Lstat(destination); err == nil {
			return Result{}, &os.PathError{Op: "create playlist output or companion folder", Path: destination, Err: os.ErrExist}
		} else if !os.IsNotExist(err) {
			return Result{}, err
		}
	}
	discs, before, err := readPlaylist(ctx, source)
	if err != nil {
		return Result{}, err
	}
	inputs := map[string]os.FileInfo{source: before}
	stage, err := fileops.NewStage(filepath.Dir(output), ".romcompressor-playlist-*")
	if err != nil {
		return Result{}, err
	}
	defer stage.Cleanup()
	stagedDiscs := make(map[string]playlistStagedFile, len(discs))
	var digestBuffer []byte
	if opts.Progress != nil {
		digestBuffer = make([]byte, 128<<10)
	}
	names := make(map[string]bool)
	var lines []string
	var inputBytes, outputBytes int64
	for index, entry := range discs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if err := stage.Check(); err != nil {
			return Result{}, err
		}
		disc := entry.path
		selected := map[string]os.FileInfo{entry.reference: entry.info, disc: entry.info}
		if err := checkInputs(selected); err != nil {
			return Result{}, fmt.Errorf("disc %d: %w", index+1, err)
		}
		if err := rememberPlaylistInputs(inputs, selected); err != nil {
			return Result{}, fmt.Errorf("disc %d: %w", index+1, err)
		}
		options := opts
		options.MaxOutput -= outputBytes
		if options.MaxOutput <= 0 {
			return Result{}, fmt.Errorf("disc set exceeds output size limit")
		}
		if opts.Progress != nil {
			options.Progress = func(progress Progress) {
				if progress.Phase == "complete" || progress.Phase == "publishing" {
					return
				}
				progress.Phase = fmt.Sprintf("disc %d/%d %s", index+1, len(discs), progress.Phase)
				opts.Progress(progress)
			}
		}
		info, err := Inspect(ctx, disc)
		if err != nil {
			return Result{}, fmt.Errorf("disc %d: %w", index+1, err)
		}
		name := ""
		if opts.Format == Raw {
			var extension string
			var splitTracks int
			options, extension, splitTracks, err = playlistRestoreOptions(ctx, disc, info, options)
			if err != nil {
				return Result{}, fmt.Errorf("disc %d: %w", index+1, err)
			}
			name = playlistRestoredDiscName(disc, info.Format, extension, splitTracks, names)
		} else {
			name = playlistDiscName(disc, opts.Format, names)
		}
		if !validPlaylistComponent(name) {
			return Result{}, fmt.Errorf("disc filename cannot be represented in a playlist")
		}
		destination := filepath.Join(stage.Path(), name)
		var result Result
		if info.Format == opts.Format {
			result, err = copyPlaylistDisc(ctx, disc, destination, options, inputs)
		} else {
			result, err = convertFile(ctx, disc, destination, options, inputs)
		}
		if err != nil {
			return Result{}, fmt.Errorf("disc %d: %w", index+1, err)
		}
		if err := stage.Check(); err != nil {
			return Result{}, err
		}
		if err := rememberStagedDisc(ctx, destination, stagedDiscs, digestBuffer); err != nil {
			return Result{}, fmt.Errorf("disc %d: %w", index+1, err)
		}
		if result.OutputBytes > opts.MaxOutput-outputBytes {
			return Result{}, fmt.Errorf("disc set exceeds output size limit")
		}
		outputBytes += result.OutputBytes
		inputBytes += result.InputBytes
		// A leading # denotes a comment in M3U. The ./ prefix also protects
		// significant leading spaces in the companion folder's name.
		lines = append(lines, "./"+filepath.ToSlash(filepath.Join(folderName, name)))
	}
	contents := strings.Join(lines, "\n") + "\n"
	if int64(len(contents)) > opts.MaxOutput-outputBytes {
		return Result{}, fmt.Errorf("disc set exceeds output size limit")
	}
	if err := stage.Check(); err != nil {
		return Result{}, err
	}
	stagedPlaylist := filepath.Join(stage.Path(), filepath.Base(output))
	if err := fileops.WriteText(stagedPlaylist, contents); err != nil {
		return Result{}, err
	}
	if err := checkInputs(inputs); err != nil {
		return Result{}, err
	}
	if err := publishPlaylist(ctx, stagedPlaylist, companion, output, stagedDiscs, digestBuffer); err != nil {
		return Result{}, err
	}
	outputBytes += int64(len(contents))
	if opts.Progress != nil {
		opts.Progress(Progress{Phase: "complete", Bytes: outputBytes, Total: outputBytes})
	}
	return Result{Output: output, Format: opts.Format, InputBytes: inputBytes + before.Size(), OutputBytes: outputBytes, Verified: true}, nil
}

func isPlaylist(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".m3u" || ext == ".m3u8"
}

type playlistDisc struct {
	reference string
	path      string
	info      os.FileInfo
}

func readPlaylist(ctx context.Context, path string) ([]playlistDisc, os.FileInfo, error) {
	f, st, err := fileops.OpenRegular(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	if st.Size() > maxPlaylistSize {
		return nil, nil, fmt.Errorf("playlist must be a regular file of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(&fileops.ContextReader{Context: ctx, Reader: f}, maxPlaylistSize+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxPlaylistSize || !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("playlist must contain UTF-8 text of at most 1 MiB")
	}
	root, err := fileops.Resolve(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	var discs []playlistDisc
	scanner := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff")))
	scanner.Buffer(make([]byte, 4096), maxPlaylistSize+1)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		name := strings.TrimSpace(scanner.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		if strings.HasPrefix(name, "\"") && strings.HasSuffix(name, "\"") && len(name) >= 2 {
			name = name[1 : len(name)-1]
		}
		name = strings.ReplaceAll(name, "\\", "/")
		if strings.HasPrefix(name, "/") || strings.ContainsAny(name, ":\x00") {
			return nil, nil, fmt.Errorf("playlist entries must be relative file paths")
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return nil, nil, fmt.Errorf("playlist entry escapes its folder")
			}
		}
		disc := filepath.Join(root, filepath.FromSlash(name))
		resolved, err := fileops.Resolve(disc)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve playlist disc: %w", err)
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, nil, fmt.Errorf("playlist disc resolves outside its folder")
		}
		if isPlaylist(resolved) {
			return nil, nil, fmt.Errorf("nested playlists are unsupported; choose disc images")
		}
		info, err := fileops.SnapshotStat(resolved, os.Lstat)
		if err != nil {
			return nil, nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("playlist disc is not a regular file")
		}
		discs = append(discs, playlistDisc{reference: disc, path: resolved, info: info})
		if len(discs) > maxPlaylistDiscs {
			return nil, nil, fmt.Errorf("playlist exceeds %d discs", maxPlaylistDiscs)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	if len(discs) == 0 {
		return nil, nil, fmt.Errorf("playlist contains no discs")
	}
	return discs, st, nil
}

func validPlaylistComponent(name string) bool {
	return name != "." && filepath.IsLocal(name) && utf8.ValidString(name) && !strings.ContainsAny(name, ":/\\\r\n\x00")
}

func playlistDiscName(source string, format Format, used map[string]bool) string {
	stem := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	name := stem + "." + string(format)
	for suffix := 2; used[strings.ToLower(name)]; suffix++ {
		name = fmt.Sprintf("%s-%d.%s", stem, suffix, format)
	}
	used[strings.ToLower(name)] = true
	return name
}

// Playlist restoration keeps each logical disc in the flat companion folder.
func playlistRestoreOptions(ctx context.Context, source string, info FileInfo, opts Options) (Options, string, int, error) {
	opts.Format = Raw
	if info.Format == CHD {
		if info.Media != "" && info.Media != "cd" && info.Media != "dvd" {
			return Options{}, "", 0, fmt.Errorf("CHD %s media is not supported in a disc playlist", info.Media)
		}
		if opts.Media == "" {
			opts.Media = info.Media
		}
		if opts.Media != "cd" && opts.Media != "dvd" {
			return Options{}, "", 0, fmt.Errorf("untagged CHD playlist restoration requires media cd or dvd")
		}
		if info.Media != "" && info.Media != opts.Media {
			return Options{}, "", 0, fmt.Errorf("CHD %s metadata conflicts with requested %s media", info.Media, opts.Media)
		}
		if opts.Media == "cd" {
			ext, count, err := chd.CDOutput(ctx, source, opts.MaxOutput)
			return opts, ext, count, err
		}
		return opts, ".iso", 0, nil
	}
	// A set may mix CHD with other image formats. A global CHD media choice
	// must not become an ignored or invalid option for those other discs.
	opts.Media = ""
	switch info.Format {
	case CSO, ZSO, RVZ, WIA, GCZ, WBFS, CISO, CCI:
		return opts, ".iso", 0, nil
	case PBP:
		return opts, ".cue", 0, nil
	case ECM:
		extension := strings.ToLower(filepath.Ext(strings.TrimSuffix(source, filepath.Ext(source))))
		switch extension {
		case ".bin", ".img", ".iso", ".cdi", ".nrg":
			return opts, extension, 0, nil
		default:
			return opts, ".bin", 0, nil
		}
	case Raw:
		extension := strings.ToLower(filepath.Ext(source))
		switch extension {
		case ".cue", ".gdi":
			return Options{}, "", 0, fmt.Errorf("already-raw CUE/GDI playlist entries require their complete track sets and cannot be copied as one image")
		case ".iso", ".gcm", ".xiso", ".bin", ".img", ".cdr", ".toast", ".cdi", ".nrg":
			return opts, extension, 0, nil
		default:
			return Options{}, "", 0, fmt.Errorf("raw playlist entry must be a supported disc image")
		}
	default:
		return Options{}, "", 0, fmt.Errorf("%s cannot be restored as a complete disc playlist entry", info.Format)
	}
}

func playlistRestoredDiscName(source string, input Format, extension string, splitTracks int, used map[string]bool) string {
	stem := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	if input == ECM && strings.EqualFold(filepath.Ext(stem), extension) {
		stem = strings.TrimSuffix(stem, filepath.Ext(stem))
	}
	for suffix := 1; ; suffix++ {
		selected := stem
		if suffix > 1 {
			selected = fmt.Sprintf("%s-%d", stem, suffix)
		}
		if used[strings.ToLower(selected)] {
			continue
		}
		names := []string{strings.ToLower(selected)}
		conflict := false
		for n := 1; n <= splitTracks; n++ {
			name := strings.ToLower(chd.GDTrackStem(selected, n))
			if used[name] {
				conflict = true
				break
			}
			names = append(names, name)
		}
		if !conflict {
			for _, name := range names {
				used[name] = true
			}
			return selected + extension
		}
	}
}

func copyPlaylistDisc(ctx context.Context, source, output string, opts Options, playlistInputs map[string]os.FileInfo) (Result, error) {
	before, err := fileops.SnapshotStat(source, os.Stat)
	if err != nil {
		return Result{}, err
	}
	size := before.Size()
	inputs := map[string]os.FileInfo{source: before}
	var src io.Reader
	if opts.Format == WBFS {
		input, err := openWBFSInput(ctx, source)
		if err != nil {
			return Result{}, err
		}
		defer input.close()
		input.addToSnapshot(inputs)
		size = input.size
		src = io.NewSectionReader(input, 0, size)
	} else {
		file, actual, err := fileops.OpenRegular(source)
		if err != nil {
			return Result{}, err
		}
		defer file.Close()
		if !fileops.SameSnapshot(before, actual) {
			return Result{}, fmt.Errorf("playlist disc changed while opening")
		}
		src = file
	}
	if size > opts.MaxOutput {
		return Result{}, fmt.Errorf("disc exceeds output size limit")
	}
	sidecar, err := findDiscSidecar(source, output)
	if err != nil {
		return Result{}, err
	}
	if sidecar != nil {
		inputs[sidecar.path] = sidecar.info
	}
	if err := rememberPlaylistInputs(playlistInputs, inputs); err != nil {
		return Result{}, err
	}
	if opts.Format == Raw {
		if opts.Progress != nil {
			opts.Progress(Progress{Phase: "copying disc", Total: size})
		}
	} else {
		if err := Verify(ctx, source, opts); err != nil {
			return Result{}, err
		}
	}
	if err := checkInputs(inputs); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	dst, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Result{}, err
	}
	var writer io.Writer = dst
	copiedDigest := sha256.New()
	if opts.Format == Raw {
		writer = io.MultiWriter(dst, copiedDigest)
	}
	n, copyErr := io.CopyBuffer(writer, io.LimitReader(&fileops.ContextReader{Context: ctx, Reader: src}, size), make([]byte, 128<<10))
	syncErr := dst.Sync()
	var verifyErr error
	if copyErr == nil && syncErr == nil && n == size && opts.Format == Raw {
		verifyErr = fileops.CheckDigest(ctx, dst, size, copiedDigest.Sum(nil))
	}
	closeErr := dst.Close()
	if err := errors.Join(copyErr, syncErr, verifyErr, closeErr); err != nil {
		return Result{}, err
	}
	if n != size {
		return Result{}, fmt.Errorf("disc size changed during copy")
	}
	// Source verification reports progress. Keep caller callbacks out of the
	// private copy's verification before recording the verified bytes.
	opts.Progress = nil
	if opts.Format != Raw {
		if err := Verify(ctx, output, opts); err != nil {
			return Result{}, err
		}
	}
	if sidecar != nil {
		if _, err := sidecar.stage(ctx, output, opts.MaxOutput-n); err != nil {
			return Result{}, err
		}
		n += sidecar.info.Size()
	}
	if err := checkInputs(inputs); err != nil {
		return Result{}, err
	}
	return Result{Output: output, Format: opts.Format, InputBytes: n, OutputBytes: n, Verified: true}, nil
}

func rememberPlaylistInputs(playlistInputs, discInputs map[string]os.FileInfo) error {
	if playlistInputs == nil {
		return nil
	}
	for path, info := range discInputs {
		if before, seen := playlistInputs[path]; seen {
			if !fileops.SameSnapshot(before, info) {
				return fmt.Errorf("source changed during playlist conversion")
			}
		}
		playlistInputs[path] = info
	}
	return nil
}

type playlistStagedFile struct {
	info   os.FileInfo
	digest [sha256.Size]byte
}

func stagedPlaylistDigest(ctx context.Context, path string, info os.FileInfo, buffer []byte) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	digest := sha256.New()
	if err := streamSnapshotFile(ctx, digest, path, info, buffer); err != nil {
		return sum, err
	}
	digest.Sum(sum[:0])
	return sum, ctx.Err()
}

func rememberStagedDisc(ctx context.Context, path string, stagedDiscs map[string]playlistStagedFile, digestBuffer []byte) error {
	record := func(path string, info os.FileInfo) error {
		if previous, recorded := stagedDiscs[path]; recorded {
			if !fileops.SameSnapshot(previous.info, info) {
				return fmt.Errorf("staged disc set file changed while recording")
			}
			return nil
		}
		file := playlistStagedFile{info: info}
		if digestBuffer != nil {
			var err error
			file.digest, err = stagedPlaylistDigest(ctx, path, info, digestBuffer)
			if err != nil {
				return err
			}
		}
		stagedDiscs[path] = file
		return nil
	}
	info, err := fileops.SnapshotStat(path, os.Lstat)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged disc must be a regular file")
	}
	if err := record(path, info); err != nil {
		return err
	}
	if err := disc.VisitReferences(path, func(reference string, info os.FileInfo) error {
		if filepath.Dir(reference) != filepath.Dir(path) {
			return fmt.Errorf("restored disc tracks must remain in the companion folder")
		}
		return record(reference, info)
	}); err != nil {
		return err
	}
	if discSidecarSupported(path) {
		sidecar, err := findSBI(path)
		if err != nil {
			return err
		}
		if sidecar != nil {
			return record(sidecar.path, sidecar.info)
		}
	}
	return nil
}

func publishPlaylist(ctx context.Context, stagedPlaylist, companion, output string, stagedDiscs map[string]playlistStagedFile, digestBuffer []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stage := filepath.Dir(stagedPlaylist)
	entries, err := readPublishEntries(stage, len(stagedDiscs)+2)
	if err != nil {
		return err
	}
	if len(entries) != len(stagedDiscs)+1 {
		return fmt.Errorf("staging does not contain the expected disc set")
	}
	var paths []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected entry in disc set")
		}
		path := filepath.Join(stage, entry.Name())
		if path == stagedPlaylist {
			continue
		}
		expected, ok := stagedDiscs[path]
		if !ok {
			return fmt.Errorf("unexpected staged disc set file %q", entry.Name())
		}
		current, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() || !fileops.SameSnapshot(expected.info, current) {
			return fmt.Errorf("staged disc set file %q changed", entry.Name())
		}
		if digestBuffer != nil {
			actual, err := stagedPlaylistDigest(ctx, path, expected.info, digestBuffer)
			if err != nil {
				return err
			}
			if actual != expected.digest {
				return fmt.Errorf("staged disc set file %q changed after verification", entry.Name())
			}
		}
		paths = append(paths, path)
	}
	if err := os.Mkdir(companion, 0700); err != nil {
		return fmt.Errorf("create companion folder without replacing files: %w", err)
	}
	folder, err := os.Open(companion)
	if err != nil {
		return err
	}
	folderInfo, err := folder.Stat()
	if err := errors.Join(err, folder.Close()); err != nil {
		return err
	}
	created := []publishedPath{{path: companion, info: folderInfo}}
	for _, path := range paths {
		destination := filepath.Join(companion, filepath.Base(path))
		item, err := publishRegularFile(ctx, path, destination, stagedDiscs[path].info)
		if err != nil {
			return rollbackPublished(created, err)
		}
		created = append(created, item)
	}
	if _, err := publishRegularFile(ctx, stagedPlaylist, output, nil); err != nil {
		return rollbackPublished(created, err)
	}
	return nil
}
