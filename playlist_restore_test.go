package romcompressor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func compressedPlaylistFixture(t *testing.T) (string, [][]byte, map[string][]byte) {
	t.Helper()
	source, payloads := playlistFixture(t)
	directory := filepath.Dir(source)
	originals := make(map[string][]byte)
	var lines []string
	for index, name := range []string{"first", "second"} {
		format := []Format{CSO, ZSO}[index]
		input := filepath.Join(directory, name, "disc.iso")
		packed := filepath.Join(directory, name, "disc."+string(format))
		if _, err := Convert(context.Background(), input, packed, Options{Format: format}); err != nil {
			t.Fatal(err)
		}
		originals[input] = payloads[index]
		data, err := os.ReadFile(packed)
		if err != nil {
			t.Fatal(err)
		}
		originals[packed] = data
		lines = append(lines, name+"/disc."+string(format))
	}
	contents := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(source, contents, 0600); err != nil {
		t.Fatal(err)
	}
	originals[source] = contents
	return source, payloads, originals
}

func assertPlaylistOriginals(t *testing.T, originals map[string][]byte) {
	t.Helper()
	for path, expected := range originals {
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, expected) {
			t.Fatalf("original changed at %q: %v", path, err)
		}
	}
}

func assertNoRestoredPlaylist(t *testing.T, output string) {
	t.Helper()
	for _, path := range []string{output, strings.TrimSuffix(output, filepath.Ext(output))} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("partial disc set published at %q: %v", path, err)
		}
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".romcompressor-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("disc-set staging leaked: %v, %v", stages, err)
	}
}

func TestPlaylistRestoresMixedImagesAndRawCopies(t *testing.T) {
	for _, progress := range []bool{false, true} {
		t.Run(map[bool]string{false: "without callback", true: "with callback"}[progress], func(t *testing.T) {
			source, payloads, originals := compressedPlaylistFixture(t)
			directory := filepath.Dir(source)
			firstSBI := filepath.Join(directory, "first", "disc.sBi")
			originals[firstSBI] = []byte("SBI\x00first generated sidecar")
			if err := os.WriteFile(firstSBI, originals[firstSBI], 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(directory, "third"), 0700); err != nil {
				t.Fatal(err)
			}
			third := filepath.Join(directory, "third", "disc.iso")
			payloads = append(payloads, bytes.Repeat([]byte{0x91}, 6144))
			originals[third] = payloads[2]
			if err := os.WriteFile(third, originals[third], 0600); err != nil {
				t.Fatal(err)
			}
			originals[source] = append(originals[source], []byte("third/disc.iso\n")...)
			if err := os.WriteFile(source, originals[source], 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(directory, "restored.m3u")
			opts := Options{Format: Raw}
			var copied bool
			if progress {
				opts.Progress = func(p Progress) { copied = copied || strings.Contains(p.Phase, "copying disc") }
			}
			result, err := Convert(context.Background(), source, output, opts)
			if err != nil || !result.Verified || result.Format != Raw {
				t.Fatalf("restore disc set = %+v, %v", result, err)
			}
			contents, err := os.ReadFile(output)
			want := "./restored/disc.iso\n./restored/disc-2.iso\n./restored/disc-3.iso\n"
			if err != nil || string(contents) != want {
				t.Fatalf("restored playlist = %q, %v", contents, err)
			}
			for index, name := range []string{"disc.iso", "disc-2.iso", "disc-3.iso"} {
				data, err := os.ReadFile(filepath.Join(directory, "restored", name))
				if err != nil || !bytes.Equal(data, payloads[index]) {
					t.Fatalf("restored disc %d changed: %v", index+1, err)
				}
			}
			data, err := os.ReadFile(filepath.Join(directory, "restored", "disc.sbi"))
			if err != nil || !bytes.Equal(data, originals[firstSBI]) {
				t.Fatalf("restored sidecar changed: %v", err)
			}
			if progress && !copied {
				t.Fatal("raw image copy had no progress")
			}
			if discs, _, err := readPlaylist(context.Background(), output); err != nil || len(discs) != 3 {
				t.Fatalf("replacement playlist is unusable: %v", err)
			}
			if _, err := ConvertPlaylist(context.Background(), source, output, opts); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing restored set was not protected: %v", err)
			}
			assertPlaylistOriginals(t, originals)
		})
	}
}

func TestPlaylistRestoresPBPTracksWithoutCompanionNameCollisions(t *testing.T) {
	directory := t.TempDir()
	packed, payload := testfixture.PBP(t)
	for _, name := range []string{"first", "second", "third"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	originals := map[string][]byte{
		filepath.Join(directory, "first", "disc.pbp"):  packed,
		filepath.Join(directory, "first", "disc.SbI"):  []byte("SBI\x00generated PBP sidecar"),
		filepath.Join(directory, "second", "disc.bin"): []byte("generated second raw disc"),
		filepath.Join(directory, "third", "disc.pbp"):  packed,
	}
	source := filepath.Join(directory, "source.m3u")
	originals[source] = []byte("first/disc.pbp\nsecond/disc.bin\nthird/disc.pbp\n")
	for path, data := range originals {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(directory, "restored.m3u8")
	result, err := ConvertPlaylist(context.Background(), source, output, Options{Format: Raw})
	if err != nil || !result.Verified {
		t.Fatalf("PBP set restoration = %+v, %v", result, err)
	}
	contents, err := os.ReadFile(output)
	if want := "./restored/disc.cue\n./restored/disc-2.bin\n./restored/disc-3.cue\n"; err != nil || string(contents) != want {
		t.Fatalf("mixed CUE/image playlist = %q, %v", contents, err)
	}
	for _, name := range []string{"disc", "disc-3"} {
		cue := filepath.Join(directory, "restored", name+".cue")
		var tracks int
		if err := disc.VisitReferences(cue, func(path string, _ os.FileInfo) error {
			tracks++
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, payload) {
				t.Fatalf("restored PBP track changed: %v", err)
			}
			return nil
		}); err != nil || tracks != 1 {
			t.Fatalf("CUE reference was not complete and usable: %v", err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(directory, "restored", "disc-2.bin")); err != nil || !bytes.Equal(data, originals[filepath.Join(directory, "second", "disc.bin")]) {
		t.Fatalf("raw disc collided with restored BIN: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "restored", "disc.sbi")); err != nil || !bytes.Equal(data, originals[filepath.Join(directory, "first", "disc.SbI")]) {
		t.Fatalf("PBP sidecar missing or changed: %v", err)
	}
	assertPlaylistOriginals(t, originals)
}

func TestPlaylistRestoresSingleImageECMEntries(t *testing.T) {
	directory := t.TempDir()
	payload := bytes.Repeat([]byte("generated single-image CD bytes"), 200)
	input := filepath.Join(directory, "source.bin")
	originals := map[string][]byte{input: payload}
	if err := os.WriteFile(input, payload, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disc.bin.ecm", "disc.ecm"} {
		path := filepath.Join(directory, name)
		if _, err := Convert(context.Background(), input, path, Options{Format: ECM}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		originals[path] = data
	}
	source := filepath.Join(directory, "source.m3u")
	originals[source] = []byte("disc.bin.ecm\ndisc.ecm\n")
	for path, data := range map[string][]byte{
		source: originals[source], filepath.Join(directory, "disc.bin.sBi"): []byte("SBI\x00generated ECM companion"),
	} {
		originals[path] = data
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(directory, "restored.m3u")
	if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: Raw}); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(output); err != nil || string(contents) != "./restored/disc.bin\n./restored/disc-2.bin\n" {
		t.Fatalf("single-image ECM playlist = %q, %v", contents, err)
	}
	for _, name := range []string{"disc.bin", "disc-2.bin"} {
		if contents, err := os.ReadFile(filepath.Join(directory, "restored", name)); err != nil || !bytes.Equal(contents, payload) {
			t.Fatalf("ECM restoration changed source bytes: %v", err)
		}
	}
	if contents, err := os.ReadFile(filepath.Join(directory, "restored", "disc.sbi")); err != nil || !bytes.Equal(contents, originals[filepath.Join(directory, "disc.bin.sBi")]) {
		t.Fatalf("ECM companion was omitted or changed: %v", err)
	}
	assertPlaylistOriginals(t, originals)
}

func TestPlaylistRejectsVerificationTempDir(t *testing.T) {
	source, _, originals := compressedPlaylistFixture(t)
	output := filepath.Join(filepath.Dir(source), "restored.m3u")
	if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: Raw, TempDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "standalone verification") {
		t.Fatalf("playlist silently ignored verification-only temp dir: %v", err)
	}
	assertNoRestoredPlaylist(t, output)
	assertPlaylistOriginals(t, originals)
}

func TestPlaylistRestoreFailuresPublishNoPartialSet(t *testing.T) {
	for _, scenario := range []string{"corrupt later disc", "set limit", "cancellation", "staged track mutation"} {
		t.Run(scenario, func(t *testing.T) {
			source, payloads, originals := compressedPlaylistFixture(t)
			directory := filepath.Dir(source)
			output := filepath.Join(directory, "restored.m3u")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := Options{Format: Raw}
			switch scenario {
			case "corrupt later disc":
				path := filepath.Join(directory, "second", "disc.zso")
				originals[path] = []byte("ZISOinvalid generated container")
				if err := os.WriteFile(path, originals[path], 0600); err != nil {
					t.Fatal(err)
				}
			case "set limit":
				opts.MaxOutput = int64(len(payloads[0]) + len(payloads[1]) - 1)
			case "cancellation", "staged track mutation":
				changed := false
				opts.Progress = func(p Progress) {
					if changed || !strings.HasPrefix(p.Phase, "disc 2/2 ") {
						return
					}
					changed = true
					if scenario == "cancellation" {
						cancel()
						return
					}
					stages, err := filepath.Glob(filepath.Join(directory, ".romcompressor-playlist-*"))
					if err != nil || len(stages) != 1 {
						t.Fatalf("find restore stage: %v, %v", stages, err)
					}
					path := filepath.Join(stages[0], "disc.iso")
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, bytes.Repeat([]byte{0x89}, int(info.Size())), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := ConvertPlaylist(ctx, source, output, opts); err == nil {
				t.Fatal("unsafe or incomplete restored set accepted")
			} else if scenario == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation identity: %v", err)
			}
			assertNoRestoredPlaylist(t, output)
			assertPlaylistOriginals(t, originals)
		})
	}
}

func TestPlaylistRestorationRechecksBINAndRollsBackLateCollision(t *testing.T) {
	for _, scenario := range []string{"BIN changed", "playlist collision", "complete set limit"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			packed, raw := testfixture.PBP(t)
			source := filepath.Join(directory, "source.m3u")
			image := filepath.Join(directory, "first.pbp")
			second := filepath.Join(directory, "second.iso")
			sidecar := filepath.Join(directory, "first.sBi")
			originals := map[string][]byte{
				source: []byte("first.pbp\nsecond.iso\n"), image: packed,
				second: []byte("generated raw disc"), sidecar: []byte("SBI\x00generated sidecar"),
			}
			for path, data := range originals {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(directory, "restored.m3u")
			opts := Options{Format: Raw}
			if scenario == "complete set limit" {
				result, err := ConvertPlaylist(context.Background(), source, output, opts)
				if err != nil || result.OutputBytes <= int64(len(raw)) {
					t.Fatalf("complete set omitted companions from result: %+v, %v", result, err)
				}
				output = filepath.Join(directory, "blockedx.m3u")
				opts.MaxOutput = result.OutputBytes - 1
			} else {
				changed := false
				opts.Progress = func(p Progress) {
					if changed || !strings.HasPrefix(p.Phase, "disc 2/2 ") {
						return
					}
					changed = true
					if scenario == "playlist collision" {
						if err := os.WriteFile(output, []byte("existing playlist"), 0600); err != nil {
							t.Fatal(err)
						}
						return
					}
					stages, err := filepath.Glob(filepath.Join(directory, ".romcompressor-playlist-*"))
					if err != nil || len(stages) != 1 {
						t.Fatalf("find restoration stage: %v, %v", stages, err)
					}
					path := filepath.Join(stages[0], "first.bin")
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, bytes.Repeat([]byte{0x58}, int(info.Size())), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := ConvertPlaylist(context.Background(), source, output, opts)
			if err == nil {
				t.Fatal("unsafe or incomplete CUE/BIN set published")
			}
			if scenario == "playlist collision" {
				if !errors.Is(err, os.ErrExist) {
					t.Fatalf("lost late collision identity: %v", err)
				}
				if data, readErr := os.ReadFile(output); readErr != nil || string(data) != "existing playlist" {
					t.Fatalf("existing playlist was changed: %q, %v", data, readErr)
				}
				if _, statErr := os.Lstat(filepath.Join(directory, "restored")); !os.IsNotExist(statErr) {
					t.Fatalf("late collision left published CUE/BIN/SBI files: %v", statErr)
				}
			} else {
				assertNoRestoredPlaylist(t, output)
			}
			assertPlaylistOriginals(t, originals)
		})
	}
}

func TestPlaylistRestoreRefusesIncompleteRawDescriptorCopies(t *testing.T) {
	for _, extension := range []string{".cue", ".gdi"} {
		t.Run(extension, func(t *testing.T) {
			directory := t.TempDir()
			descriptor := filepath.Join(directory, "disc"+extension)
			originals := map[string][]byte{descriptor: []byte("generated descriptor")}
			source := filepath.Join(directory, "source.m3u")
			originals[source] = []byte("disc" + extension + "\n")
			for path, data := range originals {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(directory, "restored.m3u")
			if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: Raw}); err == nil || !strings.Contains(err.Error(), "complete track sets") {
				t.Fatalf("raw descriptor was copied without its tracks: %v", err)
			}
			assertNoRestoredPlaylist(t, output)
			assertPlaylistOriginals(t, originals)
		})
	}
}

func TestPlaylistRestoreCHDMediaSelection(t *testing.T) {
	for _, test := range []struct {
		name, tag, media, message string
		dependency                bool
	}{
		{"tagged CD", "CHT2", "", "", true},
		{"tagged DVD", "DVD ", "", "", true},
		{"unknown", "", "", "requires media cd or dvd", false},
		{"explicit CD without track metadata", "", "cd", "CHD lacks cd media metadata", false},
		{"explicit untagged DVD", "", "dvd", "", true},
		{"conflict", "CHT2", "dvd", "conflicts", false},
		{"hard drive", "GDDD", "", "not supported in a disc playlist", false},
		{"laserdisc", "AVAV", "", "not supported in a disc playlist", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			var tags []string
			if test.tag != "" {
				tags = append(tags, test.tag)
			}
			image := testfixture.CHDMetadata(5, tags...)
			if test.tag == "CHT2" && test.dependency {
				image = testfixture.CDTrackHeader("TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:4 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0")
			}
			source := filepath.Join(directory, "source.m3u")
			originals := map[string][]byte{source: []byte("disc.chd\n"), filepath.Join(directory, "disc.chd"): image}
			for path, data := range originals {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(directory, "restored.m3u")
			_, err := ConvertPlaylist(context.Background(), source, output, Options{Format: Raw, Media: test.media, Tools: map[string]string{"chdman": filepath.Join(directory, "missing-engine")}})
			var dependency *DependencyError
			if test.dependency {
				if !errors.As(err, &dependency) || dependency.Engine != "chdman" {
					t.Fatalf("valid CHD selection did not reach normal dependency check: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.message) || errors.As(err, &dependency) {
				t.Fatalf("invalid CHD selection reached engine work: %v", err)
			}
			assertNoRestoredPlaylist(t, output)
			assertPlaylistOriginals(t, originals)
		})
	}
}
