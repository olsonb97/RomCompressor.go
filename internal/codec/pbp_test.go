package codec

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestPBPDecode(t *testing.T) {
	packed, raw := testfixture.PBP(t)
	disc, err := ReadPBP(context.Background(), bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err := disc.Decode(context.Background(), &actual, Options{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, actual.Bytes()) || len(disc.Tracks) != 1 || disc.Tracks[0] != (PBPTrack{}) {
		t.Fatal("incorrect PBP disc content or track layout")
	}
	if err := disc.Decode(context.Background(), io.Discard, Options{MaxOutput: int64(len(raw) - 1)}); err == nil {
		t.Fatal("PBP decoder exceeded its output limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := disc.Decode(ctx, io.Discard, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PBP decode returned %v", err)
	}
}

func TestPBPStopsReadingMetadataAfterCancellation(t *testing.T) {
	packed, _ := testfixture.PBP(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The fourth read is the first page of the block index.
	reader := &cancellingReaderAt{ReaderAt: bytes.NewReader(packed), cancel: cancel, cancelAfter: 4}
	_, err := ReadPBP(ctx, reader, int64(len(packed)))
	if !errors.Is(err, context.Canceled) || reader.reads != reader.cancelAfter {
		t.Fatalf("PBP metadata scan made %d reads, returned %v", reader.reads, err)
	}
}

func TestPBPReadsLeadOutWithUnstoredLeadIn(t *testing.T) {
	const psar = 0x80
	packed, raw := testfixture.PBP(t)
	toc := packed[psar+pbpTOCOffset:]
	encodePBPTime(toc[27:30], 17+PBPLeadIn)
	encodePBPTime(toc[33:36], PBPLeadIn+1)
	disc, err := ReadPBP(context.Background(), bytes.NewReader(packed), int64(len(packed)))
	if err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err := disc.Decode(context.Background(), &actual, Options{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, actual.Bytes()) {
		t.Fatal("absolute lead-out changed the restored disc")
	}
}

func TestPBPReadsCompressedBlockLargerThanRaw(t *testing.T) {
	const psar = 0x80
	const payload = psar + pbpPayloadOffset
	const table = psar + pbpTableOffset
	for _, extra := range []int{0, 1} {
		name := "exact expansion"
		if extra != 0 {
			name = "excess expansion"
		}
		t.Run(name, func(t *testing.T) {
			packed, raw := testfixture.PBP(t)
			var deflated bytes.Buffer
			writer, err := flate.NewWriter(&deflated, flate.NoCompression)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(raw[:pbpBlockSize+extra]); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if deflated.Len() <= pbpBlockSize {
				t.Fatal("fixture does not encode a larger DEFLATE block")
			}
			second := int(binary.LittleEndian.Uint32(packed[table+32:]))
			updated := make([]byte, payload+deflated.Len()+len(packed)-(payload+second))
			copy(updated, packed[:payload])
			copy(updated[payload:], deflated.Bytes())
			copy(updated[payload+deflated.Len():], packed[payload+second:])
			binary.LittleEndian.PutUint16(updated[table+4:], uint16(deflated.Len()))
			binary.LittleEndian.PutUint32(updated[table+32:], uint32(deflated.Len()))
			disc, err := ReadPBP(context.Background(), bytes.NewReader(updated), int64(len(updated)))
			if err != nil {
				t.Fatal(err)
			}
			var actual bytes.Buffer
			err = disc.Decode(context.Background(), &actual, Options{})
			if extra != 0 {
				if err == nil || actual.Len() != 0 {
					t.Fatal("accepted or wrote a block with excess expansion")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, actual.Bytes()) {
				t.Fatal("decoded disc differs from the original")
			}
		})
	}
}

func TestPBPRejectsMalformedBlocks(t *testing.T) {
	const psar = 0x80
	cases := []struct {
		name      string
		malformed func([]byte)
		decode    bool
	}{
		{"PSP payload", func(b []byte) { copy(b[psar:], "PSP payload!") }, false},
		{"multidisc", func(b []byte) { copy(b[psar:], "PSTITLEIMG000000") }, false},
		{"encrypted", func(b []byte) { copy(b[psar+0x400:], "\x00PGD") }, false},
		{"invalid BCD", func(b []byte) { b[psar+pbpTOCOffset+27] = 0xfa }, false},
		{"block overlap", func(b []byte) { binary.LittleEndian.PutUint32(b[psar+pbpTableOffset+32:], 0) }, false},
		{"unaccounted block", func(b []byte) { binary.LittleEndian.PutUint32(b[psar+pbpTableOffset+68:], 5) }, false},
		{"invalid DEFLATE block", func(b []byte) { b[psar+pbpPayloadOffset] = 7 }, true},
		{"nonzero tail padding", func(b []byte) { b[len(b)-1] = 1 }, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			packed, _ := testfixture.PBP(t)
			test.malformed(packed)
			disc, err := ReadPBP(context.Background(), bytes.NewReader(packed), int64(len(packed)))
			if test.decode {
				if err != nil {
					t.Fatal(err)
				}
				err = disc.Decode(context.Background(), io.Discard, Options{})
			}
			if err == nil {
				t.Fatal("accepted a malformed PBP")
			}
			if test.name != "PSP payload" && errors.Is(err, ErrPBPWithoutDisc) {
				t.Fatal("misclassified a PlayStation disc as an ordinary PBP")
			}
		})
	}
}
