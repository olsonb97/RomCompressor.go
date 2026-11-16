package chd

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestDVDMetadataPreservesHardLinkedFile(t *testing.T) {
	var header [chdV5HeaderSize]byte
	copy(header[:], "MComprHD")
	binary.BigEndian.PutUint32(header[8:], chdV5HeaderSize)
	binary.BigEndian.PutUint32(header[12:], 5)
	binary.BigEndian.PutUint64(header[32:], dvdSectorSize)
	binary.BigEndian.PutUint32(header[60:], dvdSectorSize)
	header[64] = 1
	dir := t.TempDir()
	original := filepath.Join(dir, "original.chd")
	staged := filepath.Join(dir, "engine-output.chd")
	if err := os.WriteFile(original, header[:], 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, staged); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := writeDVDCHDTag(staged, dvdSectorSize); err == nil {
		t.Error("tagged a staged file hard-linked to an existing CHD")
	}
	actual, err := os.ReadFile(original)
	if err != nil || !bytes.Equal(actual, header[:]) {
		t.Fatalf("original CHD changed: %v", err)
	}
}
