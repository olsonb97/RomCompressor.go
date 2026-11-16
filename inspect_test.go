package romcompressor

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestNDSContainerTitlePrefixesRemainArchivable(t *testing.T) {
	for _, title := range []string{"CISO", "ZISO", "WBFS", "CCIM", "MComprHD", "WUX0"} {
		t.Run(title, func(t *testing.T) {
			data := testfixture.NDS(false)
			copy(data[:12], title)
			// Writable cartridges can be archived, although trimming refuses them.
			if title == "WBFS" {
				binary.LittleEndian.PutUint32(data[0x94:], 1)
			}
			fixNDSFixtureHeaderChecksum(data)
			dir := t.TempDir()
			source := filepath.Join(dir, "game.NDS")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if info, err := Inspect(ctx, source); err != nil || info.Format != Raw || info.Size != int64(len(data)) {
				t.Fatalf("Inspect cartridge title %q = %+v, %v", title, info, err)
			}
			if title == "WBFS" {
				if _, err := PlanTrim(ctx, source); err == nil || !strings.Contains(err.Error(), "writable NAND") {
					t.Fatalf("recognized writable cartridge allowed trimming: %v", err)
				}
			}
			for _, format := range []Format{ZIP, GZIP} {
				archive := filepath.Join(dir, "game."+string(format))
				restored := filepath.Join(dir, "restored-"+string(format)+".nds")
				if _, err := Convert(ctx, source, archive, Options{Format: format}); err != nil {
					t.Fatalf("archive cartridge as %s: %v", format, err)
				}
				if _, err := Convert(ctx, archive, restored, Options{Format: Raw}); err != nil {
					t.Fatalf("restore cartridge from %s: %v", format, err)
				}
				for _, path := range []string{source, restored} {
					got, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(got, data) {
						t.Fatalf("cartridge bytes changed for %s: %v", path, err)
					}
				}
			}
		})
	}
}

func TestNDSRecognitionRequiresNamedValidHeader(t *testing.T) {
	for _, scenario := range []string{"bad header checksum", "bad logo checksum", "different extension"} {
		t.Run(scenario, func(t *testing.T) {
			data := testfixture.NDS(false)
			copy(data[:12], "WBFS")
			fixNDSFixtureHeaderChecksum(data)
			ext := ".nds"
			switch scenario {
			case "bad header checksum":
				data[0x15e] ^= 1
			case "bad logo checksum":
				data[0xc0] ^= 1
				fixNDSFixtureHeaderChecksum(data)
			case "different extension":
				ext = ".bin"
			}
			path := filepath.Join(t.TempDir(), "game"+ext)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if info, err := Inspect(context.Background(), path); err == nil || info.Format == Raw {
				t.Fatalf("unconfirmed cartridge bypassed container detection: %+v, %v", info, err)
			}
		})
	}
}

// Recalculate the fixture's documented CRC-16 after editing title or NAND fields.
func fixNDSFixtureHeaderChecksum(data []byte) {
	crc := uint16(0xffff)
	for _, b := range data[:0x15e] {
		crc ^= uint16(b)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xa001
			} else {
				crc >>= 1
			}
		}
	}
	binary.LittleEndian.PutUint16(data[0x15e:], crc)
}

func TestInspectRejectsSplitArchiveStarts(t *testing.T) {
	for _, tt := range []struct {
		format Format
		magic  string
		names  []string
	}{
		{SevenZip, "7z\xbc\xaf\x27\x1c", []string{"archive.7z.001", "archive.001"}},
		{ZIP, "PK\x03\x04", []string{"archive.ZIP.001", "archive.001"}},
	} {
		for _, name := range tt.names {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte(tt.magic), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(context.Background(), path); err == nil || !strings.Contains(err.Error(), "split "+string(tt.format)) {
				t.Fatalf("Inspect %s with %s magic = %v", name, tt.format, err)
			}
		}
	}
}

func TestInspectRecognizesCartridgeBeforeEmbeddedSignaturePrefix(t *testing.T) {
	for _, tt := range []struct {
		ext   string
		magic string
	}{
		{".3ds", "NCSD"},
		{".cci", "NCSD"},
		{".xci", "HEAD"},
	} {
		path := filepath.Join(t.TempDir(), "game"+tt.ext)
		var header [512]byte
		copy(header[:], "\x1f\x8b\x08")
		copy(header[0x100:], tt.magic)
		if err := os.WriteFile(path, header[:], 0600); err != nil {
			t.Fatal(err)
		}
		if info, err := Inspect(context.Background(), path); err != nil || info.Format != Raw || info.Size != int64(len(header)) {
			t.Fatalf("Inspect %s = %+v, %v; want a raw cartridge", tt.ext, info, err)
		}
	}
}

func TestInspectSeparatesNintendoCISOAndPSPCSO(t *testing.T) {
	const headerSize, blockSize = 0x8000, 0x200000
	dir := t.TempDir()
	nintendo := make([]byte, headerSize+blockSize)
	copy(nintendo, "CISO")
	binary.LittleEndian.PutUint32(nintendo[4:8], blockSize)
	nintendo[8] = 1
	binary.BigEndian.PutUint32(nintendo[headerSize+0x1c:], 0xc2339f3d)
	ctx := context.Background()
	path := filepath.Join(dir, "gamecube.ciso")
	if err := os.WriteFile(path, nintendo, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(ctx, path); err != nil || info.Format != CISO {
		t.Fatalf("expected Nintendo Compact ISO, got %v, %v", info, err)
	}
	binary.LittleEndian.PutUint32(nintendo[4:8], 16)
	if err := os.WriteFile(path, nintendo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, path); err == nil {
		t.Fatal("malformed Nintendo header was accepted as PSP CSO")
	}
	raw := filepath.Join(dir, "psp.iso")
	cso := filepath.Join(dir, "psp.cso")
	alias := filepath.Join(dir, "psp.ciso")
	if err := os.WriteFile(raw, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(ctx, raw, cso, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cso, alias); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(ctx, alias); err != nil || info.Format != CSO {
		t.Fatalf("expected PSP CSO with a .ciso extension, got %v, %v", info, err)
	}
}

func TestInspectZ3DSContentFormats(t *testing.T) {
	for magic, format := range map[string]Format{"NCSD": ZCCI, "NCCH": ZCXI, "CIA\x00": ZCIA, "3DSX": Z3DSX} {
		t.Run(string(format), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "game."+string(format))
			data := testfixture.Z3DS(magic)
			data[len(data)-5] = 3
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			info, err := Inspect(context.Background(), path)
			if err != nil || info.Format != format {
				t.Fatalf("%+v, %v", info, err)
			}
		})
	}
}

func TestInspectCHDMediaWithoutAnEngine(t *testing.T) {
	for tag, want := range map[string]string{"CHT2": "cd", "DVD ": "dvd", "GDDD": "hdd", "AVAV": "laserdisc", "": ""} {
		t.Run(tag, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.chd")
			data := testfixture.CHDMetadata(5)
			if tag != "" {
				data = testfixture.CHDMetadata(5, tag)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			info, err := InspectWithOptions(context.Background(), path, Options{Tools: map[string]string{"chdman": "missing-engine"}})
			if err != nil || info.Format != CHD || info.Media != want {
				t.Fatalf("Inspect = %+v, %v; want media %q", info, err, want)
			}
		})
	}
}
