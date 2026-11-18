package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/process"
	"github.com/olsonb97/romcompressor.go/internal/progress"
)

func convertPBP(ctx context.Context, source, output string, input string, opts Config) error {
	switch {
	case input == kind.PBP && opts.Format == kind.Raw:
		return extractPBP(ctx, source, output, opts)
	case input == kind.Raw && opts.Format == kind.PBP && strings.EqualFold(filepath.Ext(source), ".cue"):
		return compressPBP(ctx, source, output, opts)
	default:
		return fmt.Errorf("PS1 PBP compression requires a raw MODE2/2352 CUE/BIN disc, and extraction requires CUE output")
	}
}

func compressPBP(ctx context.Context, source, output string, opts Config) error {
	files, tracks, sectors, err := readPBPCue(ctx, source)
	if err != nil {
		return err
	}
	size := int64(sectors) * codec.PBPBytesPerSector
	if size > opts.MaxOutput {
		return fmt.Errorf("PBP source disc exceeds output size limit")
	}
	tool, err := resolveTool(opts, "psxpackager")
	if err != nil {
		return err
	}
	tool, err = filepath.Abs(tool)
	if err != nil {
		return err
	}
	engineDir, err := os.MkdirTemp(filepath.Dir(output), "pbp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(engineDir)
	const basename = "disc"
	bin := filepath.Join(engineDir, basename+".bin")
	cue := filepath.Join(engineDir, basename+".cue")
	packed := filepath.Join(engineDir, basename+".pbp")
	contents, err := pbpCueText(filepath.Base(bin), tracks)
	if err != nil {
		return err
	}
	if int64(len(contents)) > opts.MaxOutput-size {
		return fmt.Errorf("PBP input staging exceeds output size limit")
	}
	if err := fileops.WriteText(cue, contents); err != nil {
		return err
	}
	expected := sha256.New()
	if err := stagePBPTracks(ctx, bin, files, expected); err != nil {
		return err
	}
	// Pass only a normalized private descriptor and its generated BIN to the
	// engine. Its wildcard input handling never sees the caller's filenames.
	env := []string{"TMP=" + engineDir, "TEMP=" + engineDir, "TMPDIR=" + engineDir}
	if _, err := process.Execute(ctx, nil, env, engineDir, tool, "-i", cue, "-o", engineDir, "-v", "1", "-s"); err != nil {
		return err
	}
	info, err := fileops.SnapshotStat(packed, os.Lstat)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return fmt.Errorf("PSXPackager did not produce a regular PBP file")
	}
	if info.Size() > opts.MaxOutput-size-int64(len(contents)) {
		return fmt.Errorf("PBP staging exceeds output size limit")
	}
	if opts.Progress != nil {
		opts.Progress("verifying", 0, size)
	}
	if err := verifyPackedPBP(ctx, packed, info, opts.MaxOutput, tracks, sectors, expected.Sum(nil)); err != nil {
		return err
	}
	return fileops.Publish(packed, output)
}

func verifyPackedPBP(ctx context.Context, path string, info os.FileInfo, limit int64, tracks []codec.PBPTrack, sectors int, expected []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !fileops.SameSnapshot(info, current) {
		return fmt.Errorf("staged PBP changed before verification")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !fileops.SameSnapshot(info, opened) {
		return fmt.Errorf("staged PBP changed while opening")
	}
	if err := fileops.RequirePrivate(file); err != nil {
		return err
	}
	size := info.Size()
	if err := codec.SetPBPPregaps(file, size, tracks, sectors); err != nil {
		return err
	}
	disc, err := codec.ReadPBP(ctx, file, size)
	if err != nil {
		return err
	}
	if disc.Sectors != sectors || !slices.Equal(disc.Tracks, tracks) {
		return fmt.Errorf("PBP disc track indexes changed during conversion")
	}
	actual := sha256.New()
	if err := disc.Decode(ctx, actual, codec.Options{MaxOutput: limit}); err != nil {
		return err
	}
	if !bytes.Equal(actual.Sum(nil), expected) {
		return fmt.Errorf("PBP verification failed: restored disc sectors differ from the source")
	}
	return nil
}

func extractPBP(ctx context.Context, source, cue string, opts Config) error {
	file, _, err := fileops.OpenRegular(source)
	if err != nil {
		return err
	}
	defer file.Close()
	disc, err := openPBPDisc(ctx, file)
	if err != nil {
		return err
	}
	bin := strings.TrimSuffix(cue, filepath.Ext(cue)) + ".bin"
	contents, err := pbpCueText(filepath.Base(bin), disc.Tracks)
	if err != nil {
		return err
	}
	size := int64(disc.Sectors) * codec.PBPBytesPerSector
	if size > opts.MaxOutput-int64(len(contents)) {
		return fmt.Errorf("expanded PBP disc and CUE exceed output size limit")
	}
	output, err := os.OpenFile(bin, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	var decoded io.Writer = output
	expected := sha256.New()
	if opts.Progress != nil {
		decoded = io.MultiWriter(output, expected)
	}
	decodeErr := disc.Decode(ctx, decoded, pbpDecodeOptions(opts, "decompressing"))
	if decodeErr == nil && opts.Progress != nil {
		decodeErr = fileops.CheckDigest(ctx, output, size, expected.Sum(nil))
	}
	if err := errors.Join(decodeErr, output.Close()); err != nil {
		return err
	}
	return fileops.WriteText(cue, contents)
}

func verifyPBP(ctx context.Context, source string, opts Config) error {
	file, _, err := fileops.OpenRegular(source)
	if err != nil {
		return err
	}
	defer file.Close()
	disc, err := openPBPDisc(ctx, file)
	if err != nil {
		return err
	}
	return disc.Decode(ctx, io.Discard, pbpDecodeOptions(opts, "verifying"))
}

func openPBPDisc(ctx context.Context, file *os.File) (*codec.PBPDisc, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("PBP source must be a regular file")
	}
	return codec.ReadPBP(ctx, file, info.Size())
}

func pbpDecodeOptions(opts Config, phase string) codec.Options {
	notify := progress.Reporter(opts.Progress)
	return codec.Options{MaxOutput: opts.MaxOutput, Progress: func(n, total int64) { notify(phase, n, total) }}
}

type pbpCueFile struct {
	path    string
	info    os.FileInfo
	offset  int
	sectors int
}

func readPBPCue(ctx context.Context, source string) ([]pbpCueFile, []codec.PBPTrack, int, error) {
	var files []pbpCueFile
	var sectors int
	err := disc.VisitReferences(source, func(path string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.EqualFold(filepath.Ext(path), ".bin") {
			return fmt.Errorf("PBP cues must reference raw .bin files")
		}
		count, remainder := info.Size()/codec.PBPBytesPerSector, info.Size()%codec.PBPBytesPerSector
		if remainder != 0 || count == 0 || count > int64(codec.PBPMaxSectors-sectors) {
			return fmt.Errorf("invalid raw sector count in PBP cue reference %q", filepath.Base(path))
		}
		files = append(files, pbpCueFile{path, info, sectors, int(count)})
		sectors += int(count)
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	file, err := os.Open(source)
	if err != nil {
		return nil, nil, 0, err
	}
	defer file.Close()
	reader := &io.LimitedReader{R: &fileops.ContextReader{Context: ctx, Reader: file}, N: disc.MaxDescriptorSize + 1}
	tracks, err := parsePBPCue(filepath.Dir(source), reader, files)
	if err != nil {
		return nil, nil, 0, err
	}
	if reader.N == 0 {
		return nil, nil, 0, fmt.Errorf("PBP cue exceeds 4 MiB")
	}
	return files, tracks, sectors, nil
}

func parsePBPCue(root string, reader io.Reader, files []pbpCueFile) ([]codec.PBPTrack, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 510), 510)
	var tracks []codec.PBPTrack
	fileIndex, firstTrack := -1, 0
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		kind := strings.ToUpper(fields[0])
		switch kind {
		case "FILE":
			if fileIndex >= 0 && (len(tracks) == firstTrack || tracks[len(tracks)-1].Index1 < 0) {
				return nil, fmt.Errorf("PBP cue file has missing track indexes")
			}
			fileIndex++
			match := disc.MatchCUEFile(scanner.Text())
			if match == nil || fileIndex >= len(files) || !strings.EqualFold(fields[len(fields)-1], "BINARY") {
				return nil, fmt.Errorf("PBP cue requires FILE ... BINARY references")
			}
			name := match[1] + match[2]
			path := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(name, "\\", "/")))
			if path != files[fileIndex].path {
				return nil, fmt.Errorf("PBP cue references changed during validation")
			}
			firstTrack = len(tracks)
		case "TRACK":
			trackNumber := len(tracks) + 1
			if fileIndex < 0 || trackNumber > 99 || len(fields) != 3 || fields[1] != fmt.Sprintf("%02d", trackNumber) || (len(tracks) > 0 && tracks[len(tracks)-1].Index1 < 0) {
				return nil, fmt.Errorf("invalid PBP cue track on line %d", line)
			}
			mode := "MODE2/2352"
			if trackNumber > 1 {
				mode = "AUDIO"
			}
			if !strings.EqualFold(fields[2], mode) {
				return nil, fmt.Errorf("PBP requires a MODE2/2352 data track followed by optional AUDIO tracks")
			}
			tracks = append(tracks, codec.PBPTrack{Audio: trackNumber > 1, Index0: -1, Index1: -1})
		case "INDEX":
			if len(fields) != 3 || len(tracks) <= firstTrack || (fields[1] != "00" && fields[1] != "01") {
				return nil, fmt.Errorf("PBP cue requires INDEX 01 and can preserve one stored INDEX 00 per track")
			}
			track := &tracks[len(tracks)-1]
			file := files[fileIndex]
			position, err := parsePBPCueTime(fields[2])
			if err != nil || position >= file.sectors || track.Index1 >= 0 {
				return nil, fmt.Errorf("invalid PBP cue index on line %d", line)
			}
			position += file.offset
			if fields[1] == "00" {
				if track.Index0 >= 0 || len(tracks) == 1 {
					return nil, fmt.Errorf("PBP cannot represent the first track's stored pregap or duplicate indexes")
				}
				track.Index0 = position
				continue
			}
			if track.Index0 < 0 {
				track.Index0 = position
			}
			if track.Index0 > position || (len(tracks) == firstTrack+1 && track.Index0 != file.offset) ||
				(len(tracks) > 1 && track.Index0 <= tracks[len(tracks)-2].Index1) {
				return nil, fmt.Errorf("PBP cue has overlapping tracks or unassigned sectors on line %d", line)
			}
			track.Index1 = position
		case "REM", "TITLE", "PERFORMER", "SONGWRITER":
			if kind == "REM" && len(fields) > 1 && strings.EqualFold(fields[1], "SESSION") {
				return nil, fmt.Errorf("PBP does not support session commands")
			}
		default:
			return nil, fmt.Errorf("PBP cannot preserve CUE directive %s on line %d", kind, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(tracks) == firstTrack || tracks[len(tracks)-1].Index1 < 0 || fileIndex+1 != len(files) {
		return nil, fmt.Errorf("PBP cue has missing files or track indexes")
	}
	return tracks, nil
}

func parsePBPCueTime(value string) (int, error) {
	if len(value) != 8 || value[2] != ':' || value[5] != ':' {
		return 0, fmt.Errorf("invalid CUE timestamp")
	}
	for _, index := range []int{0, 1, 3, 4, 6, 7} {
		if value[index] < '0' || value[index] > '9' {
			return 0, fmt.Errorf("invalid CUE timestamp")
		}
	}
	m, _ := strconv.Atoi(value[:2])
	s, _ := strconv.Atoi(value[3:5])
	f, _ := strconv.Atoi(value[6:])
	if s >= 60 || f >= 75 {
		return 0, fmt.Errorf("invalid CUE timestamp")
	}
	return (m*60+s)*75 + f, nil
}

func stagePBPTracks(ctx context.Context, output string, files []pbpCueFile, hash io.Writer) (err error) {
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	writer := io.MultiWriter(file, hash)
	buffer := make([]byte, 128<<10)
	for _, entry := range files {
		input, err := os.Open(entry.path)
		if err != nil {
			return err
		}
		info, err := input.Stat()
		if err != nil || !os.SameFile(entry.info, info) || entry.info.Size() != info.Size() || !entry.info.ModTime().Equal(info.ModTime()) {
			input.Close()
			return fmt.Errorf("PBP source track changed before staging")
		}
		n, copyErr := io.CopyBuffer(writer, io.LimitReader(&fileops.ContextReader{Context: ctx, Reader: input}, info.Size()), buffer)
		if err := errors.Join(copyErr, input.Close()); err != nil {
			return err
		}
		if n != info.Size() {
			return fmt.Errorf("PBP source track changed size during staging")
		}
	}
	return nil
}

func pbpCueText(bin string, tracks []codec.PBPTrack) (string, error) {
	fileLine, err := disc.BinaryCUEFile(bin)
	if err != nil {
		return "", err
	}
	var contents strings.Builder
	contents.WriteString(fileLine)
	writeIndex := func(index, position int) {
		fmt.Fprintf(&contents, "    INDEX %02d %02d:%02d:%02d\n", index, position/(60*75), position/75%60, position%75)
	}
	for i, track := range tracks {
		mode := "MODE2/2352"
		if track.Audio {
			mode = "AUDIO"
		}
		fmt.Fprintf(&contents, "  TRACK %02d %s\n", i+1, mode)
		if track.Index0 < track.Index1 {
			writeIndex(0, track.Index0)
		}
		writeIndex(1, track.Index1)
	}
	return contents.String(), nil
}
