package codec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"
)

func ecmFixtureSector(form byte) []byte {
	length := 2052
	header := []byte{1, 2, 8, 0}
	switch form {
	case 1:
		length, header = 2051, []byte{0, 2, 0}
	case 3:
		length = 2328
		header[2] |= 0x20
	}
	payload := make([]byte, length)
	copy(payload, header)
	for i := range payload[len(header):] {
		payload[len(header)+i] = byte(i*31 + i/17)
	}
	return ecmRestoreSector(make([]byte, cdSector), payload, form)
}

func TestECMSectorReconstruction(t *testing.T) {
	// Calculated by evaluating the Reed-Solomon syndromes at 1 and 2
	// directly with polynomial multiplication (ECMA-130 Annex A).
	expected := []string{
		"2d984c5ed800d3acb79688de1dd88868eef3f88204083245862be8ad43a8c6f7",
		"ac4a65da57b6ad72123348f6651ecf0b702e23fc4bdc46fa6829c3223697763e",
		"2e70e115b4e4911b131827116ca84f0e6018ecd8771949277f59c6181fce9a48",
	}
	for i, digest := range expected {
		form := byte(i + 1)
		sector := ecmFixtureSector(form)
		if sum := fmt.Sprintf("%x", sha256.Sum256(sector)); sum != digest {
			t.Fatalf("sector type %d has digest %s, want %s", form, sum, digest)
		}
		data := bytes.Repeat(sector, 35)
		encoded := roundTripECM(t, data)
		record := bufio.NewReader(bytes.NewReader(encoded[len(ecmMagic):]))
		actual, count, err := readECMRecord(record)
		if err != nil || actual != form || count != 34 {
			t.Fatalf("expected 35 modeled sectors of type %d, got %d type %d: %v", form, count+1, actual, err)
		}
	}
}

func roundTripECM(t *testing.T, data []byte) []byte {
	t.Helper()
	var encoded, restored bytes.Buffer
	ctx := context.Background()
	if err := encodeECM(ctx, bytes.NewReader(data), int64(len(data)), &encoded, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Decode(ctx, "ecm", bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), &restored, Options{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), data) {
		t.Fatal("ECM restored different bytes")
	}
	return encoded.Bytes()
}

func TestECMPreservesMixedRawAndAudioSectors(t *testing.T) {
	var original bytes.Buffer
	original.Write(ecmFixtureSector(1))
	// XA records omit the raw sync/address prefix. It must remain in literals.
	original.Write(cdSync)
	original.Write([]byte{0, 2, 1, 2})
	original.Write(ecmFixtureSector(2))
	original.Write(cdSync)
	original.Write([]byte{0, 2, 2, 2})
	original.Write(ecmFixtureSector(3))
	damagedParity := ecmFixtureSector(1)
	damagedParity[len(damagedParity)-1] ^= 1
	original.Write(damagedParity)
	original.Write(bytes.Repeat([]byte{0x01, 0xa9}, cdSector/2))
	if len(roundTripECM(t, original.Bytes())) >= original.Len() {
		t.Fatal("valid data sectors were not modeled")
	}
}

func TestECMChecksumAndLimits(t *testing.T) {
	// The CRC-32/CD-ROM-EDC check string with its checksum (6ec2edc4).
	valid := []byte("ECM\x00\x20123456789\xfc\xff\xff\xff\x3f\xc4\xed\xc2\x6e")
	var restored bytes.Buffer
	ctx := context.Background()
	if err := Decode(ctx, "ecm", bytes.NewReader(valid), int64(len(valid)), &restored, Options{}); err != nil || restored.String() != "123456789" {
		t.Fatalf("decode literal: %q, %v", &restored, err)
	}
	corrupt := bytes.Clone(valid)
	corrupt[6] ^= 1
	for _, data := range [][]byte{corrupt, valid[:len(valid)-1], append(bytes.Clone(valid), 1), []byte("ECM\x00\xfc\xff\xff\xff\x7f")} {
		if err := Decode(ctx, "ecm", bytes.NewReader(data), int64(len(data)), io.Discard, Options{}); err == nil {
			t.Fatalf("accepted malformed ECM %x", data)
		}
	}
	if err := Decode(ctx, "ecm", bytes.NewReader(valid), int64(len(valid)), io.Discard, Options{MaxOutput: 8}); err == nil {
		t.Fatal("accepted ECM larger than its expanded byte limit")
	}
}
