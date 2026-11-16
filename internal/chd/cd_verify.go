package chd

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

const chdCDFrameBytes = 2448

func verifyRestoredCHDCD(ctx context.Context, source, cue, bin string, limit int64) error {
	restored, err := readExtractedCHDCUE(cue, bin, limit)
	if err != nil {
		return err
	}
	image, supported, err := checkCHDCDManifest(source, restored.tracks)
	if err != nil || !supported {
		return err
	}
	return checkRestoredCHDCDData(ctx, image, restored)
}

// GD-ROM, multisession, and legacy CHDs can use other frame and gap layouts.
// Only modern single-session CD metadata describes this normalization.
func checkCHDCDManifest(source string, tracks []chdCueTrack) (rawCHD, bool, error) {
	file, info, err := fileops.OpenRegular(source)
	if err != nil {
		return rawCHD{}, false, err
	}
	defer file.Close()
	var header [chdV5HeaderSize]byte
	if _, err := io.ReadFull(file, header[:16]); err != nil {
		return rawCHD{}, false, err
	}
	if binary.BigEndian.Uint32(header[12:16]) != 5 {
		return rawCHD{}, false, nil
	}
	image, err := readCHDV5Header(io.NewSectionReader(file, 0, info.Size()), &header)
	if err != nil || image.sectorSize != chdCDFrameBytes {
		return image, false, err
	}
	var entries []chdMetadata
	otherLayout := false
	err = walkCHDMetadata(file, uint64(info.Size()), chdV5HeaderSize, binary.BigEndian.Uint64(header[48:56]), func(entry chdMetadata) error {
		switch entry.tag {
		case "CHTR", "CHT2":
			entries = append(entries, entry)
		case "CHCD", "CHGT", "CHGD", "CHSE":
			otherLayout = true
		}
		return nil
	})
	if err != nil || otherLayout || len(entries) == 0 {
		return image, false, err
	}
	if len(entries) != len(tracks) {
		return image, false, fmt.Errorf("restored CUE track count differs from the CHD metadata")
	}
	for index, entry := range entries {
		var metadata [256]byte
		if entry.length == 0 || entry.length > uint64(len(metadata)) {
			return image, false, fmt.Errorf("invalid CD CHD track metadata length")
		}
		if _, err := file.ReadAt(metadata[:entry.length], entry.offset); err != nil {
			return image, false, err
		}
		if err := matchCHDCDTrack(entry.tag, strings.TrimRight(string(metadata[:entry.length]), "\x00"), index+1, tracks[index]); err != nil {
			return image, false, err
		}
	}
	return image, true, nil
}

func matchCHDCDTrack(tag, metadata string, number int, track chdCueTrack) error {
	var trackNumber int
	var mode, subtype, pregapType, pregapSub string
	var frames, pregap, postgap int64
	var err error
	if tag == "CHT2" {
		_, err = fmt.Sscanf(metadata, "TRACK:%d TYPE:%s SUBTYPE:%s FRAMES:%d PREGAP:%d PGTYPE:%s PGSUB:%s POSTGAP:%d",
			&trackNumber, &mode, &subtype, &frames, &pregap, &pregapType, &pregapSub, &postgap)
	} else {
		_, err = fmt.Sscanf(metadata, "TRACK:%d TYPE:%s SUBTYPE:%s FRAMES:%d", &trackNumber, &mode, &subtype, &frames)
	}
	if err != nil || subtype != "NONE" {
		return fmt.Errorf("invalid or unrepresentable CD CHD track metadata")
	}
	sectorBytes, err := chdCueSectorSize([]string{mode})
	storedPregap := track.index1 - track.index0
	if strings.HasPrefix(pregapType, "V") {
		storedPregap, pregap = pregap-storedPregap, 0
	}
	if err != nil || trackNumber != number || track.frames != frames || chdCueTrackFamily(track.mode) != chdCueTrackFamily(mode) ||
		track.sectorBytes != sectorBytes || storedPregap != 0 || track.pregap != pregap || track.postgap != postgap {
		return fmt.Errorf("restored CUE track %d differs from the CHD metadata", number)
	}
	return nil
}

func chdCueTrackFamily(mode string) string {
	switch {
	case strings.HasPrefix(mode, "MODE1"):
		return "MODE1"
	case strings.HasPrefix(mode, "MODE2") || strings.HasPrefix(mode, "CDI"):
		return "MODE2"
	default:
		return mode
	}
}

func checkRestoredCHDCDData(ctx context.Context, image rawCHD, restored chdCueFile) error {
	file, _, err := fileops.OpenRegular(restored.path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 128<<10)
	digest := sha1.New()
	var logicalBytes int64
	var padding [chdCDFrameBytes]byte
	for _, track := range restored.tracks {
		padded := (track.frames + 3) &^ 3
		if padded > (image.size-logicalBytes)/chdCDFrameBytes {
			return fmt.Errorf("restored BIN exceeds the CD CHD's logical size")
		}
		var frame [chdCDFrameBytes]byte
		for range track.frames {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := io.ReadFull(reader, frame[:track.sectorBytes]); err != nil {
				return err
			}
			if track.mode == "AUDIO" {
				for i := 0; i < int(track.sectorBytes); i += 2 {
					frame[i], frame[i+1] = frame[i+1], frame[i]
				}
			}
			digest.Write(frame[:])
		}
		for i := track.frames; i < padded; i++ {
			digest.Write(padding[:])
		}
		logicalBytes += padded * chdCDFrameBytes
	}
	var actual [sha1.Size]byte
	digest.Sum(actual[:0])
	if logicalBytes != image.size || actual != image.digest {
		return fmt.Errorf("restored BIN does not match the CD CHD raw SHA-1")
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		return fmt.Errorf("restored BIN has unexpected data after its last track")
	}
	return ctx.Err()
}
