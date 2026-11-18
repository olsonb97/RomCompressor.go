package chd

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

type chdCueTrack struct {
	padFrames   int64 // GD-ROM zero frames between this track and the next LBA.
	sectorBytes int64
	mode        string
	subtype     string
	index0      int64
	index1      int64
	frames      int64
	pregap      int64
	postgap     int64
	hasPregap   bool
}

type chdCueFile struct {
	path       string
	size       int64
	dataOffset int64
	dataSize   int64
	fileType   string
	tracks     []chdCueTrack
}

const chdCueWhitespace = " \t\n\v\f\r"

// ValidateCDSource checks the layout chdman will select from the input extension.
func ValidateCDSource(ctx context.Context, path string, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cue":
		return ValidateCUE(path, "", limit)
	case ".gdi":
		return ValidateGDI(path, limit)
	case ".nrg":
		return ValidateNRG(ctx, path, limit)
	case ".iso", ".cdr", ".toast":
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		// chdman's parse_iso tries sector sizes in this order. A CHD track
		// occupies whole groups of four frames, including subchannel bytes.
		for _, sector := range []int64{2048, 2336, 2352} {
			if info.Mode().IsRegular() && info.Size() > 0 && info.Size()%sector == 0 {
				return chargeCDTrack(&limit, info.Size()/sector)
			}
		}
		return fmt.Errorf("CD image requires complete 2048-, 2336-, or 2352-byte sectors")
	default:
		// Unknown extensions would be interpreted as cdrdao descriptors.
		return fmt.Errorf("CD CHD input must be a CUE/GDI descriptor or an ISO, CDR, Toast, or NRG image")
	}
}

// Each track occupies whole groups of four 2448-byte frames in the CHD.
func chargeCDTrack(remaining *int64, frames int64) error {
	const groupBytes = 4 * chdCDFrameBytes
	groups := (frames + 3) / 4
	if groups > *remaining/groupBytes {
		return fmt.Errorf("CD CHD exceeds the expanded size limit")
	}
	*remaining -= groups * groupBytes
	return nil
}

// ValidateCUE checks the boundaries chdman derives from BIN lengths and CUE indexes.
// A partial final sector is otherwise discarded by createcd.
func ValidateCUE(path, expectedBin string, limit int64) error {
	files, err := readCHDCUE(path, expectedBin, limit)
	if err != nil {
		return err
	}
	for _, file := range files {
		for _, track := range file.tracks {
			if err := chargeCDTrack(&limit, track.frames); err != nil {
				return err
			}
		}
	}
	return nil
}

func readCHDCUE(path, expectedBin string, limit int64) ([]chdCueFile, error) {
	var files []chdCueFile
	if err := disc.VisitReferences(path, func(path string, info os.FileInfo) error {
		if expectedBin != "" && (path != expectedBin || len(files) != 0) {
			return fmt.Errorf("extracted CUE must reference only its staged track")
		}
		files = append(files, chdCueFile{path: path, size: info.Size()})
		return nil
	}); err != nil {
		return nil, err
	}
	file, _, err := fileops.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, disc.MaxDescriptorSize+1))
	scanner.Buffer(make([]byte, 510), 510)
	var terminated bool
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, line, err := bufio.ScanLines(data, atEOF)
		terminated = advance > 0 && data[advance-1] == '\n'
		return advance, line, err
	})
	fileIndex, trackIndex := -1, 0
	var singleDensity, highDensity bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.ContainsRune(line, 0) {
			return nil, fmt.Errorf("CUE contains a NUL byte")
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// chdman strips quotes from the directive token itself. Detect a
		// disguised layout directive before it can read an unsnapshotted file.
		token := strings.ReplaceAll(strings.ReplaceAll(fields[0], "'", ""), "\"", "")
		directive := strings.ToUpper(token)
		switch directive {
		case "FILE", "TRACK", "PREGAP", "POSTGAP", "INDEX":
			// Older chdman parsers match uppercase tokens and skip a final
			// line left unterminated at EOF.
			if fields[0] != directive || !terminated {
				return nil, fmt.Errorf("CUE %s directive must be uppercase, unquoted, and end with a newline", directive)
			}
			if !chdCueWhitespaceMatches(line, fields) {
				return nil, fmt.Errorf("CUE %s directive requires ASCII whitespace", directive)
			}
		}
		switch directive {
		case "REM":
			comment := strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(line, chdCueWhitespace), fields[0]), chdCueWhitespace)
			// chdman treats these REM prefixes as layout commands. A second
			// session can shorten tracks or skip stored sectors in a shared BIN.
			switch {
			case strings.HasPrefix(comment, "SESSION"):
				session := strings.Fields(comment)
				if len(session) != 2 || session[0] != "SESSION" || strings.TrimLeft(session[1], "0") != "1" {
					return nil, fmt.Errorf("CHD conversion supports only single-session CUE sheets")
				}
			case strings.HasPrefix(comment, "LEAD-IN"), strings.HasPrefix(comment, "LEAD-OUT"):
				return nil, fmt.Errorf("CHD cannot preserve CUE session lead-in or lead-out boundaries")
			}
			if expectedBin == "" && token == "REM" {
				// Older chdman versions strip stored pregaps when both density
				// markers select the Dreamcast parser. Newer ones accept indentation.
				singleDensity = singleDensity || strings.HasPrefix(comment, "SINGLE-DENSITY AREA")
				highDensity = highDensity || strings.HasPrefix(comment, "HIGH-DENSITY AREA")
				if singleDensity && highDensity {
					return nil, fmt.Errorf("chdman can discard stored pregaps from Dreamcast multi-CUE tracks")
				}
			}
		case "FILE":
			fileIndex++
			match := disc.MatchCUEFile(strings.TrimSpace(line))
			if fileIndex >= len(files) || match == nil {
				return nil, fmt.Errorf("invalid CUE file directive")
			}
			if err := checkDescriptorPaths(path, match[1]+match[2]); err != nil {
				return nil, err
			}
			// chdman strips both kinds of quotes from bare tokens, while
			// reference validation treats them as literal filename bytes.
			if strings.ContainsAny(match[2], "\"'") {
				return nil, fmt.Errorf("ambiguous CUE track filename for chdman; use double quotes")
			}
			kind := fields[len(fields)-1]
			switch kind {
			case "BINARY", "WAVE":
			case "MOTOROLA":
				return nil, fmt.Errorf("chdman does not preserve MOTOROLA CUE track byte order")
			default:
				return nil, fmt.Errorf("unsupported CUE file type %q; use BINARY or WAVE without quotes", kind)
			}
			files[fileIndex].fileType = kind
			if expectedBin != "" && kind != "BINARY" {
				return nil, fmt.Errorf("extracted CUE must describe a binary track")
			}
		case "TRACK":
			if fileIndex < 0 || len(fields) < 3 || len(fields) > 4 {
				return nil, fmt.Errorf("invalid CUE track directive")
			}
			// chdman strips quotes and only recognizes uppercase subcode
			// tokens. A differing subtype changes the number of bytes per sector.
			if len(fields) == 4 && fields[3] != "NONE" && fields[3] != "RW" && fields[3] != "RW_RAW" {
				return nil, fmt.Errorf("CUE track subcode must be NONE, RW, or RW_RAW without quotes")
			}
			trackIndex++
			number, err := strconv.Atoi(fields[1])
			if err != nil || number != trackIndex || trackIndex > 99 {
				return nil, fmt.Errorf("unexpected CUE track number")
			}
			sector, err := chdCueSectorSize(fields[2:])
			if err != nil {
				return nil, err
			}
			f := &files[fileIndex]
			subtype := "NONE"
			if len(fields) == 4 {
				subtype = fields[3]
			}
			f.tracks = append(f.tracks, chdCueTrack{sectorBytes: sector, mode: fields[2], subtype: subtype, index0: -1, index1: -1})
		case "PREGAP", "POSTGAP", "INDEX":
			if fileIndex < 0 || len(files[fileIndex].tracks) == 0 {
				return nil, fmt.Errorf("CUE index or gap appears before a track")
			}
			indexes := files[fileIndex].tracks
			track := &indexes[len(indexes)-1]
			if err := track.readPosition(fields); err != nil {
				return nil, err
			}
		case "FLAGS":
			return nil, fmt.Errorf("CHD does not retain CUE track flags")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if fileIndex+1 != len(files) {
		return nil, fmt.Errorf("CUE references changed during validation")
	}
	var bytes int64
	for i := range files {
		file := &files[i]
		// chdman calculates shared-track offsets across adjacent tracks, even
		// when they refer to different files at a boundary.
		if len(files) > 1 && file.fileType == "BINARY" && len(file.tracks) > 1 {
			return nil, fmt.Errorf("chdman cannot preserve a shared BIN in a multi-file CUE")
		}
		if file.size > limit-bytes {
			return nil, fmt.Errorf("CUE tracks exceed the expanded size limit")
		}
		bytes += file.size
		if err := file.checkTracks(); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func chdCueWhitespaceMatches(line string, fields []string) bool {
	// chdman's tokenizer accepts C whitespace. Compare the separators
	// without treating whitespace inside the FILE path as syntax.
	ascii := strings.FieldsFunc(line, func(r rune) bool { return strings.ContainsRune(chdCueWhitespace, r) })
	if fields[0] == "FILE" {
		return ascii[0] == fields[0] && ascii[len(ascii)-1] == fields[len(fields)-1]
	}
	return slices.Equal(ascii, fields)
}

// chdman splits the descriptor's own path at its last backslash first, but
// on POSIX it passes track filenames to open without translating backslashes.
func checkDescriptorPaths(descriptor, track string) error {
	if filepath.Separator == '/' && (strings.ContainsRune(descriptor, '\\') || strings.ContainsRune(track, '\\')) {
		return fmt.Errorf("chdman requires POSIX descriptor and track paths without backslashes; use / for directories")
	}
	return nil
}

func (track *chdCueTrack) readPosition(fields []string) error {
	if strings.EqualFold(fields[0], "INDEX") {
		if len(fields) != 3 {
			return fmt.Errorf("invalid CUE index directive")
		}
		index, err := strconv.Atoi(fields[1])
		if err != nil || index < 0 || index > 99 {
			return fmt.Errorf("invalid CUE index number")
		}
		position, err := chdCuePosition(fields[2])
		if err != nil {
			return err
		}
		switch index {
		case 0:
			if track.index0 >= 0 || track.index1 >= 0 {
				return fmt.Errorf("unexpected CUE index 00")
			}
			track.index0 = position
		case 1:
			if track.index1 >= 0 || track.index0 > position {
				return fmt.Errorf("unexpected CUE index 01")
			}
			track.index1 = position
			if track.index0 == -1 {
				track.index0 = position
			}
		default:
			return fmt.Errorf("CHD does not retain CUE index %02d", index)
		}
		return nil
	}
	if len(fields) != 2 {
		return fmt.Errorf("invalid CUE gap directive")
	}
	position, err := chdCuePosition(fields[1])
	if err != nil {
		return err
	}
	if strings.EqualFold(fields[0], "PREGAP") {
		track.pregap = position
		track.hasPregap = true
	} else {
		track.postgap = position
	}
	return nil
}

// chdman uses the first data chunk after fmt and rounds down partial sectors.
// Require one unambiguous PCM extent before deriving the CUE track frames.
func readCHDWAVEExtent(path string) (offset, length int64, err error) {
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	var riff [12]byte
	if _, err := io.ReadFull(file, riff[:]); err != nil || string(riff[:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return 0, 0, fmt.Errorf("invalid CUE WAVE header")
	}
	end := int64(binary.LittleEndian.Uint32(riff[4:8])) + 8
	if end > info.Size() {
		return 0, 0, fmt.Errorf("truncated CUE WAVE file")
	}
	var chunk [8]byte
	formatSeen := false
	for pos, count := int64(len(riff)), 0; pos < end; count++ {
		if end-pos < int64(len(chunk)) || count >= 1024 {
			return 0, 0, fmt.Errorf("invalid or oversized CUE WAVE chunk layout")
		}
		if _, err := file.ReadAt(chunk[:], pos); err != nil {
			return 0, 0, err
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		pos += int64(len(chunk))
		padded := size + size%2
		if padded > end-pos {
			return 0, 0, fmt.Errorf("truncated CUE WAVE chunk")
		}
		// chdman advances through chunks before the audio without RIFF padding.
		kind := string(chunk[:4])
		if length == 0 && padded != size && kind != "data" {
			return 0, 0, fmt.Errorf("chdman cannot reliably read an odd-sized CUE WAVE chunk before audio")
		}
		switch kind {
		case "fmt ":
			if formatSeen || size < 16 {
				return 0, 0, fmt.Errorf("invalid or duplicate CUE WAVE format chunk")
			}
			var pcm [16]byte
			if _, err := file.ReadAt(pcm[:], pos); err != nil {
				return 0, 0, err
			}
			le := binary.LittleEndian
			if le.Uint16(pcm[:2]) != 1 || le.Uint16(pcm[2:4]) != 2 || le.Uint32(pcm[4:8]) != 44100 || le.Uint16(pcm[14:]) != 16 {
				return 0, 0, fmt.Errorf("CUE WAVE requires 44.1 kHz, 16-bit stereo PCM")
			}
			formatSeen = true
		case "data":
			if !formatSeen || length != 0 {
				return 0, 0, fmt.Errorf("CUE WAVE requires one audio data chunk after the format chunk")
			}
			if size == 0 || size%2352 != 0 {
				return 0, 0, fmt.Errorf("CUE WAVE audio must contain complete CD sectors")
			}
			length = size
			offset = pos
		}
		pos += padded
	}
	if length == 0 {
		return 0, 0, fmt.Errorf("CUE WAVE lacks a supported audio data chunk")
	}
	return offset, length, nil
}

func (file *chdCueFile) checkTracks() error {
	if len(file.tracks) == 0 {
		return fmt.Errorf("CUE file has no tracks")
	}
	remaining := file.size
	if file.fileType == "WAVE" {
		if len(file.tracks) != 1 {
			return fmt.Errorf("chdman does not preserve multiple CUE tracks in one WAVE file")
		}
		if file.tracks[0].mode != "AUDIO" || file.tracks[0].sectorBytes != 2352 {
			return fmt.Errorf("CUE WAVE requires an AUDIO track without subcode")
		}
		var err error
		file.dataOffset, remaining, err = readCHDWAVEExtent(file.path)
		if err != nil {
			return err
		}
	}
	file.dataSize = remaining
	for i := range file.tracks {
		track := &file.tracks[i]
		if track.index1 < 0 || i == 0 && track.index0 != 0 {
			return fmt.Errorf("CUE track has a missing index or unassigned leading sectors")
		}
		// Even a zero-length PREGAP can replace the stored length in chdman.
		if track.hasPregap && track.index1 > track.index0 {
			return fmt.Errorf("CHD cannot retain both a virtual and stored pregap on the same track")
		}
		frames := remaining / track.sectorBytes
		if i+1 < len(file.tracks) {
			frames = file.tracks[i+1].index0 - track.index0
		} else if remaining%track.sectorBytes != 0 {
			return fmt.Errorf("CUE binary track has a partial final sector")
		}
		if frames <= track.index1-track.index0 || frames > remaining/track.sectorBytes {
			return fmt.Errorf("CUE track indexes exceed its data or overlap")
		}
		track.frames = frames
		remaining -= frames * track.sectorBytes
	}
	return nil
}

func chdCueSectorSize(fields []string) (int64, error) {
	var sector int64
	switch canonicalCDMode(fields[0]) {
	case "MODE1", "MODE2_FORM1":
		sector = 2048
	case "MODE2_FORM2":
		sector = 2324
	case "MODE2", "MODE2_FORM_MIX":
		sector = 2336
	case "AUDIO", "MODE1_RAW", "MODE2_RAW":
		sector = 2352
	default:
		return 0, fmt.Errorf("unsupported CUE track mode %q", fields[0])
	}
	if len(fields) > 1 && (strings.EqualFold(fields[1], "RW") || strings.EqualFold(fields[1], "RW_RAW")) {
		sector += 96
	}
	return sector, nil
}

func canonicalCDMode(mode string) string {
	switch mode {
	case "MODE1/2048":
		return "MODE1"
	case "MODE1/2352":
		return "MODE1_RAW"
	case "MODE2/2048":
		return "MODE2_FORM1"
	case "MODE2/2324":
		return "MODE2_FORM2"
	case "MODE2/2336":
		return "MODE2"
	case "MODE2/2352", "CDI/2352":
		return "MODE2_RAW"
	default:
		return mode
	}
}

func chdCuePosition(value string) (int64, error) {
	parts := strings.Split(value, ":")
	if len(parts) == 3 {
		minutes, a := strconv.ParseUint(parts[0], 10, 20)
		seconds, b := strconv.ParseUint(parts[1], 10, 6)
		frames, c := strconv.ParseUint(parts[2], 10, 7)
		if a == nil && b == nil && c == nil && seconds < 60 && frames < 75 {
			return int64((minutes*60+seconds)*75 + frames), nil
		}
	}
	return 0, fmt.Errorf("invalid CUE timestamp %q", value)
}

func verifyCHDCDExtraction(log string) error {
	if strings.Contains(strings.ToLower(log), "subcode data and it will be omitted") {
		return fmt.Errorf("CUE/BIN cannot retain the CHD's subcode data")
	}
	for _, line := range strings.FieldsFunc(log, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.TrimSpace(line) == "Extraction complete" {
			return nil
		}
	}
	return fmt.Errorf("chdman did not confirm CD extraction")
}

func readExtractedCHDCUE(cue, bin string, limit int64) (chdCueFile, error) {
	info, err := os.Lstat(bin)
	if err != nil {
		return chdCueFile{}, fmt.Errorf("extracted disc track is missing: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return chdCueFile{}, fmt.Errorf("extracted disc track is not a nonempty regular file")
	}
	files, err := readCHDCUE(cue, bin, limit)
	if err != nil {
		return chdCueFile{}, err
	}
	if len(files) != 1 {
		return chdCueFile{}, fmt.Errorf("extracted CUE must reference its staged track")
	}
	return files[0], nil
}
