package integration_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/chd"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestCHDCUERefusesLossyDreamcastConversion(t *testing.T) {
	const single = "REM SINGLE-DENSITY AREA\n"
	const high = "REM HIGH-DENSITY AREA\n"
	const lowDensity = "FILE low.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n" +
		"FILE audio.bin BINARY\n TRACK 02 AUDIO\n INDEX 00 00:00:00\n INDEX 01 00:00:02\n"
	const highDensity = "FILE high.bin BINARY\n TRACK 03 MODE1/2048\n INDEX 01 00:00:00\n"
	for _, tc := range []struct {
		name, first, second string
		refuse              bool
	}{
		{"both markers", single, high, true},
		{"newer engine marker syntax", "\t'REM' \tSINGLE-DENSITY AREA note\n", "  REM\tHIGH-DENSITY AREA\n", true},
		{"single marker", single, "", false},
		{"lowercase comment", single, "rem HIGH-DENSITY AREA\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			descriptor := []byte(tc.first + lowDensity + tc.second + highDensity)
			low, audio, high := make([]byte, 32*2048), make([]byte, 34*2352), make([]byte, 32*2048)
			random := rand.New(rand.NewSource(3))
			for _, track := range [][]byte{low, audio, high} {
				_, _ = random.Read(track)
			}
			inputs := map[string][]byte{"disc.cue": descriptor, "low.bin": low, "audio.bin": audio, "high.bin": high}
			for name, data := range inputs {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cue := filepath.Join(dir, "disc.cue")
			check := func(err error) {
				t.Helper()
				if tc.refuse {
					if err == nil || !strings.Contains(err.Error(), "Dreamcast multi-CUE") {
						t.Fatalf("expected Dreamcast multi-CUE refusal, got %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			check(chd.ValidateCUE(cue, "", kind.DefaultMaxOutput))
			tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
			if tool == "" {
				return
			}
			ctx := context.Background()
			output := filepath.Join(dir, "disc.chd")
			opts := romcompressor.Options{Format: romcompressor.CHD, Tools: map[string]string{"chdman": tool}}
			_, err := romcompressor.Convert(ctx, cue, output, opts)
			check(err)
			if tc.refuse {
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatalf("refused conversion left output: %v", err)
				}
			} else {
				opts.Format, opts.Media = romcompressor.Raw, "cd"
				if _, err := romcompressor.Convert(ctx, output, filepath.Join(dir, "restored.cue"), opts); err != nil {
					t.Fatal(err)
				}
				restored, err := os.ReadFile(filepath.Join(dir, "restored.bin"))
				if err != nil || !bytes.Equal(restored, bytes.Join([][]byte{low, audio, high}, nil)) {
					t.Fatalf("restored track bytes changed: %v", err)
				}
			}
			for name, data := range inputs {
				if original, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(original, data) {
					t.Fatalf("original %s changed: %v", name, err)
				}
			}
		})
	}
}

func TestCHDCUEPreservesBinaryFileBoundaries(t *testing.T) {
	const first = "FILE first.bin BINARY\n TRACK 01 MODE1/2352\n INDEX 01 00:00:00\n"
	const second = "FILE second.bin BINARY\n TRACK 02 AUDIO\n INDEX 01 00:00:00\n"
	const third = "FILE third.bin BINARY\n TRACK 03 AUDIO\n INDEX 01 00:00:00\n"
	const audio2 = " TRACK 02 AUDIO\n INDEX 01 00:00:02\n"
	const audio3At4 = " TRACK 03 AUDIO\n INDEX 01 00:00:04\n"
	const audio3At6 = " TRACK 03 AUDIO\n INDEX 01 00:00:06\n"
	const sector = 2352
	data := make([]byte, 10*sector)
	for i := range data {
		data[i] = byte(i/sector) ^ byte(i%251)
	}
	for _, tc := range []struct {
		name, cue string
		first     []byte
		second    []byte
		third     []byte
		refuse    bool
	}{
		{"shared second file", first + second + audio3At4, data[:2*sector], data[2*sector:], nil, true},
		{"shared first file", first + audio2 + third, data[:6*sector], nil, data[6*sector:], true},
		{"one file per track", first + second + third, data[:2*sector], data[2*sector : 6*sector], data[6*sector:], false},
		{"single shared file", first + audio2 + audio3At6, data, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "disc.cue")
			files := map[string][]byte{"disc.cue": []byte(tc.cue), "first.bin": tc.first, "second.bin": tc.second, "third.bin": tc.third}
			for name, contents := range files {
				if contents != nil {
					if err := os.WriteFile(filepath.Join(dir, name), contents, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			check := func(err error) {
				t.Helper()
				if tc.refuse && (err == nil || !strings.Contains(err.Error(), "shared BIN")) || !tc.refuse && err != nil {
					t.Fatalf("validation = %v, want refusal = %t", err, tc.refuse)
				}
			}
			check(chd.ValidateCUE(source, "", kind.DefaultMaxOutput))
			tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
			if tool == "" {
				return
			}
			expected := data
			if !tc.refuse {
				last := "first.bin"
				if tc.second != nil {
					last = "second.bin"
				}
				if tc.third != nil {
					last = "third.bin"
				}
				files[last] = testfixture.CHDTrack(files[last], sector)
				if err := os.WriteFile(filepath.Join(dir, last), files[last], 0600); err != nil {
					t.Fatal(err)
				}
				expected = bytes.Join([][]byte{files["first.bin"], files["second.bin"], files["third.bin"]}, nil)
			}
			ctx := context.Background()
			chd := filepath.Join(dir, "disc.chd")
			opts := romcompressor.Options{Format: romcompressor.CHD, Tools: map[string]string{"chdman": tool}}
			_, err := romcompressor.Convert(ctx, source, chd, opts)
			check(err)
			if tc.refuse {
				if _, err := os.Lstat(chd); !os.IsNotExist(err) {
					t.Fatalf("refused conversion left an output: %v", err)
				}
			} else {
				opts.Format, opts.Media = romcompressor.Raw, "cd"
				if _, err := romcompressor.Convert(ctx, chd, filepath.Join(dir, "restored.cue"), opts); err != nil {
					t.Fatal(err)
				}
				restored, err := os.ReadFile(filepath.Join(dir, "restored.bin"))
				if err != nil || !bytes.Equal(restored, expected) {
					t.Fatalf("restored sectors differ: %v", err)
				}
			}
			for name, contents := range files {
				if contents != nil {
					if actual, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(actual, contents) {
						t.Fatalf("original %s changed: %v", name, err)
					}
				}
			}
		})
	}
}

func TestCHDRefusesAmbiguousTrackReferences(t *testing.T) {
	for _, tc := range []struct {
		name, ext, layout, selected, alternate string
		want                                   string
	}{
		{
			name: "quoted GDI name with repeated spaces", ext: ".gdi",
			layout: "1\n1 0 4 2048 \"track  image.bin\" 0\n", selected: "track  image.bin", alternate: "track image.bin",
			want: "ambiguous GDI track filename",
		},
		{
			name: "quoted GDI name without spaces", ext: ".gdi",
			layout: "1\n1 0 4 2048 \"track.bin\" 0\n", selected: "track.bin",
			want: "ambiguous GDI track filename",
		},
		{
			name: "GDI tab separator", ext: ".gdi",
			layout: "1\n1\t0 4 2048 track.bin 0\n", selected: "track.bin",
			want: "GDI track lines",
		},
		{
			name: "GDI blank line before track", ext: ".gdi",
			layout: "1\n\n1 0 4 2048 track.bin 0\n", selected: "track.bin",
			want: "GDI track lines",
		},
		{
			name: "ordinary quoted GDI name", ext: ".gdi",
			layout: "1\n1 0 4 2048 \"track image.bin\" 0\n", selected: "track image.bin",
		},
		{
			name: "nonzero first-track LBA", ext: ".gdi",
			layout: "1\n1 150 4 2048 track.bin 0\n", selected: "track.bin", want: "nonzero first-track LBA",
		},
		{
			name: "single quotes on a bare CUE name", ext: ".cue",
			layout:   "FILE 'track.bin' BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n",
			selected: "'track.bin'", alternate: "track.bin", want: "ambiguous CUE track filename",
		},
		{
			name: "literal apostrophe inside double quotes", ext: ".cue",
			layout:   "FILE \"track's image.bin\" BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n",
			selected: "track's image.bin",
		},
		{
			name: "nonbreaking space inside a quoted CUE name", ext: ".cue",
			layout:   "FILE \"track\u00a0image.bin\" BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n",
			selected: "track\u00a0image.bin",
		},
		{
			name: "lowercase CUE postgap is ignored by chdman", ext: ".cue",
			layout:   "FILE track.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n postgap 00:00:03\n",
			selected: "track.bin", want: "POSTGAP directive must be uppercase",
		},
		{
			name: "unterminated CUE postgap is ignored by chdman", ext: ".cue",
			layout:   "FILE track.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n POSTGAP 00:00:03",
			selected: "track.bin", want: "end with a newline",
		},
		{
			name: "CUE postgap with nonbreaking indentation is ignored by chdman", ext: ".cue",
			layout:   "FILE track.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n\u00a0POSTGAP 00:00:03\n",
			selected: "track.bin", want: "POSTGAP directive requires ASCII whitespace",
		},
		{
			name: "CUE postgap with a nonbreaking separator is ignored by chdman", ext: ".cue",
			layout:   "FILE track.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n POSTGAP\u00a000:00:03\n",
			selected: "track.bin", want: "POSTGAP directive requires ASCII whitespace",
		},
		{
			name: "quoted CUE directive hides a track from reference validation", ext: ".cue",
			layout:   "FILE track.bin BINARY\nF'I'LE ../outside.bin BINARY\n TRACK 01 MODE1/2048\n INDEX 01 00:00:00\n",
			selected: "track.bin", want: "FILE directive must be uppercase, unquoted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "disc"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.layout), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.selected), bytes.Repeat([]byte{1}, 2048), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.alternate != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.alternate), bytes.Repeat([]byte{2}, 2048), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if tc.ext == ".gdi" {
				err = chd.ValidateGDI(path, kind.DefaultMaxOutput)
			} else {
				err = chd.ValidateCUE(path, "", kind.DefaultMaxOutput)
			}
			checkError := func(err error) {
				t.Helper()
				if tc.want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("validation = %v, want %q", err, tc.want)
				}
			}
			checkError(err)
			if tool := os.Getenv("ROMCOMPRESSOR_CHDMAN"); tool != "" {
				if tc.want == "" {
					track := testfixture.CHDTrack(bytes.Repeat([]byte{1}, 2048), 2048)
					if err := os.WriteFile(filepath.Join(dir, tc.selected), track, 0600); err != nil {
						t.Fatal(err)
					}
				}
				output := filepath.Join(dir, "disc.chd")
				result, err := romcompressor.Convert(context.Background(), path, output, romcompressor.Options{
					Format: romcompressor.CHD, Tools: map[string]string{"chdman": tool},
				})
				checkError(err)
				if tc.want == "" && !result.Verified {
					t.Fatal("conversion did not verify the CHD")
				}
				if tc.want != "" {
					if _, err := os.Lstat(output); !os.IsNotExist(err) {
						t.Fatalf("refused conversion left an output: %v", err)
					}
				}
			}
		})
	}
}

func TestCHDCUERejectsAmbiguousSectorLayout(t *testing.T) {
	for _, tc := range []struct {
		name, fileType, trackType string
		size                      int
		want                      string
	}{
		{"quoted BINARY", `"BINARY"`, "MODE1/2048", 8*2048 + 1, "unsupported CUE file type"},
		{"NUL in file type", "BINARY\x00ignored", "MODE1/2048", 8*2048 + 1, "NUL byte"},
		{"quoted subcode", "BINARY", `MODE1/2352 "RW_RAW"`, 8 * 2352, "CUE track subcode"},
		{"lowercase subcode", "BINARY", "MODE1/2352 rw_raw", 8 * 2448, "CUE track subcode"},
		{"known subcode", "BINARY", "MODE1/2352 RW_RAW", 8 * 2448, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "disc.cue")
			input := filepath.Join(dir, "track.bin")
			output := filepath.Join(dir, "disc.chd")
			data := bytes.Repeat([]byte{0x5a}, tc.size)
			if err := os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			layout := "FILE track.bin " + tc.fileType + "\n TRACK 01 " + tc.trackType + "\n INDEX 01 00:00:00\n"
			if err := os.WriteFile(path, []byte(layout), 0600); err != nil {
				t.Fatal(err)
			}
			check := func(err error) {
				t.Helper()
				if tc.want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v, want %q", err, tc.want)
				}
			}
			check(chd.ValidateCUE(path, "", kind.DefaultMaxOutput))
			if tool := os.Getenv("ROMCOMPRESSOR_CHDMAN"); tool != "" {
				if tc.want == "" {
					data = testfixture.CHDTrack(data, 2448)
					if err := os.WriteFile(input, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				_, err := romcompressor.Convert(context.Background(), path, output, romcompressor.Options{
					Format: romcompressor.CHD, Tools: map[string]string{"chdman": tool},
				})
				check(err)
				if _, err := os.Lstat(output); tc.want != "" && !os.IsNotExist(err) {
					t.Fatalf("refused conversion left output: %v", err)
				}
				if original, err := os.ReadFile(input); err != nil || !bytes.Equal(original, data) {
					t.Fatalf("original track changed: %v", err)
				}
			}
		})
	}
}

func TestCHDCUEChecksWaveDataLayoutAndIndexes(t *testing.T) {
	var format [16]byte
	binary.LittleEndian.PutUint16(format[0:2], 1)
	binary.LittleEndian.PutUint16(format[2:4], 2)
	binary.LittleEndian.PutUint32(format[4:8], 44100)
	binary.LittleEndian.PutUint32(format[8:12], 176400)
	binary.LittleEndian.PutUint16(format[12:14], 4)
	binary.LittleEndian.PutUint16(format[14:16], 16)
	chunk := func(name string, payload []byte) []byte {
		data := append([]byte(name), 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(data[4:], uint32(len(payload)))
		data = append(data, payload...)
		if len(payload)%2 != 0 {
			data = append(data, 0)
		}
		return data
	}
	pcm := bytes.Repeat([]byte{0x18, 0x42, 0x61, 0x8a}, 8*588)
	fmtChunk, dataChunk := chunk("fmt ", format[:]), chunk("data", pcm)
	valid := [][]byte{fmtChunk, dataChunk}
	const firstFrame = " INDEX 01 00:00:00\n"
	for _, test := range []struct {
		name, indexes string
		chunks        [][]byte
		wantError     string
	}{
		{"one audio extent", firstFrame, valid, ""},
		{"stored pregap", " INDEX 00 00:00:00\n INDEX 01 00:00:02\n", valid, ""},
		{"trailing odd metadata", firstFrame, [][]byte{fmtChunk, dataChunk, chunk("JUNK", []byte{1})}, ""},
		{"audio before fmt", firstFrame, [][]byte{dataChunk, fmtChunk, chunk("data", pcm[:2500])}, "one audio data chunk"},
		{"duplicate audio", firstFrame, [][]byte{fmtChunk, dataChunk, chunk("data", pcm[:2352])}, "one audio data chunk"},
		{"odd chunk before audio", firstFrame, [][]byte{fmtChunk, chunk("JUNK", []byte{1}), dataChunk}, "odd-sized"},
		{"unassigned audio", " INDEX 01 00:00:02\n", valid, "unassigned leading sectors"},
		{"index past audio", " INDEX 00 00:00:00\n INDEX 01 00:00:12\n", valid, "indexes exceed its data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			wave := append([]byte("RIFF\x00\x00\x00\x00WAVE"), bytes.Join(test.chunks, nil)...)
			binary.LittleEndian.PutUint32(wave[4:8], uint32(len(wave)-8))
			wav := filepath.Join(dir, "audio.wav")
			if err := os.WriteFile(wav, wave, 0600); err != nil {
				t.Fatal(err)
			}
			cue := filepath.Join(dir, "audio.cue")
			if err := os.WriteFile(cue, []byte("FILE audio.wav WAVE\n TRACK 01 AUDIO\n"+test.indexes), 0600); err != nil {
				t.Fatal(err)
			}
			checkError := func(err error) {
				t.Helper()
				if test.wantError == "" && err != nil || test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
					t.Fatalf("conversion validation = %v, expected %q", err, test.wantError)
				}
			}
			checkError(chd.ValidateCUE(cue, "", kind.DefaultMaxOutput))
			tool := os.Getenv("ROMCOMPRESSOR_CHDMAN")
			if tool == "" {
				return
			}
			expectedPCM := pcm
			if test.wantError == "" {
				expectedPCM = testfixture.CHDTrack(pcm, 2352)
				chunks := append([][]byte(nil), test.chunks...)
				for i, data := range chunks {
					if string(data[:4]) == "data" {
						chunks[i] = chunk("data", expectedPCM)
					}
				}
				wave = append([]byte("RIFF\x00\x00\x00\x00WAVE"), bytes.Join(chunks, nil)...)
				binary.LittleEndian.PutUint32(wave[4:8], uint32(len(wave)-8))
				if err := os.WriteFile(wav, wave, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			output := filepath.Join(dir, "out.chd")
			opts := romcompressor.Options{Format: romcompressor.CHD, Tools: map[string]string{"chdman": tool}}
			_, err := romcompressor.Convert(ctx, cue, output, opts)
			checkError(err)
			if test.wantError != "" {
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatalf("refused conversion left output: %v", err)
				}
				return
			}
			opts.Format, opts.Media = romcompressor.Raw, "cd"
			if _, err := romcompressor.Convert(ctx, output, filepath.Join(dir, "out.cue"), opts); err != nil {
				t.Fatal(err)
			}
			if restored, err := os.ReadFile(filepath.Join(dir, "out.bin")); err != nil || !bytes.Equal(restored, expectedPCM) {
				t.Fatalf("restored PCM changed: %v", err)
			}
			if original, err := os.ReadFile(wav); err != nil || !bytes.Equal(original, wave) {
				t.Fatalf("original WAVE changed: %v", err)
			}
		})
	}
}
