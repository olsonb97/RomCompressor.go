package chd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

func extractCDCHD(ctx context.Context, tool, source, output string, limit int64) error {
	ext := filepath.Ext(output)
	layout, err := cdExtractionLayout(ctx, source, limit)
	if err != nil {
		return err
	}
	if layout.gdrom && !strings.EqualFold(ext, ".gdi") {
		return fmt.Errorf("GD-ROM extraction requires a .gdi output filename")
	}
	if !layout.gdrom && !strings.EqualFold(ext, ".cue") {
		return fmt.Errorf("CD extraction requires a .cue output filename")
	}
	if err := Verify(ctx, tool, source, limit, "cd"); err != nil {
		return err
	}
	if layout.gdrom {
		return extractGDROM(ctx, tool, source, output, layout)
	}
	bin := strings.TrimSuffix(output, ext) + ".bin"
	fileLine, err := disc.BinaryCUEFile(filepath.Base(bin))
	if err != nil {
		return err
	}
	stage, err := fileops.NewStage(filepath.Dir(output), ".chd-cue-")
	if err != nil {
		return err
	}
	defer stage.Cleanup()
	if err := runCDExtraction(ctx, tool, source, stage, "track.cue"); err != nil {
		return err
	}
	if err := fileops.Publish(filepath.Join(stage.Path(), "track.bin"), bin); err != nil {
		return err
	}
	if err := writeRestoredCUE(ctx, filepath.Join(stage.Path(), "track.cue"), output, fileLine, layout.tracks); err != nil {
		return err
	}
	return layout.verifyRestored(ctx, output, bin, limit)
}

// Stable relative names keep user paths out of chdman's template syntax and
// older versions' case-sensitive detection of the descriptor extension.
func runCDExtraction(ctx context.Context, tool, source string, stage fileops.Stage, descriptor string) error {
	tool, err := filepath.Abs(tool)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	log, err := process.Execute(ctx, nil, nil, stage.Path(), tool, "extractcd", "-i", source, "-o", descriptor)
	if err != nil {
		return err
	}
	if err := verifyCHDCDExtraction(log); err != nil {
		return err
	}
	return stage.Check()
}

func writeRestoredCUE(ctx context.Context, source, output, fileLine string, tracks []chdCueTrack) error {
	file, _, err := fileops.OpenRegular(source)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, disc.MaxDescriptorSize+1))
	if err != nil {
		return err
	}
	if len(data) > disc.MaxDescriptorSize {
		return fmt.Errorf("extracted CUE exceeds the descriptor size limit")
	}
	var cue strings.Builder
	for line := range strings.SplitAfterSeq(string(data), "\n") {
		if err := ctx.Err(); err != nil {
			return err
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "FILE" {
			if len(fields) != 3 || (fields[1] != "track.bin" && fields[1] != `"track.bin"`) || fields[2] != "BINARY" {
				return fmt.Errorf("extracted CUE must reference only its staged track")
			}
			cue.WriteString(fileLine)
			continue
		}
		// chdman writes MODE2/2336 for both plain and mixed-form sectors.
		if len(fields) == 3 && fields[0] == "TRACK" && fields[2] == "MODE2/2336" {
			n, err := strconv.Atoi(fields[1])
			if err == nil && n > 0 && n <= len(tracks) && tracks[n-1].mode == "MODE2_FORM_MIX" {
				line = strings.Replace(line, "MODE2/2336", "MODE2_FORM_MIX", 1)
			}
		}
		cue.WriteString(line)
	}
	return fileops.WriteText(output, cue.String())
}

func (layout cdManifest) verifyRestored(ctx context.Context, cue, bin string, limit int64) error {
	restored, err := readExtractedCHDCUE(cue, bin, limit)
	if err != nil {
		return err
	}
	return layout.verify(ctx, []chdCueFile{restored})
}
