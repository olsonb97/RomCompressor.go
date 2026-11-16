package chd

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

const dvdSectorSize = 2048

// chdman createdvd consists of raw compression with 2048-byte units and a
// checksummed DVD type tag containing a NUL byte. The common primitives
// create the same layout on engines that predate the named DVD commands.
func createDVDCHD(ctx context.Context, tool, source, output string, limit int64) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size()%dvdSectorSize != 0 {
		return fmt.Errorf("DVD ISO must contain complete 2048-byte sectors")
	}
	if info.Size() > limit {
		return fmt.Errorf("DVD ISO exceeds the expanded size limit")
	}
	if _, err := process.Run(ctx, tool, "createraw", "-i", source, "-o", output, "-np", "2", "-hs", "2048", "-us", "2048"); err != nil {
		return err
	}
	if err := writeDVDCHDTag(output, info.Size()); err != nil {
		return err
	}
	if err := Verify(ctx, tool, output, limit, "dvd"); err != nil {
		return err
	}
	image, err := readDVDCHD(output)
	if err != nil {
		return err
	}
	return image.checkRaw(ctx, source)
}

// A finished compressed CHD cannot be reopened for metadata writes by
// older chdman releases. Add the single type tag to our private, untagged
// createraw output, then verify the resulting container with chdman.
func writeDVDCHDTag(path string, expectedSize int64) (result error) {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("chdman did not produce a regular CHD file")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return fmt.Errorf("staged CHD changed before DVD metadata was added")
	}
	if err := fileops.RequirePrivate(file); err != nil {
		return err
	}
	var header [chdV5HeaderSize]byte
	image, err := readDVDCHDHeader(file, &header)
	if err != nil {
		return err
	}
	if image.size != expectedSize || binary.BigEndian.Uint64(header[48:56]) != 0 {
		return fmt.Errorf("expected an untagged CHD matching the source DVD size")
	}
	// CHD v5 metadata entry: four-byte tag, checksum flag, three-byte
	// payload length, eight-byte next offset, and a one-byte NUL payload.
	entry := [17]byte{'D', 'V', 'D', ' ', 1, 0, 0, 1}
	if _, err := file.WriteAt(entry[:], info.Size()); err != nil {
		return err
	}
	binary.BigEndian.PutUint64(header[48:56], uint64(info.Size()))
	// The overall SHA-1 covers the raw digest and the sorted tag/digest
	// pairs for checksummed metadata. This file has just the DVD entry.
	var digestInput [44]byte
	copy(digestInput[:20], image.digest[:])
	copy(digestInput[20:24], entry[:4])
	payloadHash := sha1.Sum(entry[16:])
	copy(digestInput[24:], payloadHash[:])
	overall := sha1.Sum(digestInput[:])
	copy(header[84:104], overall[:])
	_, err = file.WriteAt(header[:], 0)
	return err
}

func extractDVDCHD(ctx context.Context, tool, source, output string, limit int64) error {
	image, err := readDVDCHD(source)
	if err != nil {
		return err
	}
	if image.size > limit {
		return fmt.Errorf("DVD CHD exceeds the expanded size limit")
	}
	if err := Verify(ctx, tool, source, limit, "dvd"); err != nil {
		return err
	}
	// extractdvd and extractraw use the same extraction implementation.
	if _, err := process.Run(ctx, tool, "extractraw", "-i", source, "-o", output); err != nil {
		return err
	}
	return image.checkRaw(ctx, output)
}

func readDVDCHD(path string) (rawCHD, error) {
	file, _, err := fileops.OpenRegular(path)
	if err != nil {
		return rawCHD{}, err
	}
	defer file.Close()
	var header [chdV5HeaderSize]byte
	return readDVDCHDHeader(file, &header)
}

func readDVDCHDHeader(src io.Reader, header *[chdV5HeaderSize]byte) (rawCHD, error) {
	image, err := readCHDV5Header(src, header)
	if err != nil {
		return rawCHD{}, err
	}
	if image.sectorSize != dvdSectorSize {
		return rawCHD{}, fmt.Errorf("DVD CHD requires 2048-byte sectors")
	}
	return image, nil
}
