package engine

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

func TestDolphinHashOutput(t *testing.T) {
	want := sha1.Sum([]byte("generated disc"))
	valid := fmt.Sprintf("%x", want)
	for _, tc := range []struct {
		name string
		text string
		ok   bool
	}{
		{"single checksum", strings.ToUpper(valid) + "\r\n", true},
		{"missing checksum", "", false},
		{"diagnostic with checksum", "different verification mode\n" + valid + "\n", false},
		{"multiple checksums", valid + "\n" + valid + "\n", false},
		{"nonhex checksum", strings.Repeat("z", 40), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDolphinHash(tc.text)
			if tc.ok && (err != nil || got != want) || !tc.ok && err == nil {
				t.Fatalf("parseDolphinHash = %x, %v", got, err)
			}
		})
	}
}

func TestDolphinRawHashUsesSourceBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "game.iso")
	data := make([]byte, 512)
	copy(data, "GTEST1")
	binary.BigEndian.PutUint32(data[28:], 0xc2339f3d)
	data[len(data)-1] = 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkDolphinISO(path); err != nil {
		t.Fatal(err)
	}
	hash, err := dolphinDiscHash(context.Background(), filepath.Join(dir, "absent-engine"), path, dir, kind.Raw)
	if err != nil || hash != sha1.Sum(data) {
		t.Fatalf("raw hash = %x, %v", hash, err)
	}
}
