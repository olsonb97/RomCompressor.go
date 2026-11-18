package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestThreeDSHeaderAndSeekTable(t *testing.T) {
	for magic, format := range map[string]string{"NCSD": kind.ZCCI, "NCCH": kind.ZCXI, "CIA\x00": kind.ZCIA, "3DSX": kind.Z3DSX} {
		t.Run(string(format), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "game."+string(format))
			data := testfixture.Z3DS(magic)
			data[len(data)-5] = 3 // Both unused seek-table bits are ignored.
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			header, err := read3DSHeader(context.Background(), path, 1024)
			if err != nil || header.format != format || header.expanded != 1024 {
				t.Fatalf("%+v, %v", header, err)
			}
			if _, err := read3DSHeader(context.Background(), path, 1023); err == nil {
				t.Fatal("accepted oversized expansion")
			}
		})
	}
}

func TestThreeDSRejectsMalformedHeaders(t *testing.T) {
	for name, mutate := range map[string]func([]byte){
		"version":            func(b []byte) { b[8] = 2 },
		"reserved":           func(b []byte) { b[9] = 1 },
		"metadata":           func(b []byte) { binary.LittleEndian.PutUint32(b[12:], 0xffffffff) },
		"short header":       func(b []byte) { binary.LittleEndian.PutUint16(b[10:], 16) },
		"compressed length":  func(b []byte) { binary.LittleEndian.PutUint64(b[16:], 30) },
		"expanded length":    func(b []byte) { binary.LittleEndian.PutUint64(b[24:], 1025) },
		"frame count":        func(b []byte) { binary.LittleEndian.PutUint32(b[len(b)-9:], 0xffffffff) },
		"frame length":       func(b []byte) { binary.LittleEndian.PutUint32(b[len(b)-17:], 12) },
		"frame expansion":    func(b []byte) { binary.LittleEndian.PutUint32(b[len(b)-13:], 0xffffffff) },
		"table size":         func(b []byte) { binary.LittleEndian.PutUint32(b[len(b)-21:], 16) },
		"table magic":        func(b []byte) { b[len(b)-4] = 0 },
		"table reserved bit": func(b []byte) { b[len(b)-5] = 4 },
		"frame magic":        func(b []byte) { b[32] = 0 },
		"frame reserved bit": func(b []byte) { b[36] |= 8 },
		"frame content size": func(b []byte) { b[37]++ },
		"reserved block":     func(b []byte) { b[39] |= 4 },
		"missing checksum":   func(b []byte) { b[36] |= 4 },
	} {
		t.Run(name, func(t *testing.T) {
			data := testfixture.Z3DS("3DSX")
			mutate(data)
			path := filepath.Join(t.TempDir(), "game.z3dsx")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := read3DSHeader(context.Background(), path, 0); err == nil {
				t.Fatal("accepted malformed container")
			}
		})
	}
}

func TestThreeDSValidatesFrameAndMetadataExtents(t *testing.T) {
	frame := testfixture.Z3DS("3DSX")[32:43]
	// A windowed raw block with no frame content size, using a 1 KiB window.
	rawFrame := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0, 1, 0x20, 0}, bytes.Repeat([]byte{0x5a}, 1024)...)
	shortRaw := bytes.Clone(rawFrame[:len(rawFrame)-1])
	copy(shortRaw[6:9], []byte{0xf9, 0x1f, 0})
	goodMetadata := []byte{1, 1, 1, 2, 0, 'x', 'a', 'b', 0, 0, 0, 0}
	for _, tc := range []struct {
		name     string
		frame    []byte
		metadata []byte
		want     string
	}{
		{"complete records", frame, goodMetadata, ""},
		{"raw frame with no content size", rawFrame, nil, ""},
		{"raw frame expands short", shortRaw, nil, "declared expansion"},
		{"unsupported metadata version", frame, []byte{2, 0xff}, ""},
		{"short item", frame, []byte{1, 1, 1}, "metadata"},
		{"item overlaps data", frame, []byte{1, 1, 3, 0xff, 0xff, 'x'}, "metadata"},
		{"missing end", frame, goodMetadata[:8], "metadata"},
		{"extra frame bytes", append(append([]byte(nil), frame...), 0xa5), nil, "frame length"},
		{"truncated frame block", frame[:len(frame)-1], nil, "block exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "game.z3dsx")
			if err := os.WriteFile(path, testfixture.Z3DSWithFrame("3DSX", tc.frame, tc.metadata), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := read3DSHeader(context.Background(), path, 1024)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("read3DSHeader = %v; want error containing %q", err, tc.want)
			}
		})
	}
}

func TestAzahar2126Metadata(t *testing.T) {
	// Official 2126.2 Windows output from a generated 4 KiB 3DSX.
	const fixture = "WjNEUzNEU1gBACAAYAAAADEAAAAAAAAAABAAAAAAAAABAQQUAGRhdGUyMDI2LTExLTE3VDA3OjIxOjE3WgEMBgBtYXhmcmFtZXNpemUyNjIxNDQBCg0AY29tcHJlc3NvckF6YWhhciAyMTI2LjIAAAAAAAAAAAAAAAAAAAAAAAAotS/9AEB9AABAM0RTWCAACAABAPX3ARdeKk0YEQAAABgAAAAAEAAAAQAAAACx6pKP"
	data, err := base64.StdEncoding.DecodeString(fixture)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != "8a647397e931d4c3742570051b01d511f96a410aa80a7cd9245a52d1a820a8f0" {
		t.Fatal("bad regression fixture")
	}
	path := filepath.Join(t.TempDir(), "review.z3dsx")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	h, err := read3DSHeader(context.Background(), path, 4096)
	if err != nil || h.expanded != 4096 || h.format != kind.Z3DSX {
		t.Fatalf("official container: %+v, %v", h, err)
	}
}

func TestThreeDSMetadataAlignment(t *testing.T) {
	validate := func(ctx context.Context, b []byte) error {
		return validate3DSMetadata(ctx, io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b))))
	}
	ctx := context.Background()
	for n := 0; n < 16; n++ {
		data := append([]byte{1, 1, 1, byte(n), 0, 'x'}, bytes.Repeat([]byte{'v'}, n)...)
		data = append(data, 0, 0, 0, 0)
		if err := validate(ctx, data); err != nil {
			t.Fatalf("unpadded length %d: %v", len(data), err)
		}
		pad := (16 - len(data)%16) % 16
		padded := append(bytes.Clone(data), make([]byte, pad)...)
		if err := validate(ctx, padded); err != nil {
			t.Fatalf("padding %d: %v", pad, err)
		}
		if err := validate(ctx, append(bytes.Clone(padded), make([]byte, 16)...)); err == nil {
			t.Fatal("accepted excess padding")
		}
		if pad > 0 {
			padded[len(padded)-1] = 1
			if err := validate(ctx, padded); err == nil {
				t.Fatal("accepted nonzero padding")
			}
		}
	}
	for _, bad := range [][]byte{{1, 0, 0, 0, 0, 0}, {1, 0, 1, 0, 0, 'x'}, {1, 1, 0, 1, 0}, {1, 0, 0, 0}} {
		if err := validate(ctx, bad); err == nil {
			t.Fatalf("accepted malformed metadata %x", bad)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := validate(canceled, []byte{1, 0, 0, 0, 0}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled metadata check: %v", err)
	}
}

func TestThreeDSRejectsOversizedSourceBeforeLaunchingEngine(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "homebrew.3dsx")
	if err := os.WriteFile(source, []byte("3DSX"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Config{Format: kind.Z3DSX, MaxOutput: 3, Tools: map[string]string{"azahar": filepath.Join(dir, "missing-engine")}}
	if err := convert3DS(context.Background(), source, filepath.Join(dir, "homebrew.z3dsx"), kind.Raw, opts); err == nil || !strings.Contains(err.Error(), "expanded size limit") {
		t.Fatalf("convert3DS = %v; want source-size rejection before resolving the engine", err)
	}
}

func FuzzZ3DSFrame(f *testing.F) {
	f.Add(testfixture.Z3DS("3DSX")[32:43], uint64(1024))
	f.Add([]byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}, uint64(0))
	f.Fuzz(func(t *testing.T, data []byte, expanded uint64) {
		if len(data) > 128<<10 {
			t.Skip()
		}
		frame := io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))
		_ = codec.ValidateZstandardFrame(context.Background(), frame, expanded)
	})
}
