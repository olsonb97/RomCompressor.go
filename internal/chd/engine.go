// Package chd validates disc layouts and runs explicit local chdman conversions.
package chd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/process"
)

// Convert creates a CHD or restores its raw image into private caller-owned staging.
// The caller owns source snapshots, staging limits, publication, and cleanup.
func Convert(ctx context.Context, tool, source, output string, extract bool, media string, limit int64) error {
	var err error
	if extract {
		if media != "dvd" && media != "cd" && media != "hdd" {
			return fmt.Errorf("CHD extraction requires raw output and media cd, dvd, or hdd")
		}
		if media == "dvd" {
			return extractDVDCHD(ctx, tool, source, output, limit)
		}
		if media == "hdd" {
			return extractHDDCHD(ctx, tool, source, output, limit)
		}
		return extractCDCHD(ctx, tool, source, output, limit)
	}
	ext := strings.ToLower(filepath.Ext(source))
	if media == "" && (ext == ".cue" || ext == ".gdi") {
		media = "cd"
	}
	switch media {
	case "cd":
		if err := ValidateCDSource(ctx, source, limit); err != nil {
			return err
		}
	case "dvd":
		if ext != ".iso" {
			return fmt.Errorf("DVD CHD creation requires an ISO file")
		}
		return createDVDCHD(ctx, tool, source, output, limit)
	case "hdd":
		if !IsHardDiskExtension(ext) {
			return fmt.Errorf("hard-drive CHD creation requires a raw .img, .raw, .hdd, or .bin image")
		}
		return createHDDCHD(ctx, tool, source, output, limit)
	default:
		return fmt.Errorf("choose media cd, dvd, or hdd for this image")
	}
	if _, err = process.Run(ctx, tool, "createcd", "-i", source, "-o", output, "-np", "2"); err != nil {
		return err
	}
	if err := Verify(ctx, tool, output, limit, "cd"); err != nil {
		return err
	}
	if ext == ".cue" {
		return verifyCreatedCUE(ctx, source, output, limit)
	}
	return nil
}

// IsHardDiskExtension reports supported raw hard-drive image extensions.
func IsHardDiskExtension(ext string) bool {
	switch ext {
	case ".img", ".raw", ".hdd", ".bin":
		return true
	}
	return false
}

// RestoredSize checks the extraction layout and reports known raw file bytes.
// CD sizes exclude the generated CUE. Legacy CD layouts return -1.
func RestoredSize(ctx context.Context, path string, limit int64, media string) (int64, error) {
	if media == "cd" {
		layout, err := cdExtractionLayout(ctx, path, limit)
		if err != nil || len(layout.tracks) == 0 {
			return -1, err
		}
		var size int64
		for _, track := range layout.tracks {
			size += (track.frames - track.padFrames) * track.sectorBytes
		}
		if size > limit {
			return -1, fmt.Errorf("restored CD tracks exceed the output limit")
		}
		return size, nil
	}
	if media != "dvd" && media != "hdd" {
		return -1, fmt.Errorf("CHD extraction requires media cd, dvd, or hdd")
	}
	if _, err := MetadataFile(ctx, path, limit, media); err != nil {
		return -1, err
	}
	var image rawCHD
	var err error
	if media == "dvd" {
		image, err = readDVDCHD(path)
	} else {
		image, err = readHDDCHD(ctx, path, limit)
	}
	return image.size, err
}

// Verify checks metadata bounds and requires explicit engine checksum confirmation.
func Verify(ctx context.Context, tool, path string, limit int64, media string) error {
	version, err := MetadataFile(ctx, path, limit, media)
	if err != nil {
		return err
	}
	log, err := process.Run(ctx, tool, "verify", "-i", path)
	if err != nil {
		return err
	}
	return checkCHDVerification(version, log)
}

func checkCHDVerification(version uint32, log string) error {
	// chdman can exit successfully when verification is skipped or a digest mismatches.
	var raw, overall bool
	for remaining := log; remaining != ""; {
		line := remaining
		if end := strings.IndexAny(remaining, "\r\n"); end >= 0 {
			line, remaining = remaining[:end], remaining[end+1:]
		} else {
			remaining = ""
		}
		switch strings.TrimSpace(line) {
		case "Raw SHA1 verification successful!":
			raw = true
		case "Overall SHA1 verification successful!":
			overall = true
		}
	}
	if !raw || (version >= 4 && !overall) {
		return fmt.Errorf("chdman did not confirm the required CHD checksums: %s", process.FailureDiagnostics(log))
	}
	return nil
}
