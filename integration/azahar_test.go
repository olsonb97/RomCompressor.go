package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

// Set ROMCOMPRESSOR_AZAHAR to a current Azahar executable. Fixtures contain
// generated data, not copyrighted game contents; no emulation is invoked.
func TestAzaharRoundTripIntegration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_AZAHAR")
	if tool == "" {
		t.Skip("ROMCOMPRESSOR_AZAHAR is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, format := range []romcompressor.Format{romcompressor.Z3DSX, romcompressor.ZCXI, romcompressor.ZCCI, romcompressor.ZCIA} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			data := bytes.Repeat([]byte("generated homebrew compression fixture"), 16384)
			clear(data[:0x400])
			switch format {
			case romcompressor.Z3DSX:
				copy(data, "3DSX")
				binary.LittleEndian.PutUint16(data[4:], 32)
			case romcompressor.ZCXI:
				copy(data[0x100:], "NCCH")
				data[0x18f] = 4 // Decrypted NCCH.
			case romcompressor.ZCCI:
				copy(data[0x100:], "NCSD")
				binary.LittleEndian.PutUint32(data[0x120:], 1)
				binary.LittleEndian.PutUint32(data[0x124:], uint32(len(data)/512-1))
				copy(data[0x300:], "NCCH")
				data[0x38f] = 4
			case romcompressor.ZCIA:
				// One generated decrypted NCCH, with unsigned ticket/TMD structures.
				const ticketOffset, tmdOffset, contentOffset = 0x2040, 0x2300, 0x2e40
				clear(data[:contentOffset+0x200])
				binary.LittleEndian.PutUint32(data, 0x2020)
				binary.LittleEndian.PutUint32(data[12:], 0x2ac)
				binary.LittleEndian.PutUint32(data[16:], 0xb34)
				binary.LittleEndian.PutUint64(data[24:], uint64(len(data)-contentOffset))
				data[0x20] = 0x80
				binary.BigEndian.PutUint32(data[ticketOffset:], 0x10004)
				binary.BigEndian.PutUint32(data[tmdOffset:], 0x10004)
				binary.BigEndian.PutUint16(data[tmdOffset+0x1de:], 1)
				binary.BigEndian.PutUint64(data[tmdOffset+0xb0c:], uint64(len(data)-contentOffset))
				copy(data[contentOffset+0x100:], "NCCH")
				data[contentOffset+0x18f] = 4
			}
			source := filepath.Join(dir, "generated homebrew"+kind.ThreeDSRawExtension(string(format)))
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			compressed := filepath.Join(dir, "archive."+string(format))
			opts := romcompressor.Options{Format: format, Tools: map[string]string{"azahar": tool}}
			if _, err := romcompressor.Convert(ctx, source, compressed, opts); err != nil {
				t.Fatal(err)
			}
			if err := romcompressor.Verify(ctx, compressed, opts); err != nil {
				t.Fatal(err)
			}
			opts.Format = romcompressor.Raw
			restored := filepath.Join(dir, "restored"+kind.ThreeDSRawExtension(string(format)))
			if _, err := romcompressor.Convert(ctx, compressed, restored, opts); err != nil {
				t.Fatal(err)
			}
			if err := fileops.CompareFiles(ctx, source, restored); err != nil {
				t.Fatal(err)
			}
			if _, err := romcompressor.Convert(ctx, compressed, source, opts); err == nil {
				t.Fatal("overwrote original")
			}
			got, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("original changed")
			}
		})
	}
}
