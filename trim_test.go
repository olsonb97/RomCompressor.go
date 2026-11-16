package romcompressor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestTrimPlanAndCopyPreserveOriginal(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	ctx := context.Background()
	plan, err := PlanTrim(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	wantPlan := TrimPlan{Format: "3ds", InputBytes: int64(len(original)), OutputBytes: testfixture.CARD1TrimmedSize, PaddingByte: 0xff}
	if plan != wantPlan {
		t.Fatalf("PlanTrim = %+v; want %+v", plan, wantPlan)
	}
	entries, err := os.ReadDir(filepath.Dir(source))
	if err != nil || len(entries) != 1 {
		t.Fatalf("dry run wrote files: %v, %v", entries, err)
	}
	assertTrimBytes(t, source, original)

	output := filepath.Join(filepath.Dir(source), "trimmed.3ds")
	result, err := Trim(ctx, source, output, TrimOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := original[:testfixture.CARD1TrimmedSize]
	digest := sha256.Sum256(want)
	if result.TrimPlan != wantPlan || result.Output != output || result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("unexpected trim result: %+v", result)
	}
	assertTrimBytes(t, output, want)
	assertTrimBytes(t, source, original)
}

func TestTrimRejectsNonPaddingBeforeStaging(t *testing.T) {
	original := testfixture.CARD1()
	original[len(original)-1] = 0x42
	source := writeTrimInput(t, original)
	output := filepath.Join(filepath.Dir(source), "rejected.3ds")
	_, err := Trim(context.Background(), source, output, TrimOptions{Progress: func(p Progress) {
		if p.Phase == "trimming copy" || p.Phase == "publishing" {
			t.Errorf("started %s before validating the complete padding suffix", p.Phase)
		}
	}})
	if err == nil || !strings.Contains(err.Error(), "padding") {
		t.Fatalf("Trim = %v, want non-padding rejection", err)
	}
	assertTrimBytes(t, source, original)
	assertTrimAbsent(t, output)
}

func TestTrimPreviewProgressAndCancellation(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	var phases []string
	plan, err := PlanTrimWithOptions(context.Background(), source, TrimOptions{Progress: func(p Progress) {
		phases = append(phases, p.Phase)
		if p.Phase == "checking padding" && p.Total != int64(len(original)-testfixture.CARD1TrimmedSize) {
			t.Errorf("unexpected padding total: %+v", p)
		}
	}})
	if err != nil || plan.OutputBytes != testfixture.CARD1TrimmedSize || len(phases) < 3 || phases[0] != "validating cartridge" || phases[1] != "checking padding" || phases[len(phases)-1] != "complete" {
		t.Fatalf("preview = %+v, %v, phases %v", plan, err, phases)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = PlanTrimWithOptions(ctx, source, TrimOptions{Progress: func(p Progress) {
		if p.Phase == "checking padding" {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("preview cancellation = %v", err)
	}
	assertTrimBytes(t, source, original)
	entries, err := os.ReadDir(filepath.Dir(source))
	if err != nil || len(entries) != 1 {
		t.Fatalf("preview created files: %v, %v", entries, err)
	}
}

func TestTrimPreviewChecksSourceAfterCompleteCallback(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	changed := false
	_, err := PlanTrimWithOptions(context.Background(), source, TrimOptions{Progress: func(p Progress) {
		if p.Phase != "complete" {
			return
		}
		changed = true
		file, err := os.OpenFile(source, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteAt([]byte{0x42}, int64(len(original)))
		if err := errors.Join(writeErr, file.Close()); err != nil {
			t.Fatal(err)
		}
	}})
	if !changed || err == nil {
		t.Fatalf("preview accepted changed source: changed=%t, err=%v", changed, err)
	}
}

func TestTrimNoPaddingIsIdentifiable(t *testing.T) {
	original := testfixture.CARD1()[:testfixture.CARD1TrimmedSize]
	source := writeTrimInput(t, original)
	output := filepath.Join(filepath.Dir(source), "unchanged.3ds")
	_, err := Trim(context.Background(), source, output, TrimOptions{})
	if !errors.Is(err, ErrNoRemovablePadding) {
		t.Fatalf("Trim = %v, want ErrNoRemovablePadding", err)
	}
	assertTrimBytes(t, source, original)
	assertTrimAbsent(t, output)
}

func TestTrimDSReconstructsExactSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsi  bool
		keep int64
	}{
		{"DS", false, testfixture.NDSTrimmedSize},
		{"DSi", true, testfixture.DSiTrimmedSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "original.nds")
			output := filepath.Join(dir, "trimmed.nds")
			data := testfixture.NDS(tc.dsi)
			want := sha256.Sum256(data)
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := Trim(context.Background(), source, output, TrimOptions{})
			if err != nil || result.Format != "nds" || result.OutputBytes != tc.keep || result.InputBytes != int64(len(data)) {
				t.Fatalf("Trim = %+v, %v", result, err)
			}
			assertTrimBytes(t, source, data)
			assertTrimBytes(t, output, data[:tc.keep])
			kept, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			got := sha256.Sum256(append(kept, bytes.Repeat([]byte{result.PaddingByte}, int(result.InputBytes-result.OutputBytes))...))
			if got != want {
				t.Fatal("trim result does not reconstruct the input SHA-256")
			}
		})
	}
}

func TestTrimChecksDSiBlockDigestInput(t *testing.T) {
	const keep = testfixture.DSiTrimmedSize
	for _, tc := range []struct {
		name     string
		unitCode byte
		checksum uint16
		sectorAt uint32
		invalid  bool
	}{
		{"enhanced cartridge", 2, 0x9b02, keep - 0x100, true},
		{"exclusive cartridge", 3, 0x5e22, keep - 0x100, true},
		{"digest input ends at boundary", 2, 0x9b02, keep - 0xf00, false},
		{"optional digest tables absent", 2, 0x9b02, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := testfixture.NDS(true)
			data[0x12] = tc.unitCode
			binary.LittleEndian.PutUint16(data[0x15e:], tc.checksum)
			if tc.sectorAt == 0 {
				clear(data[0x1e0:0x208])
			} else {
				// The recorded sector table occupies 0x100 bytes. Six
				// block hashes read 32 sector hashes (20 bytes each) apiece.
				binary.LittleEndian.PutUint32(data[0x1f0:], tc.sectorAt)
			}
			dir := t.TempDir()
			source := filepath.Join(dir, "original.nds")
			output := filepath.Join(dir, "trimmed.nds")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			plan, planErr := PlanTrim(ctx, source)
			result, trimErr := Trim(ctx, source, output, TrimOptions{})
			assertTrimBytes(t, source, data)
			if tc.invalid {
				for _, err := range []error{planErr, trimErr} {
					if err == nil || !strings.Contains(err.Error(), "block digest input") {
						t.Errorf("got %v, want rejection of block digest input beyond the used extent", err)
					}
				}
				assertTrimAbsent(t, output)
				return
			}
			if planErr != nil || trimErr != nil || plan.OutputBytes != keep || result.TrimPlan != plan {
				t.Fatalf("PlanTrim = %+v, %v; Trim = %+v, %v", plan, planErr, result, trimErr)
			}
			assertTrimBytes(t, output, data[:keep])
		})
	}
}

func TestTrimRejectsDSOverlayIntoUniformSuffix(t *testing.T) {
	data := testfixture.NDS(false)
	// The third FAT entry lies outside the declared table and points into
	// the uniform FF suffix. The overlay references it by file ID.
	binary.LittleEndian.PutUint32(data[0x9040+0x18:], 2)
	binary.LittleEndian.PutUint32(data[0x9030:], testfixture.NDSTrimmedSize)
	binary.LittleEndian.PutUint32(data[0x9034:], testfixture.NDSTrimmedSize+0x400)
	dir := t.TempDir()
	source := filepath.Join(dir, "original.nds")
	output := filepath.Join(dir, "trimmed.nds")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := PlanTrim(ctx, source); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("PlanTrim error = %v; want rejection of out-of-table overlay", err)
	}
	if _, err := Trim(ctx, source, output, TrimOptions{}); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("Trim error = %v; want rejection of out-of-table overlay", err)
	}
	assertTrimBytes(t, source, data)
	assertTrimAbsent(t, output)
}

func TestTrimRejectsDSOverlayAuthenticationBeyondFAT(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags byte
	}{
		{"whitelisted", 0},
		{"signed", 0x40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := testfixture.NDS(false)
			data[0x1bf] = tc.flags
			// All three overlays use valid FAT IDs, but authentication reads
			// one leading FAT record per overlay. The third is undeclared.
			binary.LittleEndian.PutUint32(data[0x54:], 3*32)
			binary.LittleEndian.PutUint16(data[0x15e:], 0x2b27)
			copy(data[0x9060:0x9080], data[0x9040:0x9060])
			copy(data[0x9080:0x90a0], data[0x9040:0x9060])
			binary.LittleEndian.PutUint32(data[0x9060:], 1)
			binary.LittleEndian.PutUint32(data[0x9078:], 1)
			binary.LittleEndian.PutUint32(data[0x9080:], 2)
			binary.LittleEndian.PutUint32(data[0x9030:], testfixture.NDSTrimmedSize+0x1000)
			binary.LittleEndian.PutUint32(data[0x9034:], testfixture.NDSTrimmedSize+0x1200)
			dir := t.TempDir()
			source := filepath.Join(dir, "original.nds")
			output := filepath.Join(dir, "trimmed.nds")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := PlanTrim(ctx, source); err == nil || !strings.Contains(err.Error(), "overlay authentication") {
				t.Errorf("PlanTrim error = %v; want rejection of out-of-table authentication", err)
			}
			if _, err := Trim(ctx, source, output, TrimOptions{}); err == nil || !strings.Contains(err.Error(), "overlay authentication") {
				t.Errorf("Trim error = %v; want rejection of out-of-table authentication", err)
			}
			assertTrimBytes(t, source, data)
			assertTrimAbsent(t, output)
		})
	}
}

func TestTrimKeepsDSOverlayAuthenticationPadding(t *testing.T) {
	const overlayStart = testfixture.NDSTrimmedSize
	const used = overlayStart + 0x80
	const authenticatedEnd = overlayStart + 0x200
	for _, tc := range []struct {
		name  string
		flags byte
	}{
		{"whitelisted", 0},
		{"signed", 0x40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := testfixture.NDS(false)
			data[0x1bf] = tc.flags
			// The first FAT entry supplies the ARM9 overlay. Its 0x80-byte
			// payload hashes as 0x200 bytes, including FF beyond the used size.
			binary.LittleEndian.PutUint32(data[0x80:], used)
			binary.LittleEndian.PutUint16(data[0x15e:], 0x38a2)
			binary.LittleEndian.PutUint32(data[0x9020:], overlayStart)
			binary.LittleEndian.PutUint32(data[0x9024:], used)
			binary.LittleEndian.PutUint32(data[0x9048:], used-overlayStart)
			data[overlayStart] = 0x7c
			dir := t.TempDir()
			source := filepath.Join(dir, "original.nds")
			output := filepath.Join(dir, "trimmed.nds")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			plan, err := PlanTrim(ctx, source)
			if err != nil || plan.OutputBytes != authenticatedEnd {
				t.Fatalf("PlanTrim = %+v, %v; want output length %#x", plan, err, authenticatedEnd)
			}
			if _, err := Trim(ctx, source, output, TrimOptions{}); err != nil {
				t.Fatal(err)
			}
			assertTrimBytes(t, source, data)
			assertTrimBytes(t, output, data[:authenticatedEnd])
		})
	}
}

func TestTrimRefusesExistingOutputs(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	dir := filepath.Dir(source)
	existing := filepath.Join(dir, "existing.3ds")
	if err := os.WriteFile(existing, []byte("previous output"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, output := range []string{source, existing} {
		if _, err := Trim(ctx, source, output, TrimOptions{}); err == nil {
			t.Fatal("trim accepted an existing output")
		}
	}
	assertTrimBytes(t, source, original)
	assertTrimBytes(t, existing, []byte("previous output"))
	t.Run("hard link to input", func(t *testing.T) {
		link := filepath.Join(dir, "alias.3ds")
		if err := os.Link(source, link); err != nil {
			t.Skipf("file system cannot create hard links: %v", err)
		}
		if _, err := Trim(ctx, source, link, TrimOptions{}); err == nil {
			t.Fatal("trim accepted a hard link to the original")
		}
		assertTrimBytes(t, link, original)
		assertTrimBytes(t, source, original)
	})
}

func TestTrimRefusesLateHardLinkToSource(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	output := filepath.Join(filepath.Dir(source), "trimmed.3ds")
	probe := filepath.Join(filepath.Dir(source), "probe.3ds")
	if err := os.Link(source, probe); err != nil {
		t.Skipf("file system cannot create hard links: %v", err)
	}
	var linked bool
	_, err := Trim(context.Background(), source, output, TrimOptions{Progress: func(p Progress) {
		if p.Phase == "publishing" {
			if err := os.Link(source, output); err != nil {
				t.Fatal(err)
			}
			linked = true
		}
	}})
	if err == nil || !linked {
		t.Fatalf("Trim = %v, hard link created = %v", err, linked)
	}
	assertTrimBytes(t, source, original)
	assertTrimBytes(t, output, original)
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	assertTrimAbsent(t, output)
}

func TestTrimChecksEntirePaddingRange(t *testing.T) {
	for _, tt := range []struct {
		name string
		last byte
	}{
		{"mixed padding bytes", 0x00},
		{"nonpadding byte", 0x45},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := append(testfixture.CARD1(), bytes.Repeat([]byte{0xff}, 256<<10)...)
			original[len(original)-1] = tt.last
			source := writeTrimInput(t, original)
			output := filepath.Join(filepath.Dir(source), "trimmed.3ds")
			ctx := context.Background()
			if _, err := PlanTrim(ctx, source); err == nil {
				t.Fatal("dry run accepted a nonuniform trailer")
			}
			if _, err := Trim(ctx, source, output, TrimOptions{}); err == nil {
				t.Fatal("trim accepted a nonuniform trailer")
			}
			assertTrimBytes(t, source, original)
			assertTrimAbsent(t, output)
		})
	}
}

func TestTrimRejectsMisleadingSignature(t *testing.T) {
	data := testfixture.CARD1()
	// Keep a plausible NCSD signature and layout, but remove the signature
	// of a required nested partition. Magic alone cannot authorize trimming.
	at := testfixture.CARD1UpdateOffset + 0x100
	clear(data[at : at+4])
	source := writeTrimInput(t, data)
	output := filepath.Join(filepath.Dir(source), "trimmed.3ds")
	if _, err := Trim(context.Background(), source, output, TrimOptions{}); err == nil {
		t.Fatal("trim accepted an incomplete cartridge despite its valid-looking header")
	}
	assertTrimBytes(t, source, data)
	assertTrimAbsent(t, output)
}

func TestTrimCancellationRemovesStaging(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	output := filepath.Join(filepath.Dir(source), "trimmed.3ds")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := TrimOptions{Progress: func(p Progress) {
		if p.Phase == "trimming copy" {
			cancel()
		}
	}}
	if _, err := Trim(ctx, source, output, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("Trim error = %v; want cancellation", err)
	}
	assertTrimBytes(t, source, original)
	assertTrimAbsent(t, output)
}

func TestTrimRejectsSourceMutationAtPublication(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	output := filepath.Join(filepath.Dir(source), "trimmed.3ds")
	mutated := original[:len(original)-0x200]
	var changed bool
	var mutateErr error
	opts := TrimOptions{Progress: func(p Progress) {
		if p.Phase == "publishing" {
			changed = true
			mutateErr = os.Truncate(source, int64(len(mutated)))
		}
	}}
	_, err := Trim(context.Background(), source, output, opts)
	if !changed || mutateErr != nil {
		t.Fatalf("source mutation never happened: changed=%v, error=%v", changed, mutateErr)
	}
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("Trim error = %v; want source mutation rejection", err)
	}
	assertTrimBytes(t, source, mutated)
	assertTrimAbsent(t, output)
}

func TestTrimRechecksBytesAfterProgress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		staged bool
		offset int
		want   string
	}{
		{"source padding", false, testfixture.CARD1TrimmedSize + 10, "suffix is not uniform padding"},
		{"source prefix", false, testfixture.CARD1UpdateOffset + 0x10, "does not reproduce the original prefix"},
		{"staged prefix", true, testfixture.CARD1UpdateOffset + 0x10, "does not reproduce the original prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := testfixture.CARD1()
			source := writeTrimInput(t, original)
			output := filepath.Join(filepath.Dir(source), "trimmed.3ds")
			changed := original[tc.offset] ^ 0x5a
			var called bool
			_, err := Trim(context.Background(), source, output, TrimOptions{Progress: func(p Progress) {
				if p.Phase != "publishing" {
					return
				}
				called = true
				path := source
				if tc.staged {
					matches, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".romcompressor-trim-*", filepath.Base(output)))
					if err != nil || len(matches) != 1 {
						t.Fatalf("locate staged trim: %v, %v", matches, err)
					}
					path = matches[0]
				}
				st, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte{changed}, int64(tc.offset))
				if err := errors.Join(err, file.Close()); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
					t.Fatal(err)
				}
			}})
			if !called || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Trim = %v, progress called = %v; want %q", err, called, tc.want)
			}
			if !tc.staged {
				original[tc.offset] = changed
			}
			assertTrimBytes(t, source, original)
			assertTrimAbsent(t, output)
		})
	}
}

func TestTrimPreservesReplacementStagingDirectory(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	dir := filepath.Dir(source)
	output := filepath.Join(dir, "trimmed.3ds")
	var replacement string
	_, err := Trim(context.Background(), source, output, TrimOptions{Progress: func(p Progress) {
		if p.Phase != "publishing" {
			return
		}
		matches, err := filepath.Glob(filepath.Join(dir, ".romcompressor-trim-*"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("find stage: %v, %v", matches, err)
		}
		stage := matches[0]
		if err := os.Rename(stage, filepath.Join(dir, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(stage, 0700); err != nil {
			t.Fatal(err)
		}
		replacement = filepath.Join(stage, "important.bin")
		if err := os.WriteFile(replacement, []byte("unrelated"), 0600); err != nil {
			t.Fatal(err)
		}
	}})
	if err == nil || !strings.Contains(err.Error(), "staging directory was replaced") || replacement == "" {
		t.Fatalf("Trim = %v, replacement = %q", err, replacement)
	}
	assertTrimBytes(t, replacement, []byte("unrelated"))
	assertTrimBytes(t, source, original)
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("unexpected output: %v", err)
	}
}

func TestTrimRejectsByteIdenticalStagedReplacement(t *testing.T) {
	original := testfixture.CARD1()
	source := writeTrimInput(t, original)
	dir := filepath.Dir(source)
	output := filepath.Join(dir, "trimmed.3ds")
	want := original[:testfixture.CARD1TrimmedSize]
	_, err := Trim(context.Background(), source, output, TrimOptions{Progress: func(p Progress) {
		if p.Phase != "publishing" {
			return
		}
		matches, err := filepath.Glob(filepath.Join(dir, ".romcompressor-trim-*", filepath.Base(output)))
		if err != nil || len(matches) != 1 {
			t.Fatalf("find staged copy: %v, %v", matches, err)
		}
		if err := os.Rename(matches[0], filepath.Join(dir, "moved.3ds")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(matches[0], want, 0600); err != nil {
			t.Fatal(err)
		}
	}})
	if err == nil || !strings.Contains(err.Error(), "staged trim was replaced") {
		t.Fatalf("Trim = %v; want staged-file identity failure", err)
	}
	assertTrimBytes(t, source, original)
	assertTrimAbsent(t, output)
}

func writeTrimInput(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "original.3ds")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertTrimBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s did not retain expected bytes: got %d, want %d, error %v", path, len(got), len(want), err)
	}
}

func assertTrimAbsent(t *testing.T, output string) {
	t.Helper()
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("trim created an output on failure: %v", err)
	}
	staged, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".romcompressor-trim-*"))
	if err != nil || len(staged) != 0 {
		t.Fatalf("trim left staged files: %v, %v", staged, err)
	}
}
