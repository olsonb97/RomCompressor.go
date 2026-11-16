package codec

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrPBPWithoutDisc identifies an ordinary PBP with no PlayStation disc payload.
var ErrPBPWithoutDisc = errors.New("PBP does not contain a supported PS1 disc")

const (
	PBPBytesPerSector = 2352
	PBPLeadIn         = 150
	PBPMaxSectors     = 100*60*75 - PBPLeadIn - 1
	pbpBlockSize      = 16 * PBPBytesPerSector
	pbpTableOffset    = 0x4000
	pbpPayloadOffset  = 0x100000
	pbpTOCOffset      = 0x800
	pbpTOCEntrySize   = 10
)

// PBPTrack describes the stored INDEX 00 and INDEX 01 locations of one track.
// Index0 equals Index1 when there is no stored pregap.
type PBPTrack struct {
	Audio  bool
	Index0 int
	Index1 int
}

type pbpBlock struct {
	offset int64
	size   int
}

// PBPDisc describes one unencrypted, raw-sector PS1 disc in a PBP container.
type PBPDisc struct {
	Tracks  []PBPTrack
	Sectors int
	src     io.ReaderAt
	blocks  []pbpBlock
}

func readPBPHeader(src io.ReaderAt, size int64) (int64, []byte, error) {
	var header [40]byte
	if _, err := src.ReadAt(header[:], 0); err != nil {
		return 0, nil, fmt.Errorf("read PBP header: %w", err)
	}
	if string(header[:4]) != "\x00PBP" {
		return 0, nil, fmt.Errorf("invalid PBP signature")
	}
	previous := int64(len(header))
	for field := 8; field < len(header); field += 4 {
		offset := int64(binary.LittleEndian.Uint32(header[field:]))
		if offset < previous || offset > size {
			return 0, nil, fmt.Errorf("invalid PBP section offsets")
		}
		previous = offset
	}
	psar := previous
	if size-psar < 12 {
		return 0, nil, ErrPBPWithoutDisc
	}
	var magic [16]byte
	if _, err := src.ReadAt(magic[:min(int64(len(magic)), size-psar)], psar); err != nil {
		return 0, nil, err
	}
	if string(magic[:]) == "PSTITLEIMG000000" {
		return 0, nil, fmt.Errorf("multi-disc PBP containers are unsupported")
	}
	if string(magic[:12]) != "PSISOIMG0000" {
		return 0, nil, ErrPBPWithoutDisc
	}
	if size-psar <= pbpPayloadOffset || size > int64(^uint32(0)) {
		return 0, nil, fmt.Errorf("PBP disc payload size is out of bounds")
	}
	metadata := make([]byte, 0xc00)
	if _, err := src.ReadAt(metadata, psar); err != nil {
		return 0, nil, err
	}
	if string(metadata[0x400:0x404]) == "\x00PGD" {
		return 0, nil, fmt.Errorf("encrypted PS1 PBP discs are unsupported")
	}
	if binary.LittleEndian.Uint32(metadata[0xbfc:]) != pbpPayloadOffset {
		return 0, nil, fmt.Errorf("PBP uses an unsupported disc block layout")
	}
	return psar, metadata[pbpTOCOffset:0xbfc], nil
}

// ReadPBP validates the single-disc header, TOC, and bounded block directory.
func ReadPBP(ctx context.Context, src io.ReaderAt, size int64) (*PBPDisc, error) {
	metadata := contextReaderAt{ReaderAt: src, ctx: ctx}
	psar, toc, err := readPBPHeader(metadata, size)
	if err != nil {
		return nil, err
	}
	tracks, sectors, err := readPBPTracks(toc)
	if err != nil {
		return nil, err
	}
	count := (sectors + 15) / 16
	blocks, err := readPBPBlocks(metadata, psar, size, count)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &PBPDisc{Tracks: tracks, Sectors: sectors, src: src, blocks: blocks}, nil
}

func pbpTrackCount(toc []byte) (int, error) {
	if toc[2] != 0xa0 || toc[7] != 1 || toc[12] != 0xa1 || toc[22] != 0xa2 {
		return 0, fmt.Errorf("invalid PBP table of contents")
	}
	count := decodePBPBCD(toc[17])
	if count < 1 || (count+3)*pbpTOCEntrySize > len(toc) {
		return 0, fmt.Errorf("invalid PBP track count")
	}
	return count, nil
}

func readPBPTracks(toc []byte) ([]PBPTrack, int, error) {
	count, err := pbpTrackCount(toc)
	if err != nil {
		return nil, 0, err
	}
	sectors := decodePBPTime(toc[27:30])
	if sectors < 1 {
		return nil, 0, fmt.Errorf("invalid PBP disc sector count")
	}
	tracks := make([]PBPTrack, 0, count)
	for i := range count {
		t := toc[(i+3)*pbpTOCEntrySize:][:pbpTOCEntrySize]
		start := decodePBPTime(t[7:10])
		pregap := decodePBPTime(t[3:6])
		if t[2] != encodePBPBCD(i+1) || (t[0] != 0x41 && t[0] != 0x01) || start < PBPLeadIn || pregap < 0 {
			return nil, 0, fmt.Errorf("invalid PBP track %d", i+1)
		}
		if i == 0 {
			if t[0] != 0x41 || start != PBPLeadIn || (pregap != 0 && pregap <= start) {
				return nil, 0, fmt.Errorf("unsupported first PBP track layout")
			}
			// Older encoders use a pregap address after INDEX 01 to indicate an
			// unstored lead-in and give the lead-out as an absolute disc address.
			if pregap > start {
				sectors -= PBPLeadIn
			}
			if sectors < 1 || sectors > PBPMaxSectors {
				return nil, 0, fmt.Errorf("invalid PBP disc sector count")
			}
			pregap = PBPLeadIn
		} else if pregap < PBPLeadIn || t[0] != 0x01 {
			return nil, 0, fmt.Errorf("unsupported PBP track %d layout", i+1)
		}
		track := PBPTrack{Audio: t[0] == 0x01, Index0: pregap - PBPLeadIn, Index1: start - PBPLeadIn}
		if track.Index0 > track.Index1 || track.Index1 >= sectors || (i > 0 && track.Index0 <= tracks[i-1].Index1) {
			return nil, 0, fmt.Errorf("invalid PBP track %d position", i+1)
		}
		tracks = append(tracks, track)
	}
	return tracks, sectors, nil
}

func readPBPBlocks(src io.ReaderAt, psar, size int64, count int) ([]pbpBlock, error) {
	dataStart := psar + pbpPayloadOffset
	const indexSize = 32
	sections := io.NewSectionReader(src, psar+pbpTableOffset, pbpPayloadOffset-pbpTableOffset)
	reader := bufio.NewReaderSize(sections, 64<<10)
	blocks := make([]pbpBlock, count)
	var index [indexSize]byte
	lastEnd := dataStart
	for i := range (pbpPayloadOffset - pbpTableOffset) / indexSize {
		if _, err := io.ReadFull(reader, index[:]); err != nil {
			return nil, fmt.Errorf("read PBP block directory: %w", err)
		}
		offset := int64(binary.LittleEndian.Uint32(index[:4]))
		length := int64(binary.LittleEndian.Uint16(index[4:6]))
		if i >= count {
			if offset != 0 || length != 0 {
				return nil, fmt.Errorf("PBP block directory exceeds the declared disc size")
			}
			continue
		}
		start := dataStart + offset
		if length < 1 || start < lastEnd || start > size || length > size-start {
			return nil, fmt.Errorf("invalid PBP block %d extent", i)
		}
		blocks[i] = pbpBlock{offset: start, size: int(length)}
		lastEnd = start + length
	}
	return blocks, nil
}

// SetPBPPregaps corrects an encoder's track pregaps from the original CUE.
// The encoder must already have produced the same lead-out, types, and INDEX 01.
func SetPBPPregaps(out interface {
	io.ReaderAt
	io.WriterAt
}, size int64, tracks []PBPTrack, sectors int) error {
	psar, toc, err := readPBPHeader(out, size)
	if err != nil {
		return err
	}
	count, err := pbpTrackCount(toc)
	if err != nil {
		return err
	}
	if count != len(tracks) || decodePBPTime(toc[27:30]) != sectors {
		return fmt.Errorf("PSXPackager changed the disc's track count or sector count")
	}
	for i, track := range tracks {
		t := toc[(i+3)*pbpTOCEntrySize:][:pbpTOCEntrySize]
		trackType := byte(0x41)
		if track.Audio {
			trackType = 0x01
		}
		if t[0] != trackType || t[2] != encodePBPBCD(i+1) || decodePBPTime(t[7:10]) != track.Index1+PBPLeadIn {
			return fmt.Errorf("PSXPackager changed the disc's track %d", i+1)
		}
		if i != 0 {
			encodePBPTime(t[3:6], track.Index0+PBPLeadIn)
		}
	}
	_, err = out.WriteAt(toc[:(count+3)*pbpTOCEntrySize], psar+pbpTOCOffset)
	return err
}

func decodePBPBCD(value byte) int {
	upper, lower := int(value>>4), int(value&15)
	if upper > 9 || lower > 9 {
		return -1
	}
	return 10*upper + lower
}

func decodePBPTime(msf []byte) int {
	m, s, f := decodePBPBCD(msf[0]), decodePBPBCD(msf[1]), decodePBPBCD(msf[2])
	if m < 0 || s < 0 || s >= 60 || f < 0 || f >= 75 {
		return -1
	}
	return (m*60+s)*75 + f
}

func encodePBPBCD(value int) byte { return byte(value/10<<4 | value%10) }

func encodePBPTime(msf []byte, frame int) {
	msf[0] = encodePBPBCD(frame / (60 * 75))
	msf[1] = encodePBPBCD(frame / 75 % 60)
	msf[2] = encodePBPBCD(frame % 75)
}

// Decode writes every declared disc sector and validates DEFLATE block bounds.
// The format does not reliably store a digest for the complete disc.
func (d *PBPDisc) Decode(ctx context.Context, dst io.Writer, opts Options) error {
	total := int64(d.Sectors) * PBPBytesPerSector
	opts, err := normalize(ctx, total, opts)
	if err != nil {
		return err
	}
	if total > opts.MaxOutput {
		return fmt.Errorf("expanded PBP disc exceeds output size limit")
	}
	maxPackedSize := pbpBlockSize
	for _, block := range d.blocks {
		maxPackedSize = max(maxPackedSize, block.size)
	}
	packed := make([]byte, maxPackedSize)
	decoded := make([]byte, pbpBlockSize)
	var compressed bytes.Reader
	decoder := flate.NewReader(&compressed)
	defer decoder.Close()
	var done int64
	for i, block := range d.blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		data := packed[:block.size]
		if _, err := d.src.ReadAt(data, block.offset); err != nil {
			return fmt.Errorf("read PBP block %d: %w", i, err)
		}
		// Only a stored block exactly the raw block size bypasses DEFLATE.
		if block.size != pbpBlockSize {
			compressed.Reset(data)
			if err := decoder.(flate.Resetter).Reset(&compressed, nil); err != nil {
				return err
			}
			if _, err := io.ReadFull(decoder, decoded); err != nil {
				return fmt.Errorf("decode PBP block %d: %w", i, err)
			}
			var extra [1]byte
			if n, err := decoder.Read(extra[:]); n != 0 || err != io.EOF || compressed.Len() != 0 {
				return fmt.Errorf("PBP block %d has excess or invalid DEFLATE data", i)
			}
			data = decoded
		}
		if i == 0 && (string(data[:12]) != "\x00\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\x00" || data[15] != 2) {
			return fmt.Errorf("PBP data track must contain raw MODE2/2352 sectors")
		}
		length := min(int64(pbpBlockSize), total-done)
		for _, value := range data[length:] {
			if value != 0 {
				return fmt.Errorf("PBP disc has nonzero data beyond its lead-out")
			}
		}
		n, err := dst.Write(data[:length])
		if err != nil {
			return err
		}
		if int64(n) != length {
			return io.ErrShortWrite
		}
		done += length
		if opts.Progress != nil {
			opts.Progress(done, total)
		}
	}
	return ctx.Err()
}
