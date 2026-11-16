package romcompressor

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olsonb97/romcompressor.go/internal/disc"
)

func TestNativeFileLifecycle(t *testing.T) {
	for _, format := range []Format{ZIP, GZIP, CSO, ZSO} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game.iso")
			ext := string(format)
			if format == GZIP {
				ext = "gz"
			}
			packed := filepath.Join(dir, "game."+ext)
			restored := filepath.Join(dir, "restored.iso")
			data := bytes.Repeat([]byte{0, 1, 2, 3}, 8192)
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := Convert(context.Background(), source, packed, Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Verified {
				t.Fatal("missing verification")
			}
			if err := Verify(context.Background(), packed, Options{}); err != nil {
				t.Fatal(err)
			}
			if _, err := Convert(context.Background(), packed, restored, Options{Format: Raw, Progress: func(Progress) {}}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(restored)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("roundtrip: %v", err)
			}
			if _, err := Convert(context.Background(), source, packed, Options{Format: format}); err == nil {
				t.Fatal("overwrote output")
			}
			got, _ = os.ReadFile(source)
			if !bytes.Equal(got, data) {
				t.Fatal("source changed")
			}
			matches, _ := filepath.Glob(filepath.Join(dir, ".romcompressor-*"))
			if len(matches) != 0 {
				t.Fatal("staging leaked")
			}
		})
	}
}

func TestNativeCompressionNeverStagesPastLimit(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.iso")
	const limit = 2048
	payload := make([]byte, limit)
	if _, err := rand.New(rand.NewSource(1)).Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []Format{CSO, ZSO, ZIP, GZIP} {
		t.Run(string(format), func(t *testing.T) {
			staged := filepath.Join(dir, "staged."+string(format))
			info := FileInfo{Format: Raw, Size: limit}
			opts := Options{Format: format, MaxOutput: limit}
			err := convertNative(context.Background(), source, staged, info, opts, nil, progressReporter(nil))
			if err == nil || !strings.Contains(err.Error(), "size limit") {
				t.Errorf("convertNative = %v; want an output-size rejection", err)
			}
			stat, err := os.Stat(staged)
			if err != nil {
				t.Fatal(err)
			}
			if stat.Size() > limit {
				t.Errorf("staged %d bytes beyond the %d-byte limit", stat.Size(), limit)
			}
		})
	}
}

func TestCancellationAndMutationLeaveNoOutput(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		dir := t.TempDir()
		src := filepath.Join(dir, "game.iso")
		out := filepath.Join(dir, "out.cso")
		if err := os.WriteFile(src, make([]byte, 65536), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		_, err := Convert(ctx, src, out, Options{Format: CSO, Progress: func(p Progress) {
			if p.Phase == "converting" {
				if mutate {
					_ = os.WriteFile(src, []byte("changed"), 0600)
				} else {
					cancel()
				}
			}
		}})
		cancel()
		if err == nil {
			t.Fatal("expected failure")
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("output published")
		}
		matches, _ := filepath.Glob(filepath.Join(dir, ".romcompressor-*"))
		if len(matches) != 0 {
			t.Fatal("staging leaked")
		}
	}
}

func TestConcurrentPublication(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.iso")
	out := filepath.Join(dir, "a.cso")
	_ = os.WriteFile(src, make([]byte, 32768), 0600)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Convert(context.Background(), src, out, Options{Format: CSO})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes %d", successes)
	}
	if err := Verify(context.Background(), out, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestPublishCompanionConflictRollsBack(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	_ = os.Mkdir(stage, 0700)
	for _, name := range []string{"entry.bin", "entry.sbi", "entry.cue"} {
		_ = os.WriteFile(filepath.Join(stage, name), []byte("staged"), 0600)
	}
	_ = os.WriteFile(filepath.Join(dir, "entry.sbi"), []byte("original"), 0600)
	_, err := publish(context.Background(), stage, filepath.Join(stage, "entry.cue"), filepath.Join(dir, "entry.cue"), 1000, filepath.Join(stage, "entry.bin"), filepath.Join(stage, "entry.sbi"))
	if err == nil {
		t.Fatal("expected conflict")
	}
	if _, err := os.Stat(filepath.Join(dir, "entry.bin")); !os.IsNotExist(err) {
		t.Fatal("earlier companion not removed")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "entry.sbi"))
	if string(got) != "original" {
		t.Fatal("existing file changed")
	}
}

func TestPublicationRollbackPreservesReplacement(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "staged.bin")
	output := filepath.Join(dir, "output.bin")
	if err := os.WriteFile(staged, []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}
	created, err := publishRegularFile(context.Background(), staged, output, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(output, filepath.Join(dir, "moved.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("companion collision")
	if err := rollbackPublished([]publishedPath{created}, cause); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("rollback altered replacement: %q, %v", got, err)
	}
}

func TestNativeVerificationFollowsFinalProgressCallback(t *testing.T) {
	ctx := context.Background()
	for _, format := range []Format{GZIP, Raw} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.bin")
			original := []byte("generated payload")
			if err := os.WriteFile(source, original, 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "converted.gz")
			input := source
			if format == Raw {
				input = filepath.Join(dir, "source.gz")
				if _, err := Convert(ctx, source, input, Options{Format: GZIP}); err != nil {
					t.Fatal(err)
				}
				output = filepath.Join(dir, "converted.bin")
			}
			var changed bool
			_, err := Convert(ctx, input, output, Options{Format: format, Progress: func(p Progress) {
				if p.Phase != "publishing" {
					return
				}
				matches, err := filepath.Glob(filepath.Join(dir, ".romcompressor-*", filepath.Base(output)))
				if err != nil || len(matches) != 1 {
					t.Fatalf("find staged output: %v, %v", matches, err)
				}
				file, err := os.OpenFile(matches[0], os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte{0}, 0)
				if err := errors.Join(err, file.Close()); err != nil {
					t.Fatal(err)
				}
				changed = true
			}})
			if !changed || err == nil {
				t.Fatalf("Convert = %v, staged bytes changed = %v", err, changed)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("unexpected output: %v", err)
			}
			if after, err := os.ReadFile(source); err != nil || !bytes.Equal(after, original) {
				t.Fatalf("source changed: %q, %v", after, err)
			}
		})
	}
}

func TestVerifyMultipleZIPEntriesAndBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "games.zip")
	f, _ := os.Create(path)
	z := zip.NewWriter(f)
	for _, name := range []string{"a.gb", "b.gb"} {
		entry, _ := z.Create(name)
		_, _ = entry.Write([]byte("12345678"))
	}
	_ = z.Close()
	_ = f.Close()
	if err := Verify(context.Background(), path, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), path, Options{MaxOutput: 10}); err == nil {
		t.Fatal("total expansion limit ignored")
	}
	if _, err := Convert(context.Background(), path, filepath.Join(dir, "a.gb"), Options{Format: Raw}); err == nil {
		t.Fatal("ambiguous ZIP selected implicitly")
	}
}

func TestVerifyRejectsIncompatibleOptions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source, archive := filepath.Join(dir, "game.bin"), filepath.Join(dir, "game.gz")
	if err := os.WriteFile(source, []byte("generated fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(ctx, source, archive, Options{Format: GZIP}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"entry", Options{Entry: "missing.bin"}},
		{"media", Options{Media: "dvd"}},
		{"keys", Options{Keys: "unused.keys"}},
		{"extract all", Options{ExtractAll: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(ctx, archive, tc.opts); err == nil {
				t.Fatalf("Verify accepted the incompatible %s option", tc.name)
			}
		})
	}
}

func TestVerifyRefusesSourceChangeDuringProgress(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source, archive := filepath.Join(dir, "game.bin"), filepath.Join(dir, "game.gz")
	if err := os.WriteFile(source, bytes.Repeat([]byte{0x7f}, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(ctx, source, archive, Options{Format: GZIP}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	var changed bool
	opts := Options{Progress: func(p Progress) {
		if changed || p.Bytes == 0 {
			return
		}
		changed = true
		file, err := os.OpenFile(archive, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		// The gzip header has already been read from this file.
		_, err = file.WriteAt([]byte{0}, 0)
		if err := errors.Join(err, file.Close()); err != nil {
			t.Fatal(err)
		}
		modified := before.ModTime().Add(time.Minute)
		if err := os.Chtimes(archive, modified, modified); err != nil {
			t.Fatal(err)
		}
	}}
	if err := Verify(ctx, archive, opts); !changed || err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("Verify = %v, fixture changed = %v", err, changed)
	}
}

func TestDiscReferencesStayInSetAndAreSnapshotted(t *testing.T) {
	dir := t.TempDir()
	track := filepath.Join(dir, "track.bin")
	cue := filepath.Join(dir, "game.cue")
	_ = os.WriteFile(track, make([]byte, 2352), 0600)
	_ = os.WriteFile(cue, []byte("FILE \"track.bin\" BINARY\n TRACK 01 MODE1/2352\n INDEX 01 00:00:00\n"), 0600)
	before, err := snapshotInputs(cue)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatal("track not captured")
	}
	_ = os.WriteFile(track, []byte("changed"), 0600)
	if err := checkInputs(before); err == nil {
		t.Fatal("track mutation undetected")
	}
	_ = os.WriteFile(cue, []byte("FILE \"../outside.bin\" BINARY\n"), 0600)
	if err := disc.ValidateReferences(cue); err == nil {
		t.Fatal("escape accepted")
	}
}

func TestSnapshotIdentifiesReplacementWithMatchingMetadata(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	inputs, err := snapshotInputs(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, source+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	mtime := inputs[source].ModTime()
	if err := os.Chtimes(source, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := checkInputs(inputs); err == nil {
		t.Fatal("source replacement with matching size and mtime went undetected")
	}
}
