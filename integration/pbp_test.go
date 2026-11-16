package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestPSPBootPackageRemainsAvailableForGenericArchives(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "EBOOT.PBP")
	const psar = 80
	data := make([]byte, psar)
	copy(data, "\x00PBP")
	binary.LittleEndian.PutUint32(data[4:], 0x00010000)
	for offset := 8; offset < 36; offset += 4 {
		binary.LittleEndian.PutUint32(data[offset:], 40)
	}
	binary.LittleEndian.PutUint32(data[36:], psar)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := romcompressor.Inspect(ctx, source)
	if err != nil || info.Format != romcompressor.Raw {
		t.Fatalf("PSP PBP classified as %+v, %v", info, err)
	}
	archived := filepath.Join(dir, "boot.zip")
	if _, err := romcompressor.Convert(ctx, source, archived, romcompressor.Options{Format: romcompressor.ZIP}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.pbp")
	if _, err := romcompressor.Convert(ctx, archived, restored, romcompressor.Options{Format: romcompressor.Raw}); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(restored)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("archiving the PSP PBP changed bytes: %v", err)
	}
}

func TestInspectPBPHonorsCancellationAfterEntry(t *testing.T) {
	packed, _ := testfixture.PBP(t)
	for _, ext := range []string{".pbp", ".bin"} {
		t.Run(ext, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "disc"+ext)
			if err := os.WriteFile(source, packed, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Cancel immediately after Inspect's first context check passes.
			info, err := romcompressor.Inspect(cancelAfterPBPInspectCheck{Context: ctx, cancel: cancel}, source)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Inspect returned %+v, %v after cancellation", info, err)
			}
		})
	}
}

type cancelAfterPBPInspectCheck struct {
	context.Context
	cancel context.CancelFunc
}

func (c cancelAfterPBPInspectCheck) Err() error {
	err := c.Context.Err()
	c.cancel()
	return err
}

func TestInspectPreservesRawFallbackForInvalidPBPPrefix(t *testing.T) {
	source := filepath.Join(t.TempDir(), "disc.bin")
	if err := os.WriteFile(source, []byte("\x00PBP"), 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := romcompressor.Inspect(context.Background(), source); err != nil || info.Format != romcompressor.Raw {
		t.Fatalf("Inspect returned %+v, %v for a raw file with a PBP prefix", info, err)
	}
}

func TestPBPExtractionWithProgress(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "disc.pbp")
	packed, raw := testfixture.PBP(t)
	if err := os.WriteFile(source, packed, 0600); err != nil {
		t.Fatal(err)
	}
	cue := filepath.Join(dir, "restored.cue")
	opts := romcompressor.Options{Format: romcompressor.Raw, Progress: func(romcompressor.Progress) {}}
	result, err := romcompressor.Convert(context.Background(), source, cue, opts)
	if err != nil || !result.Verified {
		t.Fatalf("PBP extraction = %+v, %v", result, err)
	}
	if actual, err := os.ReadFile(filepath.Join(dir, "restored.bin")); err != nil || !bytes.Equal(actual, raw) {
		t.Fatalf("PBP extraction changed decoded bytes: %v", err)
	}
}

func TestPBPExtractionRejectsChangesInProgress(t *testing.T) {
	for _, hardLink := range []bool{false, true} {
		name := "changed BIN bytes"
		if hardLink {
			name = "CUE aliases original PBP"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "disc.pbp")
			packed, _ := testfixture.PBP(t)
			if err := os.WriteFile(source, packed, 0600); err != nil {
				t.Fatal(err)
			}
			if hardLink {
				if err := os.Link(source, filepath.Join(dir, "link-probe.pbp")); err != nil {
					t.Skipf("file system cannot create hard links: %v", err)
				}
			}
			output := filepath.Join(dir, "restored.cue")
			var changed bool
			_, err := romcompressor.Convert(context.Background(), source, output, romcompressor.Options{Format: romcompressor.Raw, Progress: func(p romcompressor.Progress) {
				if changed || p.Phase != "decompressing" {
					return
				}
				stages, err := filepath.Glob(filepath.Join(dir, ".romcompressor-*"))
				if err != nil || len(stages) != 1 {
					t.Fatalf("locate stage: %v, %v", stages, err)
				}
				if hardLink {
					if err := os.Link(source, filepath.Join(stages[0], filepath.Base(output))); err != nil {
						t.Fatal(err)
					}
				} else {
					file, err := os.OpenFile(filepath.Join(stages[0], "restored.bin"), os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, err = file.WriteAt([]byte{0xa5}, 0)
					if err := errors.Join(err, file.Close()); err != nil {
						t.Fatal(err)
					}
				}
				changed = true
			}})
			if !changed || err == nil {
				t.Fatalf("Convert = %v, stage changed = %v", err, changed)
			}
			if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, packed) {
				t.Fatalf("original PBP changed: %v", err)
			}
			for _, path := range []string{output, filepath.Join(dir, "restored.bin")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("published a file after a staging change: %v", err)
				}
			}
		})
	}
}

// ROMCOMPRESSOR_PSXPACKAGER opts into a SHA-verified official engine. All image
// bytes and the tiny ISO9660 fixture below are generated here.
func TestPSXPackagerRoundTripIntegration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_PSXPACKAGER")
	if tool == "" {
		t.Skip("ROMCOMPRESSOR_PSXPACKAGER not set")
	}
	for _, mixed := range []bool{false, true} {
		name := "data"
		if mixed {
			name = "mixed mode"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			data := syntheticPSXDisc()
			if err := os.WriteFile(filepath.Join(dir, "data track.bin"), data, 0600); err != nil {
				t.Fatal(err)
			}
			cue := "FILE \"data track.bin\" BINARY\n  TRACK 01 MODE2/2352\n    INDEX 01 00:00:00\n"
			if mixed {
				audio := make([]byte, 161*codec.PBPBytesPerSector)
				for i := range audio {
					audio[i] = byte(i ^ i>>9)
				}
				if err := os.WriteFile(filepath.Join(dir, "audio track.bin"), audio, 0600); err != nil {
					t.Fatal(err)
				}
				cue += "FILE \"audio track.bin\" BINARY\n  TRACK 02 AUDIO\n    INDEX 00 00:00:00\n    INDEX 01 00:02:00\n"
				data = append(data, audio...)
			}
			source := filepath.Join(dir, "source.cue")
			if err := os.WriteFile(source, []byte(cue), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			packed := filepath.Join(dir, "packed.pbp")
			result, err := romcompressor.Convert(ctx, source, packed, romcompressor.Options{Format: romcompressor.PBP, Tools: map[string]string{"psxpackager": tool}})
			if err != nil || !result.Verified {
				t.Fatalf("PSXPackager compression: %+v, %v", result, err)
			}
			if err := romcompressor.Verify(ctx, packed, romcompressor.Options{}); err != nil {
				t.Fatal(err)
			}
			restoredCue := filepath.Join(dir, "restored disc.cue")
			if _, err := romcompressor.Convert(ctx, packed, restoredCue, romcompressor.Options{Format: romcompressor.Raw}); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(filepath.Join(dir, "restored disc.bin"))
			if err != nil || !bytes.Equal(restored, data) {
				t.Fatalf("PBP round trip changed disc sectors: %v", err)
			}
			wantCue := "FILE \"restored disc.bin\" BINARY\n  TRACK 01 MODE2/2352\n    INDEX 01 00:00:00\n"
			if mixed {
				wantCue += "  TRACK 02 AUDIO\n    INDEX 00 00:04:21\n    INDEX 01 00:06:21\n"
			}
			actualCue, err := os.ReadFile(restoredCue)
			if err != nil || string(actualCue) != wantCue {
				t.Fatalf("PBP round trip changed track indexes: %q, %v", actualCue, err)
			}
			blockedCue := filepath.Join(dir, "blocked.cue")
			blockedBin := filepath.Join(dir, "blocked.bin")
			const original = "existing output"
			if err := os.WriteFile(blockedBin, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := romcompressor.Convert(ctx, packed, blockedCue, romcompressor.Options{Format: romcompressor.Raw}); err == nil {
				t.Fatal("PBP extraction replaced an existing companion BIN")
			}
			if _, err := os.Lstat(blockedCue); !os.IsNotExist(err) {
				t.Fatalf("PBP extraction published a CUE without its BIN: %v", err)
			}
			blocked, err := os.ReadFile(blockedBin)
			if err != nil || string(blocked) != original {
				t.Fatalf("PBP extraction damaged the existing BIN: %v", err)
			}
		})
	}
}

func syntheticPSXDisc() []byte {
	const logicalSize = 2048
	const sectors = 321
	raw := make([]byte, sectors*codec.PBPBytesPerSector)
	for i := range sectors {
		sector := raw[i*codec.PBPBytesPerSector:]
		sector[0], sector[11] = 0, 0
		for j := 1; j <= 10; j++ {
			sector[j] = 0xff
		}
		sector[15], sector[18], sector[22] = 2, 8, 8
	}
	block := func(n int) []byte { return raw[n*codec.PBPBytesPerSector+24:][:logicalSize] }
	endian16 := func(out []byte, v uint16) {
		binary.LittleEndian.PutUint16(out, v)
		binary.BigEndian.PutUint16(out[2:], v)
	}
	endian32 := func(out []byte, v uint32) {
		binary.LittleEndian.PutUint32(out, v)
		binary.BigEndian.PutUint32(out[4:], v)
	}
	directoryRecord := func(out []byte, extent, size uint32, dir bool, name []byte) int {
		length := 33 + len(name)
		length += length % 2
		out[0] = byte(length)
		endian32(out[2:], extent)
		endian32(out[10:], size)
		copy(out[18:], []byte{126, 11, 14, 12, 0, 0, 0})
		if dir {
			out[25] = 2
		}
		endian16(out[28:], 1)
		out[32] = byte(len(name))
		copy(out[33:], name)
		return length
	}
	pvd, terminator := block(16), block(17)
	pvd[0], pvd[6], terminator[0], terminator[6] = 1, 1, 255, 1
	copy(pvd[1:], "CD001")
	copy(terminator[1:], "CD001")
	copy(pvd[8:72], strings.Repeat(" ", 64))
	copy(pvd[40:], "ROMCOMPRESSOR TEST")
	endian32(pvd[80:], sectors)
	endian16(pvd[120:], 1)
	endian16(pvd[124:], 1)
	endian16(pvd[128:], logicalSize)
	endian32(pvd[132:], 10)
	binary.LittleEndian.PutUint32(pvd[140:], 18)
	binary.BigEndian.PutUint32(pvd[148:], 19)
	directoryRecord(pvd[156:], 20, logicalSize, true, []byte{0})
	for _, offset := range []int{813, 830, 847, 864} {
		copy(pvd[offset:], "2026111412000000")
	}
	pvd[881] = 1
	block(18)[0], block(19)[0] = 1, 1
	binary.LittleEndian.PutUint32(block(18)[2:], 20)
	binary.BigEndian.PutUint32(block(19)[2:], 20)
	binary.LittleEndian.PutUint16(block(18)[6:], 1)
	binary.BigEndian.PutUint16(block(19)[6:], 1)
	root := block(20)
	offset := directoryRecord(root, 20, logicalSize, true, []byte{0})
	offset += directoryRecord(root[offset:], 20, logicalSize, true, []byte{1})
	config := []byte("BOOT = cdrom:\\SLUS_000.00;1\r\n")
	directoryRecord(root[offset:], 21, uint32(len(config)), false, []byte("SYSTEM.CNF;1"))
	copy(block(21), config)
	return raw
}
