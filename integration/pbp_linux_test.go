package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/codec"
)

func TestPBPCompressionNeverRewritesHardLinkedEngineOutput(t *testing.T) {
	const psar = 0x80
	const audioSectors = 448
	audio := make([]byte, audioSectors*codec.PBPBytesPerSector)
	copy(audio, "\x00PBP")
	for at := 8; at < 40; at += 4 {
		binary.LittleEndian.PutUint32(audio[at:], psar)
	}
	copy(audio[psar:], "PSISOIMG0000")
	binary.LittleEndian.PutUint32(audio[psar+0xbfc:], 0x100000)
	toc := audio[psar+0x800:]
	toc[2], toc[7], toc[12], toc[17], toc[22] = 0xa0, 1, 0xa1, 2, 0xa2
	toc[28], toc[29] = 0x05, 0x74 // 449 sectors including the data track.
	toc[30], toc[32], toc[38] = 0x41, 1, 2
	toc[40], toc[42], toc[44], toc[48], toc[49] = 0x01, 2, 2, 2, 1
	track := make([]byte, codec.PBPBytesPerSector)
	copy(track, "\x00\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\x00")
	track[15] = 2
	cue := "FILE \"data.bin\" BINARY\n TRACK 01 MODE2/2352\n  INDEX 01 00:00:00\n" +
		"FILE \"audio.bin\" BINARY\n TRACK 02 AUDIO\n  INDEX 01 00:00:00\n"
	for _, name := range []string{"audio.bin", "preexisting.pbp"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string][]byte{"disc.cue": []byte(cue), "data.bin": track, "audio.bin": audio}
			files[name] = audio
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			linked := filepath.Join(dir, name)
			t.Setenv("ROMCOMPRESSOR_TEST_PBP_LINK", linked)
			tool := filepath.Join(dir, "psxpackager")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nln \"$ROMCOMPRESSOR_TEST_PBP_LINK\" disc.pbp\n"), 0700); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "disc.pbp")
			if _, err := romcompressor.Convert(context.Background(), filepath.Join(dir, "disc.cue"), output, romcompressor.Options{
				Format: romcompressor.PBP, Tools: map[string]string{"psxpackager": tool},
			}); err == nil {
				t.Error("Convert accepted hard-linked engine output")
			}
			if got, err := os.ReadFile(linked); err != nil || !bytes.Equal(got, audio) {
				t.Errorf("file linked to engine output changed: %v", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Errorf("unexpected output publication: %v", err)
			}
		})
	}
}
