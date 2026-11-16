package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
)

const (
	testCCISector = 2048
	testXISOMagic = "MICROSOFT*XBOX*MEDIA"
)

func testXISO() []byte {
	data := make([]byte, 35*testCCISector)
	for i := range data[testCCISector : 2*testCCISector] {
		data[testCCISector+i] = byte(i*19 + i/31)
	}
	boot := data[32*testCCISector : 33*testCCISector]
	copy(boot, testXISOMagic)
	copy(boot[testCCISector-len(testXISOMagic):], testXISOMagic)
	binary.LittleEndian.PutUint32(boot[20:], 33)
	binary.LittleEndian.PutUint32(boot[24:], 32)
	copy(data[33*testCCISector:], "generated root directory")
	return data
}

func TestCCIConvertRoundTripAndSplitRefusal(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.xiso")
	compressed := filepath.Join(dir, "game.cci")
	restored := filepath.Join(dir, "restored.iso")
	image := testXISO()
	if err := os.WriteFile(source, image, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	result, err := romcompressor.Convert(ctx, source, compressed, romcompressor.Options{Format: romcompressor.CCI})
	if err != nil || !result.Verified || result.OutputBytes >= result.InputBytes {
		t.Fatalf("expected verified CCI compression: %+v, %v", result, err)
	}
	info, err := romcompressor.Inspect(ctx, compressed)
	if err != nil || info.Format != romcompressor.CCI {
		t.Fatalf("CCI inspection: %+v, %v", info, err)
	}
	if err := romcompressor.Verify(ctx, compressed, romcompressor.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := romcompressor.Convert(ctx, compressed, restored, romcompressor.Options{Format: romcompressor.Raw}); err != nil {
		t.Fatal(err)
	}
	recovered, err := os.ReadFile(restored)
	if err != nil || !bytes.Equal(image, recovered) {
		t.Fatalf("CCI restored different bytes: %v", err)
	}
	if original, err := os.ReadFile(source); err != nil || !bytes.Equal(image, original) {
		t.Fatalf("CCI changed the source: %v", err)
	}
	numbered := filepath.Join(dir, "game.1.cci")
	if _, err := romcompressor.Convert(ctx, source, numbered, romcompressor.Options{Format: romcompressor.CCI}); err == nil {
		t.Fatal("created an ambiguous split CCI filename")
	}
	if err := os.Rename(compressed, numbered); err != nil {
		t.Fatal(err)
	}
	if _, err := romcompressor.Inspect(ctx, numbered); err == nil {
		t.Fatal("treated a numbered CCI part as a complete disc")
	}
}

func TestCCIIndependentRawLayout(t *testing.T) {
	// Assemble a raw CCI using the published layout, without our encoder.
	image := testXISO()
	// An Xbox CCI payload can contain incidental Nintendo magic at 0x100.
	copy(image[0x100-32:], "NCSD")
	indexAt := 32 + len(image)
	archive := make([]byte, indexAt+4*(len(image)/testCCISector+1))
	copy(archive, "CCIM")
	binary.LittleEndian.PutUint32(archive[4:], 32)
	binary.LittleEndian.PutUint64(archive[8:], uint64(len(image)))
	binary.LittleEndian.PutUint64(archive[16:], uint64(indexAt))
	binary.LittleEndian.PutUint32(archive[24:], 2048)
	archive[28], archive[29] = 1, 2
	copy(archive[32:], image)
	for i := 0; i <= len(image)/testCCISector; i++ {
		binary.LittleEndian.PutUint32(archive[indexAt+i*4:], uint32((32+i*testCCISector)/4))
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "foreign.cci")
	output := filepath.Join(dir, "restored.iso")
	if err := os.WriteFile(source, archive, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := romcompressor.Convert(ctx, source, output, romcompressor.Options{Format: romcompressor.Raw}); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(restored, image) {
		t.Fatalf("unexpected restored raw CCI bytes: %v", err)
	}
	copy(archive[indexAt+8:indexAt+12], archive[indexAt+4:indexAt+8])
	if err := os.WriteFile(source, archive, 0600); err != nil {
		t.Fatal(err)
	}
	if err := romcompressor.Verify(ctx, source, romcompressor.Options{}); err == nil {
		t.Fatal("accepted non-increasing CCI sector offsets")
	}
}

func TestInspectKeepsNintendoCCIAsRaw(t *testing.T) {
	data := make([]byte, 512)
	copy(data[0x100:], "NCSD")
	path := filepath.Join(t.TempDir(), "nintendo.cci")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := romcompressor.Inspect(context.Background(), path); err != nil || info.Format != romcompressor.Raw {
		t.Fatalf("misidentified Nintendo .cci: %+v, %v", info, err)
	}
}
