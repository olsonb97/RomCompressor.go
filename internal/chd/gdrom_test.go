package chd

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGDTrackRejectsUnrepresentableGeometry(t *testing.T) {
	const good = "TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:10 PAD:3 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"
	track, err := parseGDTrack(good, 1)
	if err != nil || track.padFrames != 3 || track.sectorBytes != 2352 {
		t.Fatalf("GD track = %+v, %v", track, err)
	}
	for _, change := range [][2]string{
		{"TRACK:1", "TRACK:2"}, {"FRAMES:10", "FRAMES:2147483648"},
		{"PAD:3", "PAD:-1"}, {"PAD:3", "PAD:10"}, {"TYPE:MODE1_RAW", "TYPE:MODE2_RAW"},
		{"SUBTYPE:NONE", "SUBTYPE:RW_RAW"}, {"PREGAP:0", "PREGAP:1"},
		{"PGSUB:NONE", "PGSUB:RW"}, {"POSTGAP:0", "POSTGAP:1"}, {"POSTGAP:0", "POSTGAP:0 extra"},
	} {
		if _, err := parseGDTrack(strings.Replace(good, change[0], change[1], 1), 1); err == nil {
			t.Errorf("accepted %s", change[1])
		}
	}
}

func TestGDTrackHashChecksAudioAndOmittedFrames(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "audio.bin")
	data := make([]byte, 2352)
	data[0], data[1] = 1, 2
	if err := os.WriteFile(bin, data, 0600); err != nil {
		t.Fatal(err)
	}
	track := chdCueTrack{mode: "AUDIO", frames: 3, padFrames: 2, sectorBytes: 2352}
	files := []chdCueFile{{path: bin, tracks: []chdCueTrack{track}}}
	for _, legacy := range []bool{false, true} {
		raw := make([]byte, 4*chdCDFrameBytes)
		raw[0], raw[1] = 2, 1
		if legacy {
			raw[0], raw[1] = 1, 2
		}
		layout := cdManifest{littleEndianAudio: legacy, image: rawCHD{size: int64(len(raw)), digest: sha1.Sum(raw)}}
		if err := layout.checkTrackData(t.Context(), files); err != nil {
			t.Fatal(err)
		}
		// A nonzero byte in an omitted inter-track sector cannot be recovered
		// from a GDI. The output must be refused even when every track fits.
		raw[chdCDFrameBytes] = 1
		layout.image.digest = sha1.Sum(raw)
		if err := layout.checkTrackData(t.Context(), files); err == nil {
			t.Fatal("lost nonzero inter-track data")
		}
	}
}
