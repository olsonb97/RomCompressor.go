package romcompressor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	splitTestBlockSize = 2 << 20
	splitTestFirstEnd  = splitTestBlockSize + 0x1a
	splitTestSecondEnd = splitTestFirstEnd + 0x10000
	splitTestISOSize   = 143432 * (0x8000)
)

func TestSplitWBFSInspectVerifyAndRead(t *testing.T) {
	source, paths, volume := makeSplitWBFS(t)
	ctx := context.Background()
	info, err := Inspect(ctx, source)
	if err != nil || info.Format != WBFS || info.Size != int64(len(volume)) {
		t.Fatalf("Inspect: %+v, %v", info, err)
	}
	if _, err := Inspect(ctx, paths[1]); err == nil || !strings.Contains(err.Error(), "primary .wbfs") {
		t.Fatalf("expected an explanation for the orphan part, got %v", err)
	}
	input, err := openWBFSInput(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.close()
	for _, at := range []int64{0, splitTestFirstEnd - 9, splitTestSecondEnd - 9, int64(len(volume)) - 9} {
		got := make([]byte, 19)
		n, err := input.ReadAt(got, at)
		expect := volume[at:min(at+int64(len(got)), int64(len(volume)))]
		if n != len(expect) || !bytes.Equal(got[:n], expect) {
			t.Fatalf("read at %d got %d, %v", at, n, err)
		}
		if (n < len(got) && !errors.Is(err, io.EOF)) || (n == len(got) && err != nil) {
			t.Fatalf("unexpected read error at %d: %v", at, err)
		}
	}
	if err := Verify(ctx, source, Options{}); err != nil {
		t.Fatal(err)
	}
	assertSplitWBFSUnchanged(t, paths, volume)
}

func TestSplitWBFSRejectsMissingOrUnexpectedParts(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(t *testing.T, paths []string)
		errorHas string
	}{
		{"gap", func(t *testing.T, paths []string) {
			if err := os.Remove(paths[1]); err != nil {
				t.Fatal(err)
			}
		}, "required WBFS part"},
		{"short last part", func(t *testing.T, paths []string) {
			if err := os.Truncate(paths[2], 512); err != nil {
				t.Fatal(err)
			}
		}, "required WBFS part"},
		{"long last part", func(t *testing.T, paths []string) {
			if err := os.Truncate(paths[2], splitTestBlockSize); err != nil {
				t.Fatal(err)
			}
		}, "extends beyond"},
		{"empty part", func(t *testing.T, paths []string) {
			if err := os.Truncate(paths[1], 0); err != nil {
				t.Fatal(err)
			}
		}, "nonempty regular file"},
		{"unexpected part after gap", func(t *testing.T, paths []string) {
			if err := os.WriteFile(paths[0][:len(paths[0])-1]+"4", []byte{1}, 0600); err != nil {
				t.Fatal(err)
			}
		}, "unexpected WBFS part"},
		{"same companion twice", func(t *testing.T, paths []string) {
			if err := os.Remove(paths[2]); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(paths[1], paths[2]); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
		}, "repeats an earlier file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, paths, _ := makeSplitWBFS(t)
			test.mutate(t, paths)
			if _, err := Inspect(context.Background(), source); err == nil || !strings.Contains(err.Error(), test.errorHas) {
				t.Fatalf("got %v, want error containing %q", err, test.errorHas)
			}
		})
	}
}

func TestWBFSRejectsCompanionsAfterCompletePrimary(t *testing.T) {
	source, _, volume := makeSplitWBFS(t)
	if err := os.WriteFile(source, volume, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), source); err == nil || !strings.Contains(err.Error(), "unexpected WBFS part") {
		t.Fatalf("expected rejection of companion to complete WBFS, got %v", err)
	}
}

func TestSplitWBFSVerifyDetectsCompanionMutation(t *testing.T) {
	source, paths, _ := makeSplitWBFS(t)
	mutated := false
	oldTime := time.Unix(100, 0)
	err := Verify(context.Background(), source, Options{Progress: func(Progress) {
		if !mutated {
			mutated = true
			if err := os.Chtimes(paths[1], oldTime, oldTime); err != nil {
				t.Error(err)
			}
		}
	}})
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("expected source change, got %v", err)
	}
}

func TestSplitWBFSFailurePreservesFiles(t *testing.T) {
	source, paths, volume := makeSplitWBFS(t)
	ctx := context.Background()
	plan, err := PlanConversions(ctx, source, Options{})
	if err != nil || planHas(plan, SevenZip, "") || !planHas(plan, Raw, "") {
		t.Fatalf("incorrect split WBFS targets: %+v, %v", plan.Targets, err)
	}
	for _, test := range []struct {
		name string
		opts Options
		want string
	}{
		{"restore.iso", Options{Format: Raw, MaxOutput: 1024}, "expanded size limit"},
		{"archive.zip", Options{Format: ZIP}, "split WBFS conversion requires ISO"},
	} {
		output := filepath.Join(filepath.Dir(source), test.name)
		if _, err := Convert(ctx, source, output, test.opts); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("got %v, want error containing %q", err, test.want)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("failed conversion left its output: %v", err)
		}
	}
	assertSplitWBFSUnchanged(t, paths, volume)
}

func TestSplitWBFSPlaylistJoinsAllParts(t *testing.T) {
	source, paths, volume := makeSplitWBFS(t)
	dir := filepath.Dir(source)
	playlist := filepath.Join(dir, "game.m3u")
	if err := os.WriteFile(playlist, []byte("game.wbfs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "joined.m3u")
	result, err := Convert(context.Background(), playlist, output, Options{Format: WBFS})
	if err != nil || !result.Verified {
		t.Fatalf("playlist: %+v, %v", result, err)
	}
	restored, err := os.ReadFile(filepath.Join(dir, "joined", "game.wbfs"))
	if err != nil || !bytes.Equal(restored, volume) {
		t.Fatalf("joined WBFS differs: %v", err)
	}
	assertSplitWBFSUnchanged(t, paths, volume)
}

// The raw output is a full single-layer ISO. Keep this disk-heavy test opt-in.
func TestSplitWBFSFullRestore(t *testing.T) {
	if os.Getenv("ROMCOMPRESSOR_TEST_LARGE_WBFS") != "1" {
		t.Skip("set ROMCOMPRESSOR_TEST_LARGE_WBFS=1 for a full split WBFS restoration")
	}
	source, paths, volume := makeSplitWBFS(t)
	output := filepath.Join(filepath.Dir(source), "restored.iso")
	result, err := Convert(context.Background(), source, output, Options{Format: Raw})
	if err != nil || !result.Verified || result.OutputBytes != splitTestISOSize {
		t.Fatalf("full restoration: %+v, %v", result, err)
	}
	restored, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got := sha256.New()
	if _, err := io.Copy(got, restored); err != nil {
		t.Fatal(err)
	}
	want := sha256.New()
	want.Write(volume[splitTestBlockSize:])
	var zeros [128 << 10]byte
	for remaining := int64(splitTestISOSize - splitTestBlockSize); remaining > 0; {
		n := min(remaining, int64(len(zeros)))
		want.Write(zeros[:n])
		remaining -= n
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatal("restored SHA-256 differs from the synthetic ISO")
	}
	assertSplitWBFSUnchanged(t, paths, volume)
}

func makeSplitWBFS(t *testing.T) (string, []string, []byte) {
	t.Helper()
	volume := make([]byte, 2*splitTestBlockSize)
	copy(volume, "WBFS")
	binary.BigEndian.PutUint32(volume[4:], uint32(len(volume)/512))
	volume[8], volume[9], volume[12] = 9, 21, 1
	binary.BigEndian.PutUint32(volume[0x200+0x18:], 0x5d1c9ea3)
	volume[0x301] = 1 // Map the first ISO block to physical WBFS block 1.
	binary.BigEndian.PutUint32(volume[splitTestBlockSize+0x18:], 0x5d1c9ea3)
	copy(volume[splitTestSecondEnd-4:], "payload spanning the last two parts")
	dir := t.TempDir()
	first := filepath.Join(dir, "game.wbfs")
	paths := []string{first, filepath.Join(dir, "game.wbf1"), filepath.Join(dir, "game.wbf2")}
	offsets := []int{0, splitTestFirstEnd, splitTestSecondEnd, len(volume)}
	for i, path := range paths {
		if err := os.WriteFile(path, volume[offsets[i]:offsets[i+1]], 0600); err != nil {
			t.Fatal(err)
		}
	}
	return first, paths, volume
}

func assertSplitWBFSUnchanged(t *testing.T, paths []string, volume []byte) {
	t.Helper()
	offsets := []int{0, splitTestFirstEnd, splitTestSecondEnd, len(volume)}
	for i, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, volume[offsets[i]:offsets[i+1]]) {
			t.Fatalf("source WBFS part %d changed: %v", i, err)
		}
	}
}
