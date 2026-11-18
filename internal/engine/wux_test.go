package engine

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

func TestWUXRejectsIncompleteAndUnsafeImages(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "generated.wux")
	const discSize = uint64(0x5d3a00000)
	const indexEnd = ((32 + ((discSize+0x7fff)/0x8000)*4 + 0x7fff) / 0x8000) * 0x8000
	data := make([]byte, indexEnd+0x8000)
	binary.LittleEndian.PutUint32(data[0:4], 0x30585557)
	binary.LittleEndian.PutUint32(data[4:8], 0x1099d02e)
	binary.LittleEndian.PutUint32(data[8:12], 0x8000)
	binary.LittleEndian.PutUint64(data[16:24], discSize)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWUXSource(ctx, file, kind.WUX, Config{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWUXSource(ctx, file, kind.WUX, Config{MaxOutput: 1 << 20}); err == nil {
		t.Fatal("accepted output over the limit")
	}
	if err := ValidateWUXSource(ctx, file, kind.Raw, Config{}); err == nil {
		t.Fatal("accepted incomplete WUD")
	}
	if err := ValidateWUXSource(ctx, file, kind.Raw, Config{MaxOutput: 1 << 20}); err == nil || !strings.Contains(err.Error(), "expanded size limit") {
		t.Fatalf("JWUDTool compression ignored the expanded size limit: %v", err)
	}
	binary.LittleEndian.PutUint32(data[32:36], 1)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWUXSource(ctx, file, kind.WUX, Config{}); err == nil {
		t.Fatal("accepted sector outside the file")
	}
	if err := os.WriteFile(file, data[:32], 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWUXSource(ctx, file, kind.WUX, Config{}); err == nil {
		t.Fatal("accepted missing sector table")
	}
}

func TestWUXRequiresVerificationMarker(t *testing.T) {
	for _, tc := range []struct {
		output string
		valid  bool
	}{
		{"Compressed files is valid.\r\n", true},
		{"Compressing: INVALID game.wud\nCompressed files is valid.\n", true},
		{"Compression successful!\n", false},
		{"Compressing: Compressed files is valid.\n", false},
		{"Compressed files is valid.\nWarning! (De)Compressed file is INVALID!\n", false},
	} {
		if got := wuxConfirmed(tc.output); got != tc.valid {
			t.Errorf("output %q: got %v", tc.output, got)
		}
	}
}

func TestWUXProgressLogKeepsVerificationAfterFullDiscProgress(t *testing.T) {
	const sectors = 0x5d3a00000 / 0x8000
	update := []byte(wuxProgressPrefix + "100.00% | Ratio: 1:2.00 | Read: 23865.00MB | Written: 11932.50MB\t")
	if len(update)*(sectors/10) <= 4<<20 {
		t.Fatal("test progress must exceed the engine log limit")
	}
	var log wuxProgressLog
	for range sectors / 10 {
		if _, err := log.Write(update); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Write([]byte("\r\nCompression successful!\r\nCompressed files is valid.\r\n")); err != nil {
		t.Fatal(err)
	}
	result, err := log.finish()
	if err != nil || !wuxConfirmed(result) || strings.Contains(result, "Progress") {
		t.Fatalf("unexpected engine log %q, %v", result, err)
	}
}

func TestWUXProgressLogHandlesSplitUpdates(t *testing.T) {
	const diagnostic = "Initial diagnostic\r\n\rCompressing into another format"
	const status = "\r\nWarning! (De)Compressed file is INVALID!\n\rCompr"
	stream := diagnostic + wuxProgressPrefix + "2.00%" + status
	var log wuxProgressLog
	for i := range len(stream) {
		if _, err := log.Write([]byte(stream[i : i+1])); err != nil {
			t.Fatal(err)
		}
	}
	result, err := log.finish()
	if err != nil || result != diagnostic+status {
		t.Fatalf("unexpected engine log %q, %v", result, err)
	}
}

func TestWUXProgressLogRejectsOversizedDiagnostics(t *testing.T) {
	var log wuxProgressLog
	if _, err := log.Write(make([]byte, (4<<20)+1)); err == nil {
		t.Fatal("accepted engine diagnostics exceeding 4 MiB")
	}
}

func TestWUXRejectsWUMADArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.wud")
	if err := os.WriteFile(path, []byte("Wii "), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWUXSource(context.Background(), path, kind.Raw, Config{}); err == nil || !strings.Contains(err.Error(), "WUMAD") {
		t.Fatalf("expected WUMAD archive error, got %v", err)
	}
}
