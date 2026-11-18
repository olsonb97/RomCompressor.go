package chd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

func parseGDTrack(text string, number int) (chdCueTrack, error) {
	var track chdCueTrack
	var index int
	var pgMode, pgSub string
	r := strings.NewReader(strings.TrimSpace(strings.TrimSuffix(text, "\x00")))
	_, err := fmt.Fscanf(r, "TRACK:%d TYPE:%s SUBTYPE:%s FRAMES:%d PAD:%d PREGAP:%d PGTYPE:%s PGSUB:%s POSTGAP:%d",
		&index, &track.mode, &track.subtype, &track.frames, &track.padFrames, &track.pregap, &pgMode, &pgSub, &track.postgap)
	if err != nil || r.Len() != 0 || index != number || track.frames <= 0 || track.frames > 1<<31-1 || track.padFrames < 0 || track.padFrames >= track.frames {
		return track, fmt.Errorf("invalid GD-ROM track %d metadata", number)
	}
	if track.subtype != "NONE" || track.pregap != 0 || track.postgap != 0 || pgSub != "NONE" {
		return track, fmt.Errorf("GDI cannot retain separate pregap, postgap or subcode metadata for GD-ROM track %d", number)
	}
	switch track.mode {
	case "AUDIO", "MODE1_RAW":
		track.sectorBytes = 2352
	case "MODE1":
		track.sectorBytes = 2048
	default:
		return track, fmt.Errorf("unsupported GDI track mode %q", track.mode)
	}
	return track, nil
}

// CDOutput reports the descriptor extension and number of separate GD-ROM tracks.
func CDOutput(ctx context.Context, source string, limit int64) (string, int, error) {
	layout, err := cdExtractionLayout(ctx, source, limit)
	if err != nil {
		return "", 0, err
	}
	if layout.gdrom {
		return ".gdi", len(layout.tracks), nil
	}
	return ".cue", 0, nil
}

// GDTrackStem gives each split track a predictable name for publication and M3Us.
func GDTrackStem(stem string, number int) string {
	// Keep unusual descriptor names usable without putting unrepresentable
	// characters into the GDI's quoted filename fields.
	if len(stem) > 200 || !utf8.ValidString(stem) || strings.ContainsAny(stem, "\"\\:") ||
		strings.IndexFunc(stem, unicode.IsControl) >= 0 || strings.Contains(stem, "  ") || strings.HasSuffix(stem, " ") {
		sum := sha256.Sum256([]byte(stem))
		stem = fmt.Sprintf("Disc-%x", sum[:8])
	}
	return fmt.Sprintf("%s (Track %02d)", stem, number)
}

func extractGDROM(ctx context.Context, tool, source, output string, layout cdManifest) error {
	stem := strings.TrimSuffix(filepath.Base(output), filepath.Ext(output))
	stage, err := fileops.NewStage(filepath.Dir(output), ".gdrom-*")
	if err != nil {
		return err
	}
	defer stage.Cleanup()
	// Both legacy and current chdman derive track01.bin, track02.raw, etc.
	// from track.gdi. Legacy versions ignore a custom output-bin template.
	if err := runCDExtraction(ctx, tool, source, stage, "track.gdi"); err != nil {
		return err
	}
	files := make([]chdCueFile, 0, len(layout.tracks))
	snapshots := make([]os.FileInfo, 0, len(layout.tracks))
	var descriptor strings.Builder
	fmt.Fprintln(&descriptor, len(layout.tracks))
	var lba int64
	for i, track := range layout.tracks {
		kind, ext := 4, ".bin"
		if track.mode == "AUDIO" {
			kind, ext = 0, ".raw"
		}
		path := filepath.Join(stage.Path(), fmt.Sprintf("track%02d%s", i+1, ext))
		info, err := fileops.SnapshotStat(path, os.Lstat)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != (track.frames-track.padFrames)*track.sectorBytes {
			return fmt.Errorf("GD-ROM track %d has the wrong file type or length", i+1)
		}
		name := GDTrackStem(stem, i+1) + ".bin"
		staged := filepath.Join(filepath.Dir(output), name)
		if err := fileops.Publish(path, staged); err != nil {
			return err
		}
		files = append(files, chdCueFile{path: staged, tracks: []chdCueTrack{track}})
		snapshots = append(snapshots, info)
		fmt.Fprintf(&descriptor, "%d %d %d %d \"%s\" 0\n", i+1, lba, kind, track.sectorBytes, name)
		lba += track.frames
	}
	// Reassemble the normalized CHD hash with the correct audio byte order,
	// omitted zero sectors, and four-frame track alignment before publishing.
	if err := layout.checkTrackData(ctx, files); err != nil {
		return err
	}
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(file.path)
		if err != nil {
			return err
		}
		if !fileops.SameSnapshot(snapshots[i], info) {
			return fmt.Errorf("GD-ROM track %d changed during verification", i+1)
		}
	}
	return fileops.WriteText(output, descriptor.String())
}
