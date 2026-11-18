package integration_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rc "github.com/olsonb97/romcompressor.go"
)

func TestGDROMSplitTrackRestoration(t *testing.T) {
	tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
	if tool == "" {
		t.Skip("set ROMCOMPRESSOR_CHDMAN to exercise Dreamcast restoration")
	}
	ctx := t.Context()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	source := write("source.gdi", []byte("3\n1 0 4 2048 a.bin 0\n2 80 0 2352 b.bin 0\n3 45000 4 2352 c.bin 0\n"))
	var originals [][]byte
	for i, sector := range []int{2048, 2352, 2352} {
		data := make([]byte, (65+i)*sector)
		for j := range data {
			data[j] = byte(i*13 + j*19 + j/11)
		}
		write([]string{"a.bin", "b.bin", "c.bin"}[i], data)
		originals = append(originals, data)
	}
	opts := rc.Options{Format: rc.CHD, Tools: map[string]string{"chdman": tool}}
	image := filepath.Join(dir, "Disc.chd")
	if got, err := rc.Convert(ctx, source, image, opts); err != nil || !got.Verified {
		t.Fatalf("create GD-ROM CHD: %+v, %v", got, err)
	}
	plan, err := rc.PlanConversions(ctx, image, opts)
	if err != nil {
		t.Fatal(err)
	}
	var planned bool
	for _, target := range plan.Targets {
		if target.Format == rc.Raw && target.Media == "cd" {
			planned = target.Available && target.Extension == ".gdi"
		}
	}
	if !planned {
		t.Fatal("planner missed split-track GDI restoration")
	}
	runDir := t.TempDir()
	name := "Disc %02t"
	check := func(gdi string, stem string) {
		t.Helper()
		contents, err := os.ReadFile(gdi)
		if err != nil {
			t.Fatal(err)
		}
		want := "3\n"
		for i, geometry := range []string{"1 0 4 2048", "2 80 0 2352", "3 45000 4 2352"} {
			track := fmt.Sprintf("%s (Track %02d).bin", stem, i+1)
			want += geometry + " \"" + track + "\" 0\n"
			data, err := os.ReadFile(filepath.Join(filepath.Dir(gdi), track))
			if err != nil || !bytes.Equal(data, originals[i]) {
				t.Fatalf("restored track %d differs: %v", i+1, err)
			}
		}
		if string(contents) != want {
			t.Fatalf("restored GDI geometry or references differ: %q", contents)
		}
	}
	opts.Format, opts.Media = rc.Raw, "cd"
	out := filepath.Join(runDir, name+".gdi")
	conflict := filepath.Join(runDir, name+" (Track 02).bin")
	if err := os.WriteFile(conflict, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Convert(ctx, image, out, opts); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing track: %v", err)
	}
	if entries, err := os.ReadDir(runDir); err != nil || len(entries) != 1 {
		t.Fatalf("collision left partial outputs: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(conflict); err != nil || string(data) != "existing" {
		t.Fatalf("replaced existing track: %v", err)
	}
	out = filepath.Join(t.TempDir(), name+".gdi")
	if got, err := rc.Convert(ctx, image, out, opts); err != nil || !got.Verified {
		t.Fatalf("direct restoration: %+v, %v", got, err)
	}
	check(out, name)
	// The first raw disc reserves the default track-one stem. The restored
	// GD-ROM needs another stem for its descriptor and all three track files.
	write("Disc (Track 01).bin", []byte("separate disc"))
	write("Disc-2 (Track 03).bin", []byte("another disc"))
	list := write("input.m3u", []byte("Disc (Track 01).bin\nDisc.chd\nDisc-2 (Track 03).bin\n"))
	playlist := filepath.Join(t.TempDir(), "set.m3u")
	opts.Media = ""
	if got, err := rc.ConvertPlaylist(ctx, list, playlist, opts); err != nil || !got.Verified {
		t.Fatalf("playlist restoration: %+v, %v", got, err)
	}
	data, err := os.ReadFile(playlist)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || lines[0] != "./set/Disc (Track 01).bin" || lines[1] != "./set/Disc-2.gdi" || lines[2] != "./set/Disc-2 (Track 03)-2.bin" {
		t.Fatalf("unexpected playlist: %q", data)
	}
	check(filepath.Join(filepath.Dir(playlist), filepath.FromSlash(lines[1])), "Disc-2")
	for index, expected := range map[int]string{0: "separate disc", 2: "another disc"} {
		actual, err := os.ReadFile(filepath.Join(filepath.Dir(playlist), filepath.FromSlash(lines[index])))
		if err != nil || string(actual) != expected {
			t.Fatalf("playlist entry %d changed: %v", index, err)
		}
	}
}
