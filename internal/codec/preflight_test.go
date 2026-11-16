package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type headerOnlyReader struct {
	header []byte
	offset int64
	bytes  int
	reads  int
}

func (r *headerOnlyReader) ReadAt(p []byte, offset int64) (int, error) {
	r.reads++
	r.bytes += len(p)
	if offset != r.offset || len(p) != len(r.header) {
		return 0, errors.New("preflight read outside its bounded header")
	}
	return copy(p, r.header), nil
}

func TestNativeCreationPreflightReadsOnlyRequiredHeaders(t *testing.T) {
	gameCube := make([]byte, 32)
	binary.BigEndian.PutUint32(gameCube[28:], 0xc2339f3d)
	wii := make([]byte, 32)
	binary.BigEndian.PutUint32(wii[24:], 0x5d1c9ea3)
	xiso := make([]byte, cciSectorBytes)
	copy(xiso, xisoMagic)
	copy(xiso[len(xiso)-len(xisoMagic):], xisoMagic)
	binary.LittleEndian.PutUint32(xiso[20:], 33)
	binary.LittleEndian.PutUint32(xiso[24:], 1)
	for _, tc := range []struct {
		format string
		size   int64
		header []byte
		offset int64
	}{
		{"cso", 0, nil, 0}, {"zso", 0, nil, 0}, {"wux", WUDImageBytes, nil, 0},
		{"ciso", gameCubeDiscSize, gameCube, 0}, {"ciso", wiiDualLayerSize, wii, 0},
		{"wbfs", wiiSingleLayerSize, wii, 0}, {"cci", 34 * cciSectorBytes, xiso, xisoVolume * cciSectorBytes},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.format, tc.size), func(t *testing.T) {
			reader := &headerOnlyReader{header: tc.header, offset: tc.offset}
			if err := PreflightEncode(context.Background(), tc.format, reader, tc.size, 0); err != nil {
				t.Fatal(err)
			}
			if reader.bytes != len(tc.header) || reader.reads > 1 {
				t.Fatalf("unbounded preflight: %d bytes in %d reads", reader.bytes, reader.reads)
			}
		})
	}
}

func TestNativeCreationPreflightRejectsBeforeReadingImpossibleGeometry(t *testing.T) {
	for _, format := range []string{"wbfs", "ciso", "cci", "wux"} {
		reader := &headerOnlyReader{}
		if err := PreflightEncode(context.Background(), format, reader, 17, 0); err == nil || reader.reads != 0 {
			t.Fatalf("%s: invalid geometry reached source reads: %v (%d)", format, err, reader.reads)
		}
	}
	reader := &headerOnlyReader{}
	if err := PreflightEncode(context.Background(), "zip", reader, 18, 17); err == nil || reader.reads != 0 {
		t.Fatalf("size limit ignored: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PreflightEncode(ctx, "wux", reader, WUDImageBytes, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestNativeRestorationPreflightSharesCSOHeaderValidation(t *testing.T) {
	header := make([]byte, csoHeaderSize)
	copy(header, "CISO")
	binary.LittleEndian.PutUint32(header[4:], csoHeaderSize)
	binary.LittleEndian.PutUint32(header[16:], 2048)
	header[20] = 1
	for _, tc := range []struct {
		name  string
		edit  func([]byte)
		limit int64
		size  int64
		want  string
	}{
		{"valid", func([]byte) {}, 0, 28, ""},
		{"version", func(b []byte) { b[20] = 2 }, 0, 28, "unsupported cso version"},
		{"expansion", func(b []byte) { binary.LittleEndian.PutUint64(b[8:], 4096) }, 2048, 36, "expanded size limit"},
		{"block", func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 1) }, 0, 28, "invalid disc block size"},
		{"index", func([]byte) {}, 0, 24, "truncated disc index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Clone(header)
			tc.edit(data)
			reader := &headerOnlyReader{header: data}
			_, err := PreflightDecode(context.Background(), "cso", reader, tc.size, tc.limit)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("header validation: %v, want %s", err, tc.want)
			}
			if reader.reads != 1 || reader.bytes != csoHeaderSize {
				t.Fatalf("restoration scanned payload: %+v", reader)
			}
		})
	}
	reader := &headerOnlyReader{}
	if _, err := PreflightDecode(context.Background(), "wux", reader, 1, WUDImageBytes-1); err == nil || reader.reads != 0 {
		t.Fatalf("known WUX expansion not bounded first: %v", err)
	}
	if _, err := PreflightDecode(context.Background(), "cso", bytes.NewReader(nil), 0, 0); !errors.Is(err, io.EOF) {
		t.Fatalf("truncated read identity lost: %v", err)
	}
}
