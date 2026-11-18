package chd

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMaxOutput int64 = 1 << 40

func TestMetadataFileHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MetadataFile(ctx, filepath.Join(t.TempDir(), "absent.chd"), testMaxOutput, ""); err != context.Canceled {
		t.Fatalf("metadata lookup after cancellation: %v", err)
	}
}

func TestVerifyCHDRejectsCyclicMetadataBeforeLaunch(t *testing.T) {
	for _, tc := range []struct {
		name            string
		version         uint32
		headerSize      int
		logicalAt       int
		metaOffsetField int
		hunkAt          int
	}{
		{"v3", 3, 120, 28, 36, 76},
		{"v4", 4, 108, 28, 36, 44},
		{"v5", 5, 124, 32, 48, 56},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.chd")
			const entrySize = 17
			data := make([]byte, tc.headerSize+2*entrySize)
			copy(data, "MComprHD")
			binary.BigEndian.PutUint32(data[8:12], uint32(tc.headerSize))
			binary.BigEndian.PutUint32(data[12:16], tc.version)
			binary.BigEndian.PutUint64(data[tc.logicalAt:], 2048)
			binary.BigEndian.PutUint32(data[tc.hunkAt:], 2048)
			if tc.version < 5 {
				binary.BigEndian.PutUint32(data[24:28], 1)
			} else {
				binary.BigEndian.PutUint32(data[60:64], 2048)
			}
			binary.BigEndian.PutUint64(data[tc.metaOffsetField:], uint64(tc.headerSize))
			first, second := data[tc.headerSize:], data[tc.headerSize+entrySize:]
			first[7], second[7] = 1, 1
			binary.BigEndian.PutUint64(first[8:16], uint64(tc.headerSize+entrySize))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if version, err := MetadataFile(context.Background(), path, testMaxOutput, ""); err != nil || version != tc.version {
				t.Fatalf("valid metadata chain returned version %d, %v", version, err)
			}
			if err := Verify(context.Background(), "no-engine", path, 2047, ""); err == nil || !strings.Contains(err.Error(), "expanded size limit") {
				t.Fatalf("expected size limit rejection before engine launch, got %v", err)
			}
			first[7] = 2
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Verify(context.Background(), "no-engine", path, testMaxOutput, ""); err == nil || !strings.Contains(err.Error(), "overlapping CHD metadata records") {
				t.Fatalf("expected overlapping metadata rejection before engine launch, got %v", err)
			}
			first[7] = 1
			binary.BigEndian.PutUint64(second[8:16], uint64(tc.headerSize))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Verify(context.Background(), "no-engine", path, testMaxOutput, ""); err == nil || !strings.Contains(err.Error(), "invalid CHD metadata chain") {
				t.Fatalf("expected cyclic metadata rejection before engine launch, got %v", err)
			}
		})
	}
}

func TestCHDMediaSelection(t *testing.T) {
	for _, test := range []struct {
		name  string
		tag   string
		media string
		want  string
	}{
		{"existing untagged DVD raw image", "", "dvd", ""},
		{"tagged DVD", "DVD ", "dvd", ""},
		{"hard-drive geometry with 2048-byte sectors", "GDDD", "hdd", ""},
		{"hard drive selected as DVD", "GDDD", "dvd", "conflicts with requested dvd"},
		{"hard drive selected as CD", "GDDD", "cd", "conflicts with requested cd"},
		{"untagged CD request", "", "cd", "lacks cd media"},
		{"untyped verification", "GDDD", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disc.chd")
			data := make([]byte, chdV5HeaderSize+16)
			copy(data, "MComprHD")
			binary.BigEndian.PutUint32(data[8:12], chdV5HeaderSize)
			binary.BigEndian.PutUint32(data[12:16], 5)
			binary.BigEndian.PutUint64(data[32:40], 2048)
			binary.BigEndian.PutUint32(data[56:60], 2048)
			binary.BigEndian.PutUint32(data[60:64], 2048)
			if test.tag != "" {
				binary.BigEndian.PutUint64(data[48:56], chdV5HeaderSize)
				copy(data[chdV5HeaderSize:], test.tag)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := MetadataFile(context.Background(), path, testMaxOutput, test.media)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("media validation = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCHDRestorationChecksTrackModeEvenWhenSectorSizesMatch(t *testing.T) {
	track := chdCueTrack{mode: "MODE2/2048", subtype: "NONE", sectorBytes: 2048, frames: 4}
	for _, mode := range []string{"MODE1", "MODE2_FORM1"} {
		want, err := parseCDTrack("CHTR", "TRACK:1 TYPE:"+mode+" SUBTYPE:NONE FRAMES:4", 1)
		if err != nil {
			t.Fatal(err)
		}
		layout := cdManifest{tracks: []chdCueTrack{want}}
		err = layout.verify(context.Background(), []chdCueFile{{tracks: []chdCueTrack{track}}})
		if (err == nil) != (mode == "MODE2_FORM1") {
			t.Fatalf("mode %s: %v", mode, err)
		}
	}
}

func TestDVDCHDRejectsOversizedSourceBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "large.iso")
	if err := os.WriteFile(source, make([]byte, 2*dvdSectorSize), 0600); err != nil {
		t.Fatal(err)
	}
	if err := createDVDCHD(context.Background(), "no-engine", source, filepath.Join(dir, "test.chd"), dvdSectorSize); err == nil || !strings.Contains(err.Error(), "expanded size limit") {
		t.Fatalf("expected size limit rejection before engine launch, got %v", err)
	}
}

func TestCHDCUETrackGeometry(t *testing.T) {
	dir := t.TempDir()
	cue := filepath.Join(dir, "game.cue")
	bin := filepath.Join(dir, "game.bin")
	for _, test := range []struct {
		name  string
		cue   string
		size  int
		error string
	}{
		{"mixed sectors", "TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n TRACK 02 AUDIO\n INDEX 01 00:00:03\n", 3*2048 + 5*2352, ""},
		{"partial final sector", "TRACK 01 AUDIO\n INDEX 01 00:00:00\n", 8*2352 + 1, "partial final sector"},
		{"unsupported CD index", "TRACK 01 AUDIO\n INDEX 01 00:00:00\n INDEX 02 00:00:04\n", 8 * 2352, "does not retain CUE index 02"},
		{"pre-emphasis", "TRACK 01 AUDIO\n FLAGS PRE\n INDEX 01 00:00:00\n", 8 * 2352, "does not retain CUE track flags"},
		{"index outside BIN", "TRACK 01 AUDIO\n INDEX 00 00:00:00\n INDEX 01 00:00:10\n", 8 * 2352, "indexes exceed"},
		{"combined pregaps", "TRACK 01 AUDIO\n PREGAP 00:00:02\n INDEX 00 00:00:00\n INDEX 01 00:00:01\n", 8 * 2352, "virtual and stored pregap"},
		{"zero pregap replacing a stored pregap", "TRACK 01 AUDIO\n INDEX 00 00:00:00\n INDEX 01 00:00:02\n PREGAP 00:00:00\n", 8 * 2352, "virtual and stored pregap"},
		{"stored pregap", "TRACK 01 AUDIO\n INDEX 00 00:00:00\n INDEX 01 00:00:02\n", 8 * 2352, ""},
		{"single session label", "REM SESSION 01\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n", 8 * 2352, ""},
		{"second session", "REM SESSION 02\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n", 8 * 2352, "single-session"},
		{"session lead-in", "REM LEAD-IN 00:01:00\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n", 8 * 2352, "session lead-in"},
		{"lowercase track mode", "TRACK 01 audio\n INDEX 01 00:00:00\n", 8 * 2352, "unsupported CUE track mode"},
		{"CDG shorthand", "TRACK 01 CDG\n INDEX 01 00:00:00\n", 8 * 2448, "unsupported CUE track mode"},
		{"virtual pregap", "TRACK 01 AUDIO\n PREGAP 00:00:02\n INDEX 01 00:00:00\n", 8 * 2352, ""},
		{"unterminated comment", "TRACK 01 AUDIO\n INDEX 01 00:00:00\n REM comment", 8 * 2352, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(cue, []byte("FILE \"game.bin\" BINARY\n"+test.cue), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, make([]byte, test.size), 0600); err != nil {
				t.Fatal(err)
			}
			_, extractErr := readExtractedCHDCUE(cue, bin, testMaxOutput)
			for _, err := range []error{ValidateCUE(cue, "", testMaxOutput), extractErr} {
				if test.error == "" && err != nil || test.error != "" && (err == nil || !strings.Contains(err.Error(), test.error)) {
					t.Fatalf("validation = %v, want %q", err, test.error)
				}
			}
		})
	}
}

func TestCHDExtractionRefusesSubcodeOmission(t *testing.T) {
	log := "Warning: Track 1 has subcode data. bin/cue and gdi formats cannot contain subcode data and it will be omitted.\nExtraction complete\n"
	if err := verifyCHDCDExtraction(log); err == nil || !strings.Contains(err.Error(), "subcode data") {
		t.Fatalf("expected subcode omission to fail, got %v", err)
	}
	if err := verifyCHDCDExtraction("Extraction complete\n"); err != nil {
		t.Fatal(err)
	}
}

func TestCHDGDITrackGeometry(t *testing.T) {
	dir := t.TempDir()
	gdi := filepath.Join(dir, "disc.gdi")
	for _, test := range []struct {
		name   string
		layout string
		size   int
		want   string
	}{
		{"valid", "2\n1 0 4 2048 data.bin 0\n2 8 0 2352 audio.bin 0\n", 8 * 2352, ""},
		{"partial", "2\n1 0 4 2048 data.bin 0\n2 8 0 2352 audio.bin 0\n", 8*2352 + 1, "complete sectors"},
		{"ignored offset", "2\n1 0 4 2048 data.bin 0\n2 8 0 2352 audio.bin 2352\n", 8 * 2352, "nonzero file offset"},
		{"overlap", "2\n1 0 4 2048 data.bin 0\n2 7 0 2352 audio.bin 0\n", 8 * 2352, "overlap"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, data := range map[string][]byte{"disc.gdi": []byte(test.layout), "data.bin": make([]byte, 8*2048), "audio.bin": make([]byte, test.size)} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateGDI(gdi, testMaxOutput); (test.want == "" && err != nil) || (test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want))) {
				t.Fatalf("validation = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCHDCUEWaveRequiresWholeAudioSectors(t *testing.T) {
	dir := t.TempDir()
	cue, wav := filepath.Join(dir, "audio.cue"), filepath.Join(dir, "audio.wav")
	if err := os.WriteFile(cue, []byte("FILE \"audio.wav\" WAVE\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, samples := range []int{8 * 588, 8*588 + 1} {
		data := make([]byte, 44+samples*4)
		copy(data, "RIFF")
		binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
		copy(data[8:], "WAVEfmt ")
		binary.LittleEndian.PutUint32(data[16:20], 16)
		binary.LittleEndian.PutUint16(data[20:22], 1) // PCM
		binary.LittleEndian.PutUint16(data[22:24], 2)
		binary.LittleEndian.PutUint32(data[24:28], 44100)
		binary.LittleEndian.PutUint32(data[28:32], 176400)
		binary.LittleEndian.PutUint16(data[32:34], 4)
		binary.LittleEndian.PutUint16(data[34:36], 16)
		copy(data[36:], "data")
		binary.LittleEndian.PutUint32(data[40:44], uint32(samples*4))
		if err := os.WriteFile(wav, data, 0600); err != nil {
			t.Fatal(err)
		}
		err := ValidateCUE(cue, "", testMaxOutput)
		if samples%588 == 0 && err != nil || samples%588 != 0 && (err == nil || !strings.Contains(err.Error(), "complete CD sectors")) {
			t.Fatalf("expected aligned wave success and partial audio refusal, got %d samples: %v", samples, err)
		}
		if samples%588 == 0 {
			for _, mode := range []string{"MODE1/2048", "AUDIO RW"} {
				mislabeled := filepath.Join(dir, "mislabeled.cue")
				if err := os.WriteFile(mislabeled, []byte("FILE \"audio.wav\" WAVE\n TRACK 01 "+mode+"\n INDEX 01 00:00:00\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := ValidateCUE(mislabeled, "", testMaxOutput); err == nil || !strings.Contains(err.Error(), "AUDIO track without subcode") {
					t.Fatalf("expected WAVE mode %s to fail, got %v", mode, err)
				}
			}
			multi := filepath.Join(dir, "multiple.cue")
			if err := os.WriteFile(multi, []byte("FILE \"audio.wav\" WAVE\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n TRACK 02 AUDIO\n INDEX 01 00:00:03\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateCUE(multi, "", testMaxOutput); err == nil || !strings.Contains(err.Error(), "multiple CUE tracks") {
				t.Fatalf("expected refusal to change the WAVE track boundaries, got %v", err)
			}
		}
	}
}

func TestCHDCUERefusesMotorolaTracks(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		sectorSize int
	}{
		{"audio", "AUDIO", 2352},
		{"data", "MODE1/2048", 2048},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			cue, bin := filepath.Join(dir, "disc.cue"), filepath.Join(dir, "track.bin")
			descriptor := "FILE \"track.bin\" MOTOROLA\n TRACK 01 " + test.mode + "\n INDEX 01 00:00:00\n"
			if err := os.WriteFile(cue, []byte(descriptor), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, bytes.Repeat([]byte{0x15, 0x36}, test.sectorSize), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateCUE(cue, "", testMaxOutput); err == nil || !strings.Contains(err.Error(), "MOTOROLA") {
				t.Fatalf("expected refusal to change track byte order, got %v", err)
			}
		})
	}
}

func TestCHDRestorationChecksBINChecksumAndTrackGaps(t *testing.T) {
	dir := t.TempDir()
	chd, cue, bin := filepath.Join(dir, "disc.chd"), filepath.Join(dir, "disc.cue"), filepath.Join(dir, "disc.bin")
	const metadata = "TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:2 PREGAP:1 PGTYPE:VAUDIO PGSUB:NONE POSTGAP:0\x00"
	data := make([]byte, chdV5HeaderSize+16+len(metadata))
	copy(data, "MComprHD")
	binary.BigEndian.PutUint32(data[8:12], chdV5HeaderSize)
	binary.BigEndian.PutUint32(data[12:16], 5)
	binary.BigEndian.PutUint64(data[32:40], 4*chdCDFrameBytes)
	binary.BigEndian.PutUint64(data[48:56], chdV5HeaderSize)
	binary.BigEndian.PutUint32(data[56:60], 4*chdCDFrameBytes)
	binary.BigEndian.PutUint32(data[60:64], chdCDFrameBytes)
	canonical := make([]byte, 4*chdCDFrameBytes)
	audioFrame := bytes.Repeat([]byte{0x36, 0x15}, 2352/2)
	copy(canonical, audioFrame)
	copy(canonical[chdCDFrameBytes:], audioFrame)
	digest := sha1.Sum(canonical)
	copy(data[64:84], digest[:])
	entry := data[chdV5HeaderSize:]
	copy(entry, "CHT2")
	binary.BigEndian.PutUint32(entry[4:8], uint32(len(metadata))|1<<24)
	copy(entry[16:], metadata)
	// chdman reads the textual track table first, even if an older copy remains.
	binary.BigEndian.PutUint64(entry[8:16], uint64(len(data)))
	var legacy [16]byte
	copy(legacy[:], "CHCD")
	data = append(data, legacy[:]...)
	if err := os.WriteFile(chd, data, 0600); err != nil {
		t.Fatal(err)
	}
	track := bytes.Repeat([]byte{0x15, 0x36}, 2352)
	if err := os.WriteFile(bin, track, 0600); err != nil {
		t.Fatal(err)
	}
	cueText := "FILE \"disc.bin\" BINARY\n TRACK 01 AUDIO\n INDEX 00 00:00:00\n INDEX 01 00:00:01\n"
	if err := os.WriteFile(cue, []byte(cueText), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := verifyCreatedCUE(ctx, cue, chd, testMaxOutput); err != nil {
		t.Fatal(err)
	}
	layout, err := cdExtractionLayout(ctx, chd, testMaxOutput)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.verifyRestored(ctx, cue, bin, testMaxOutput); err != nil {
		t.Fatal(err)
	}
	track[0] ^= 1
	if err := os.WriteFile(bin, track, 0600); err != nil {
		t.Fatal(err)
	}
	if err := layout.verifyRestored(ctx, cue, bin, testMaxOutput); err == nil || !strings.Contains(err.Error(), "raw SHA-1") {
		t.Fatalf("expected changed BIN bytes to fail verification, got %v", err)
	}
	if err := verifyCreatedCUE(ctx, cue, chd, testMaxOutput); err == nil || !strings.Contains(err.Error(), "raw SHA-1") {
		t.Fatalf("accepted an internally consistent CHD made from different input bytes: %v", err)
	}
	if err := os.WriteFile(cue, []byte(strings.Replace(cueText, "INDEX 01 00:00:01", "INDEX 01 00:00:00", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := layout.verifyRestored(ctx, cue, bin, testMaxOutput); err == nil || !strings.Contains(err.Error(), "track 1 differs") {
		t.Fatalf("expected changed CUE pregap to fail verification, got %v", err)
	}
}
