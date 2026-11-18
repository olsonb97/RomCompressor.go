package chd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

type cdManifest struct {
	tracks                   []chdCueTrack // INDEX 01 is relative to the track's first stored sector.
	image                    rawCHD        // v5 checksum of the normalized CD frames.
	gdrom, littleEndianAudio bool
}

func readCDManifest(ctx context.Context, path string, limit int64) (cdManifest, error) {
	if err := ctx.Err(); err != nil {
		return cdManifest{}, err
	}
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return cdManifest{}, err
	}
	defer file.Close()
	var chtr, cht2, chgd, chgt []chdMetadata
	var legacyCD bool
	version, _, err := readMetadata(ctx, file, info.Size(), limit, "cd", func(entry chdMetadata) error {
		switch entry.tag {
		case "CHTR":
			chtr = append(chtr, entry)
		case "CHT2":
			cht2 = append(cht2, entry)
		case "CHGT":
			chgt = append(chgt, entry)
		case "CHGD":
			chgd = append(chgd, entry)
		case "CHCD", "CHSE":
			legacyCD = true
		}
		return nil
	})
	if err != nil {
		return cdManifest{}, err
	}
	// chdman tries CHTR, CHT2, CHGT and CHGD at each track index.
	count := max(len(chtr), len(cht2))
	var layout cdManifest
	if gdCount := max(len(chgd), len(chgt)); gdCount > count {
		if version != 5 || count != 0 {
			return cdManifest{}, fmt.Errorf("GD-ROM restoration requires a v5 CHD with a dedicated GD-ROM track table")
		}
		layout.gdrom, layout.littleEndianAudio = true, len(chgt) > 0
		chtr, cht2, count = chgt, chgd, gdCount
	}
	if count == 0 && legacyCD {
		return cdManifest{}, nil
	}
	if count == 0 || count > 99 {
		return cdManifest{}, fmt.Errorf("invalid CD CHD track count")
	}
	for i := range count {
		var entry chdMetadata
		if i < len(chtr) {
			entry = chtr[i]
		} else {
			entry = cht2[i]
		}
		if err := ctx.Err(); err != nil {
			return cdManifest{}, err
		}
		var data [256]byte
		if entry.length == 0 || entry.length > uint64(len(data)) {
			return cdManifest{}, fmt.Errorf("invalid CD CHD track metadata length")
		}
		if _, err := file.ReadAt(data[:entry.length], entry.offset); err != nil {
			return cdManifest{}, err
		}
		var track chdCueTrack
		text := string(data[:entry.length])
		if layout.gdrom {
			track, err = parseGDTrack(text, i+1)
		} else {
			track, err = parseCDTrack(entry.tag, text, i+1)
		}
		if err != nil {
			return cdManifest{}, err
		}
		layout.tracks = append(layout.tracks, track)
	}
	if layout.gdrom && layout.tracks[count-1].padFrames != 0 {
		return cdManifest{}, fmt.Errorf("GDI cannot represent padding after the final GD-ROM track")
	}
	if layout.gdrom {
		var sectors int64
		for _, track := range layout.tracks {
			sectors += track.frames
		}
		if sectors > 1<<31-1 {
			return cdManifest{}, fmt.Errorf("GD-ROM exceeds the GDI sector address range")
		}
	}
	if version == 5 {
		var header [chdV5HeaderSize]byte
		layout.image, err = readCHDV5Header(io.NewSectionReader(file, 0, info.Size()), &header)
		if err != nil {
			return cdManifest{}, err
		}
		if layout.image.sectorSize != chdCDFrameBytes {
			return cdManifest{}, fmt.Errorf("invalid CD CHD frame size")
		}
		left := layout.image.size
		for _, track := range layout.tracks {
			if err := chargeCDTrack(&left, track.frames); err != nil {
				return cdManifest{}, fmt.Errorf("CD tracks exceed the CHD logical size")
			}
		}
		if left != 0 {
			return cdManifest{}, fmt.Errorf("CD track lengths disagree with the CHD logical size")
		}
	}
	return layout, nil
}

func parseCDTrack(tag, text string, number int) (chdCueTrack, error) {
	var track chdCueTrack
	var index int
	var pgMode, pgSub string
	var err error
	r := strings.NewReader(strings.TrimSpace(strings.TrimSuffix(text, "\x00")))
	if tag == "CHT2" {
		_, err = fmt.Fscanf(r, "TRACK:%d TYPE:%s SUBTYPE:%s FRAMES:%d PREGAP:%d PGTYPE:%s PGSUB:%s POSTGAP:%d",
			&index, &track.mode, &track.subtype, &track.frames, &track.pregap, &pgMode, &pgSub, &track.postgap)
	} else {
		_, err = fmt.Fscanf(r, "TRACK:%d TYPE:%s SUBTYPE:%s FRAMES:%d", &index, &track.mode, &track.subtype, &track.frames)
	}
	if err != nil || r.Len() != 0 || index != number || track.frames <= 0 || track.frames > 1<<32-1 || track.pregap < 0 || track.postgap < 0 {
		return track, fmt.Errorf("invalid CD CHD track %d metadata", number)
	}
	if track.subtype != "NONE" && track.subtype != "RW" && track.subtype != "RW_RAW" {
		return track, fmt.Errorf("invalid CD CHD track %d subcode", number)
	}
	track.mode = canonicalCDMode(track.mode)
	track.sectorBytes, err = chdCueSectorSize([]string{track.mode, track.subtype})
	if err != nil {
		return track, err
	}
	if track.pregap > 0 {
		if pgSub != "NONE" {
			return track, fmt.Errorf("CUE cannot retain pregap subcode for CHD track %d", number)
		}
		if mode, stored := strings.CutPrefix(pgMode, "V"); stored {
			if canonicalCDMode(mode) != track.mode || track.pregap >= track.frames {
				return track, fmt.Errorf("CUE cannot retain the stored pregap layout for CHD track %d", number)
			}
			track.index1, track.pregap = track.pregap, 0
		}
	}
	return track, nil
}

func cdExtractionLayout(ctx context.Context, path string, limit int64) (cdManifest, error) {
	layout, err := readCDManifest(ctx, path, limit)
	if err != nil {
		return cdManifest{}, err
	}
	for _, track := range layout.tracks {
		if track.subtype != "NONE" {
			return cdManifest{}, fmt.Errorf("CUE/BIN cannot retain the CHD's subcode data")
		}
	}
	return layout, nil
}
