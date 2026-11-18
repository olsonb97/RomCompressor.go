package chd

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// ValidateNRG checks that Nero 5.5+ DAOX extents cover the entire track area
// without stored pregaps that chdman would model as virtual.
func ValidateNRG(ctx context.Context, path string, limit int64) error {
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var footer [12]byte
	footerAt := info.Size() - int64(len(footer))
	if footerAt < 8 || info.Size() > math.MaxUint32 {
		return fmt.Errorf("NRG must be a Nero 5.5+ image below 4 GiB")
	}
	if _, err := file.ReadAt(footer[:], footerAt); err != nil {
		return err
	}
	dataEnd := binary.BigEndian.Uint64(footer[4:])
	if string(footer[:4]) != "NER5" || dataEnd == 0 || dataEnd > uint64(footerAt-8) {
		return fmt.Errorf("invalid NRG footer or track data extent")
	}
	if int64(dataEnd) > limit {
		return fmt.Errorf("NRG tracks exceed the expanded size limit")
	}
	var chunk [8]byte
	var trackCount int
	var sessionTracks uint32
	for at, count := int64(dataEnd), 0; at <= footerAt-8 && count < 1024; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := file.ReadAt(chunk[:], at); err != nil {
			return err
		}
		length := int64(binary.BigEndian.Uint32(chunk[4:]))
		at += int64(len(chunk))
		if length > footerAt-at {
			return fmt.Errorf("NRG chunk exceeds the metadata area")
		}
		switch string(chunk[:4]) {
		case "DAOX":
			if trackCount != 0 {
				return fmt.Errorf("NRG contains multiple track tables")
			}
			trackCount, err = validateNRGTracks(io.NewSectionReader(file, at, length), dataEnd, limit)
			if err != nil {
				return err
			}
		case "DAOI", "ETNF", "ETN2":
			return fmt.Errorf("NRG requires a single DAOX track table")
		case "SINF":
			if length != 4 || sessionTracks != 0 {
				return fmt.Errorf("NRG requires a single session")
			}
			var session [4]byte
			if _, err := file.ReadAt(session[:], at); err != nil {
				return err
			}
			sessionTracks = binary.BigEndian.Uint32(session[:])
			if sessionTracks == 0 || sessionTracks > 99 {
				return fmt.Errorf("NRG has an invalid session track count")
			}
		case "END!":
			if length == 0 && at == footerAt && trackCount != 0 && (sessionTracks == 0 || sessionTracks == uint32(trackCount)) {
				return nil
			}
			return fmt.Errorf("NRG terminator or track table is inconsistent")
		}
		at += length
	}
	return fmt.Errorf("NRG metadata has too many chunks or lacks an end marker")
}

func validateNRGTracks(table *io.SectionReader, dataEnd uint64, limit int64) (int, error) {
	var header [22]byte
	if _, err := io.ReadFull(table, header[:]); err != nil {
		return 0, fmt.Errorf("read NRG track table: %w", err)
	}
	var record [42]byte
	tracks := int(header[21])
	if header[20] != 1 || tracks == 0 || tracks > 99 || table.Size() != int64(len(header)+tracks*len(record)) {
		return 0, fmt.Errorf("invalid NRG track numbering or table size")
	}
	var next uint64
	for track := 1; track <= tracks; track++ {
		if _, err := io.ReadFull(table, record[:]); err != nil {
			return 0, fmt.Errorf("read NRG track %d: %w", track, err)
		}
		sector := uint64(binary.BigEndian.Uint16(record[12:]))
		mode := binary.BigEndian.Uint16(record[14:])
		if !(mode == 0 && sector == 2048 || (mode == 0x0600 || mode == 0x0700) && sector == 2352) {
			return 0, fmt.Errorf("unsupported NRG sector format for track %d", track)
		}
		index0 := binary.BigEndian.Uint64(record[18:])
		index1 := binary.BigEndian.Uint64(record[26:])
		end := binary.BigEndian.Uint64(record[34:])
		if index0 < index1 {
			return 0, fmt.Errorf("chdman would discard the stored pregap for NRG track %d", track)
		}
		if index0 != next || index1 != index0 || end <= index1 || end > dataEnd || (end-index1)%sector != 0 {
			return 0, fmt.Errorf("NRG tracks must have consecutive and complete sector extents")
		}
		if err := chargeCDTrack(&limit, int64((end-index1)/sector)); err != nil {
			return 0, err
		}
		next = end
	}
	if next != dataEnd {
		return 0, fmt.Errorf("NRG tracks do not cover all data before the metadata")
	}
	return tracks, nil
}
