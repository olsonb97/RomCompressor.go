package chd

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestHDDCHDRejectsInvalidMetadata(t *testing.T) {
	const size = 2048
	disk := rawCHD{size: size, sectorSize: 512}
	for _, tc := range []struct {
		name    string
		tag     string
		payload string
		flags   byte
		next    uint64
		wantErr bool
	}{
		{"valid", "GDDD", "CYLS:2,HEADS:1,SECS:2,BPS:512\x00", 1, 0, false},
		{"wrong size", "GDDD", "CYLS:3,HEADS:1,SECS:2,BPS:512\x00", 1, 0, true},
		{"wrong unit", "GDDD", "CYLS:1,HEADS:1,SECS:2,BPS:1024\x00", 1, 0, true},
		{"missing terminator", "GDDD", "CYLS:2,HEADS:1,SECS:2,BPS:512", 1, 0, true},
		{"overflow", "GDDD", "CYLS:4294967295,HEADS:4294967295,SECS:4294967295,BPS:512\x00", 1, 0, true},
		{"unchecksummed", "GDDD", "CYLS:2,HEADS:1,SECS:2,BPS:512\x00", 0, 0, true},
		{"cycle", "GDDD", "CYLS:2,HEADS:1,SECS:2,BPS:512\x00", 1, chdV5HeaderSize, true},
		{"truncated next record", "GDDD", "CYLS:2,HEADS:1,SECS:2,BPS:512\x00", 1, 1 << 60, true},
		{"DVD", "DVD ", "\x00", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, chdV5HeaderSize+16+len(tc.payload))
			record := data[chdV5HeaderSize:]
			copy(record[:4], tc.tag)
			binary.BigEndian.PutUint32(record[4:8], uint32(tc.flags)<<24|uint32(len(tc.payload)))
			binary.BigEndian.PutUint64(record[8:16], tc.next)
			copy(record[16:], tc.payload)
			err := checkHDDMetadata(bytes.NewReader(data), uint64(len(data)), chdV5HeaderSize, disk)
			if (err != nil) != tc.wantErr {
				t.Fatalf("metadata validation = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestDVDCHDRawVerificationRejectsDifferentBytes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "disc.iso")
	chd := filepath.Join(dir, "disc.chd")
	raw := bytes.Repeat([]byte{0xe2, 0x1f}, dvdSectorSize/2)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var header [chdV5HeaderSize]byte
	copy(header[:8], "MComprHD")
	binary.BigEndian.PutUint32(header[8:12], chdV5HeaderSize)
	binary.BigEndian.PutUint32(header[12:16], 5)
	binary.BigEndian.PutUint64(header[32:40], uint64(len(raw)))
	binary.BigEndian.PutUint32(header[56:60], dvdSectorSize)
	binary.BigEndian.PutUint32(header[60:64], dvdSectorSize)
	digest := sha1.Sum(raw)
	copy(header[64:84], digest[:])
	if err := os.WriteFile(chd, header[:], 0600); err != nil {
		t.Fatal(err)
	}
	expected, err := readDVDCHD(chd)
	if err != nil {
		t.Fatal(err)
	}
	if err := expected.checkRaw(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := expected.checkRaw(context.Background(), file); err == nil {
		t.Fatal("accepted a mismatched raw image of the same size")
	}
	clear(header[64:84])
	if err := os.WriteFile(chd, header[:], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDVDCHD(chd); err == nil {
		t.Fatal("accepted a DVD CHD without a stored raw checksum")
	}
}

func TestHDDCHDInvalidRawRejectedBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"partial.img", "not-a-disk.cue"} {
		source := filepath.Join(dir, name)
		if err := os.WriteFile(source, make([]byte, 513), 0600); err != nil {
			t.Fatal(err)
		}
		if err := createHDDCHD(context.Background(), "no-engine", source, filepath.Join(dir, "output.chd"), testMaxOutput); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}
