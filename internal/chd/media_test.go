package chd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestCDTrackTablePrecedence(t *testing.T) {
	data := testfixture.CDTrackHeader("unused CHT2 fallback at track one")
	last := 124
	appendRecord := func(tag, value string) {
		next := len(data)
		binary.BigEndian.PutUint64(data[last+8:], uint64(next))
		var header [16]byte
		copy(header[:], tag)
		binary.BigEndian.PutUint32(header[4:], uint32(len(value)+1))
		data = append(data, header[:]...)
		data = append(data, value...)
		data = append(data, 0)
		last = next
	}
	appendRecord("CHTR", "TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:4")
	appendRecord("CHGD", "unused GD-ROM fallback at track one")
	path := filepath.Join(t.TempDir(), "mixed-metadata.chd")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := cdExtractionLayout(t.Context(), path, testMaxOutput)
	if err != nil || len(layout.tracks) != 1 || layout.tracks[0].mode != "AUDIO" {
		t.Fatalf("expected the first CHTR record: %+v, %v", layout, err)
	}
	appendRecord("CHGD", "active GD-ROM track two")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cdExtractionLayout(t.Context(), path, testMaxOutput); err == nil {
		t.Fatal("offered a single BIN for a disc with an active GD-ROM track")
	}
}

func TestMediaReadsOnlyExplicitTagsAcrossCHDVersions(t *testing.T) {
	for _, version := range []uint32{3, 4, 5} {
		for tag, want := range map[string]string{
			"CHCD": "cd", "CHTR": "cd", "CHT2": "cd", "CHSE": "cd", "CHGT": "cd", "CHGD": "cd",
			"DVD ": "dvd", "GDDD": "hdd", "AVAV": "laserdisc", "AVLD": "laserdisc", "TEST": "", "": "",
		} {
			t.Run(fmt.Sprintf("v%d/%s", version, tag), func(t *testing.T) {
				var tags []string
				if tag != "" {
					tags = []string{tag, tag}
				}
				data := testfixture.CHDMetadata(version, tags...)
				got, err := Media(context.Background(), bytes.NewReader(data), int64(len(data)))
				if err != nil || got != want {
					t.Fatalf("Media = %q, %v; want %q", got, err, want)
				}
			})
		}
	}
}

func TestMediaRejectsConflictingAndMalformedMetadata(t *testing.T) {
	for _, scenario := range []string{"conflict", "cycle", "overlap", "truncated", "too many records", "bad header"} {
		t.Run(scenario, func(t *testing.T) {
			data := testfixture.CHDMetadata(5, "CHT2", "CHTR")
			switch scenario {
			case "conflict":
				copy(data[141:], "GDDD")
			case "cycle":
				binary.BigEndian.PutUint64(data[149:157], 124)
			case "overlap":
				data[131] = 2
			case "truncated":
				data = data[:len(data)-1]
			case "too many records":
				tags := make([]string, 1025)
				data = testfixture.CHDMetadata(5, tags...)
			case "bad header":
				data[59] = 0
				data[58] = 0
			}
			if _, err := Media(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
				t.Fatal("invalid metadata was accepted")
			}
		})
	}
}

type cancelMetadataReader struct {
	*bytes.Reader
	cancel context.CancelFunc
}

func (r cancelMetadataReader) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.Reader.ReadAt(p, offset)
	if offset >= chdV5HeaderSize {
		r.cancel()
	}
	return n, err
}

func TestMediaRespectsCancellationDuringMetadataWalk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := testfixture.CHDMetadata(5, "CHT2", "CHTR")
	reader := cancelMetadataReader{bytes.NewReader(data), cancel}
	if _, err := Media(ctx, reader, int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Fatalf("metadata walk ignored cancellation: %v", err)
	}
}
