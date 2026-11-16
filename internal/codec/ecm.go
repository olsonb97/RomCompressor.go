package codec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

const (
	ecmMagic = "ECM\x00"
	cdSector = 2352
	xaSector = 2336
	ecmEnd   = ^uint32(0)
)

var (
	cdSync = []byte{0, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 0}
	cdEDC  = crc32.MakeTable(0xd8018001)
	// For each byte x, these tables compute x*2 and x/3 in GF(256).
	cdDouble, cdDivide = cdParityTables()
)

func ecmEDC(previous uint32, payload []byte) uint32 {
	// ECMA-130 uses an initial zero and no final complement.
	return ^crc32.Update(^previous, cdEDC, payload)
}

func cdParityTables() (double, divide [256]byte) {
	for value := range 256 {
		doubled := value << 1
		if doubled >= 256 {
			doubled ^= 0x11d
		}
		double[value] = byte(doubled)
		divide[doubled^value] = byte(value)
	}
	return double, divide
}

// cdParity generates the two Reed-Solomon check symbols for each interleave
// defined by ECMA-130 Annex A. The syndromes are evaluated at 1 and 2.
func cdParity(data, parity []byte, groups, symbols, startStride, step int) {
	for group := range groups {
		pos := group/2*startStride + group%2
		var sum, weighted byte
		for range symbols {
			symbol := data[pos]
			sum ^= symbol
			weighted = cdDouble[weighted^symbol]
			pos += step
			if pos >= len(data) {
				pos -= len(data)
			}
		}
		first := cdDivide[cdDouble[weighted]^sum]
		parity[group] = first
		parity[group+groups] = first ^ sum
	}
}

func ecmRestoreSector(sector, payload []byte, form byte) []byte {
	clear(sector)
	if form == 1 {
		copy(sector, cdSync)
		copy(sector[12:15], payload[:3])
		sector[15] = 1
		copy(sector[16:2064], payload[3:])
		binary.LittleEndian.PutUint32(sector[2064:2068], ecmEDC(0, sector[:2064]))
	} else {
		copy(sector[16:20], payload[:4])
		copy(sector[20:], payload)
		if form == 3 {
			binary.LittleEndian.PutUint32(sector[2348:], ecmEDC(0, sector[16:2348]))
			return sector[16:]
		}
		binary.LittleEndian.PutUint32(sector[2072:2076], ecmEDC(0, sector[16:2072]))
	}
	cdParity(sector[12:2076], sector[2076:2248], 86, 24, 2, 86)
	cdParity(sector[12:2248], sector[2248:], 52, 43, 86, 88)
	if form == 2 {
		return sector[16:]
	}
	return sector
}

// ECM stores count-minus-one, with two form bits in the first byte and
// up to four additional 7-bit groups. The reserved maximum marks EOF.
func ecmRecordHeader(form byte, countMinusOne uint32) []byte {
	var header [5]byte
	header[0] = byte(countMinusOne&31)<<2 | form
	remainder := countMinusOne >> 5
	length := 1
	for remainder != 0 {
		header[length-1] |= 0x80
		header[length] = byte(remainder & 127)
		remainder >>= 7
		length++
	}
	return header[:length]
}

func readECMRecord(r *bufio.Reader) (byte, uint32, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	form := first & 3
	value := uint32(first>>2) & 31
	for shift := uint(5); first&0x80 != 0; shift += 7 {
		first, err = r.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		part := uint32(first & 127)
		if shift >= 32 || part > ecmEnd>>shift {
			return 0, 0, errors.New("ECM record count overflow")
		}
		value |= part << shift
	}
	return form, value, nil
}

func decodeECM(ctx context.Context, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	reader := bufio.NewReaderSize(io.NewSectionReader(src, 0, size), 128<<10)
	var magic [4]byte
	if _, err := io.ReadFull(reader, magic[:]); err != nil || string(magic[:]) != ecmMagic {
		return errors.New("invalid ECM header")
	}
	var digest uint32
	var expanded int64
	buffer := make([]byte, 128<<10)
	var sector [cdSector]byte
	storedSizes := [...]int{0, 2051, 2052, 2328}
	outputSizes := [...]int64{1, cdSector, xaSector, xaSector}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		form, value, err := readECMRecord(reader)
		if err != nil {
			return fmt.Errorf("read ECM record: %w", err)
		}
		if value == ecmEnd {
			if form != 0 {
				return errors.New("invalid ECM end marker")
			}
			break
		}
		count := int64(value) + 1
		if count >= 1<<31 || count > (opts.MaxOutput-expanded)/outputSizes[form] {
			return errors.New("ECM output exceeds supported size")
		}
		for count > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			step := int64(1)
			if form == 0 {
				step = min(count, int64(len(buffer)))
			}
			stored := buffer[:max(int(step), storedSizes[form])]
			if _, err := io.ReadFull(reader, stored); err != nil {
				return fmt.Errorf("read ECM payload: %w", err)
			}
			payload := stored
			if form != 0 {
				payload = ecmRestoreSector(sector[:], stored, form)
			}
			if err := writeAll(dst, payload); err != nil {
				return err
			}
			digest = ecmEDC(digest, payload)
			count -= step
			expanded += int64(len(payload))
			if opts.Progress != nil {
				opts.Progress(expanded, 0)
			}
		}
	}
	var checksum [4]byte
	if _, err := io.ReadFull(reader, checksum[:]); err != nil {
		return fmt.Errorf("read ECM checksum: %w", err)
	}
	if binary.LittleEndian.Uint32(checksum[:]) != digest {
		return errors.New("ECM checksum mismatch")
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("ECM contains trailing data")
	}
	return ctx.Err()
}

func encodeECM(ctx context.Context, src io.ReaderAt, size int64, dst io.Writer, opts Options) error {
	writer := bufio.NewWriterSize(dst, 128<<10)
	if _, err := writer.WriteString(ecmMagic); err != nil {
		return err
	}
	remaining := size
	reader := bufio.NewReaderSize(io.NewSectionReader(src, 0, size), 128<<10)
	sectorSize := cdSector
	if size%cdSector != 0 && size%xaSector == 0 {
		sectorSize = xaSector
	}
	var digest uint32
	var rebuilt [cdSector]byte
	var modeOne [2051]byte
	var count uint32
	var runForm byte
	body := make([]byte, 0, 128<<10)
	flush := func() error {
		if count == 0 {
			return nil
		}
		if err := writeAll(writer, ecmRecordHeader(runForm, count-1)); err != nil {
			return err
		}
		if err := writeAll(writer, body); err != nil {
			return err
		}
		body = body[:0]
		count = 0
		return nil
	}
	add := func(form byte, data []byte) error {
		if form != runForm || len(data) > cap(body)-len(body) {
			if err := flush(); err != nil {
				return err
			}
		}
		runForm = form
		body = append(body, data...)
		if form == 0 {
			count += uint32(len(data))
		} else {
			count++
		}
		return nil
	}
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int(min(remaining, int64(sectorSize)))
		sector, err := reader.Peek(n)
		if err != nil {
			return err
		}
		form, prefix, data := ecmSectorPayload(sector, rebuilt[:], modeOne[:])
		if prefix != 0 {
			if err := add(0, sector[:prefix]); err != nil {
				return err
			}
		}
		if err := add(form, data); err != nil {
			return err
		}
		digest = ecmEDC(digest, sector)
		if _, err := reader.Discard(n); err != nil {
			return err
		}
		remaining -= int64(n)
		if opts.Progress != nil {
			opts.Progress(size-remaining, size)
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if err := writeAll(writer, ecmRecordHeader(0, ecmEnd)); err != nil {
		return err
	}
	var checksum [4]byte
	binary.LittleEndian.PutUint32(checksum[:], digest)
	if err := writeAll(writer, checksum[:]); err != nil {
		return err
	}
	return writer.Flush()
}

func ecmSectorPayload(sector, rebuilt, modeOne []byte) (form byte, prefix int, data []byte) {
	xa := sector
	if len(sector) == cdSector && bytes.HasPrefix(sector, cdSync) {
		switch sector[15] {
		case 1:
			copy(modeOne, sector[12:15])
			copy(modeOne[3:], sector[16:2064])
			if bytes.Equal(ecmRestoreSector(rebuilt, modeOne, 1), sector) {
				return 1, 0, modeOne
			}
		case 2:
			xa = sector[16:]
		}
	}
	if len(xa) == xaSector && bytes.Equal(xa[:4], xa[4:8]) {
		form, end := byte(2), 2056
		if xa[2]&0x20 != 0 {
			form, end = 3, 2332
		}
		// Check EDC first. Literal data, including audio and intentionally
		// unusual sectors, must survive without changing a byte.
		if binary.LittleEndian.Uint32(xa[end:end+4]) == ecmEDC(0, xa[:end]) &&
			bytes.Equal(ecmRestoreSector(rebuilt, xa[4:end], form), xa) {
			return form, len(sector) - len(xa), xa[4:end]
		}
	}
	return 0, 0, sector
}
