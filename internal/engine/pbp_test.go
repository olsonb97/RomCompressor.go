package engine

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/codec"
)

func TestPBPCueKeepsStoredAudioPregap(t *testing.T) {
	root := t.TempDir()
	files := []pbpCueFile{
		{path: filepath.Join(root, "game.bin"), sectors: 301},
		{path: filepath.Join(root, "music.bin"), offset: 301, sectors: 161},
	}
	contents := `FILE "game.bin" BINARY
  TRACK 01 MODE2/2352
    INDEX 01 00:00:00
FILE "music.bin" BINARY
  TRACK 02 AUDIO
    INDEX 00 00:00:00
    INDEX 01 00:02:00
`
	tracks, err := parsePBPCue(root, strings.NewReader(contents), files)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tracks, []codec.PBPTrack{{}, {Audio: true, Index0: 301, Index1: 451}}) {
		t.Fatalf("unexpected PBP track layout: %+v", tracks)
	}
	merged, err := pbpCueText("combined.bin", tracks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(merged, "INDEX 00 00:04:01\n    INDEX 01 00:06:01") {
		t.Fatalf("merged CUE changed pregap: %s", merged)
	}
	for _, directive := range []string{"PREGAP 00:02:00", "INDEX 03 00:02:01", "FLAGS PRE", "REM SESSION 02"} {
		if _, err := parsePBPCue(root, strings.NewReader(contents+directive+"\n"), files); err == nil {
			t.Fatalf("accepted unsupported CUE directive %q", directive)
		}
	}
}
