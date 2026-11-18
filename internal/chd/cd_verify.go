package chd

import (
	"bufio"
	"context"
	"crypto/sha1"
	"fmt"
	"hash"
	"io"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

const chdCDFrameBytes = 2448

func (layout cdManifest) verify(ctx context.Context, files []chdCueFile) error {
	if len(layout.tracks) == 0 {
		// Legacy binary CD metadata uses a different track layout.
		return ctx.Err()
	}
	index := 0
	for _, file := range files {
		for _, track := range file.tracks {
			if index >= len(layout.tracks) {
				return fmt.Errorf("CUE track count differs from the CHD metadata")
			}
			want := layout.tracks[index]
			if canonicalCDMode(track.mode) != want.mode || track.subtype != want.subtype ||
				track.frames != want.frames || track.index1-track.index0 != want.index1 ||
				track.pregap != want.pregap || track.postgap != want.postgap {
				return fmt.Errorf("CUE track %d differs from the CHD metadata", index+1)
			}
			index++
		}
	}
	if index != len(layout.tracks) {
		return fmt.Errorf("CUE track count differs from the CHD metadata")
	}
	if layout.image.size == 0 {
		return ctx.Err() // Older CHD versions use a different checksum contract.
	}
	return layout.checkTrackData(ctx, files)
}

func verifyCreatedCUE(ctx context.Context, source, output string, limit int64) error {
	files, err := readCHDCUE(source, "", limit)
	if err != nil {
		return err
	}
	layout, err := readCDManifest(ctx, output, limit)
	if err != nil {
		return err
	}
	if layout.gdrom || len(layout.tracks) == 0 || layout.image.size == 0 {
		return fmt.Errorf("created CHD lacks the expected v5 single-session CD track metadata")
	}
	return layout.verify(ctx, files)
}

func (layout cdManifest) checkTrackData(ctx context.Context, files []chdCueFile) error {
	digest := sha1.New()
	remaining := layout.image.size
	for _, source := range files {
		if err := hashCDTracks(ctx, digest, source, &remaining, !layout.littleEndianAudio); err != nil {
			return err
		}
	}
	var actual [sha1.Size]byte
	digest.Sum(actual[:0])
	if remaining != 0 || actual != layout.image.digest {
		return fmt.Errorf("disc track bytes do not match the CD CHD raw SHA-1")
	}
	return ctx.Err()
}

func hashCDTracks(ctx context.Context, digest hash.Hash, source chdCueFile, remaining *int64, swapAudio bool) error {
	file, _, err := fileops.OpenRegular(source.path)
	if err != nil {
		return err
	}
	defer file.Close()
	var sectors io.Reader = file
	if source.fileType == "WAVE" {
		sectors = io.NewSectionReader(file, source.dataOffset, source.dataSize)
	}
	reader := bufio.NewReaderSize(sectors, 128<<10)
	var padding [chdCDFrameBytes]byte
	for _, track := range source.tracks {
		padded := (track.frames + 3) &^ 3
		if err := chargeCDTrack(remaining, track.frames); err != nil {
			return fmt.Errorf("CUE tracks exceed the CD CHD's logical size")
		}
		var frame [chdCDFrameBytes]byte
		stored := track.frames - track.padFrames
		for range stored {
			if err := ctx.Err(); err != nil {
				return err
			}
			// chdman stores data and any subcode contiguously, then zero-fills
			// the rest of the frame. Only the audio samples are byte-swapped.
			if _, err := io.ReadFull(reader, frame[:track.sectorBytes]); err != nil {
				return err
			}
			if track.mode == "AUDIO" && swapAudio {
				for i := 0; i < 2352; i += 2 {
					frame[i], frame[i+1] = frame[i+1], frame[i]
				}
			}
			digest.Write(frame[:])
		}
		for i := stored; i < padded; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			digest.Write(padding[:])
		}
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		return fmt.Errorf("CUE file has unexpected data after its last track")
	}
	return ctx.Err()
}
