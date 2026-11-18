package integration_test

import (
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"

	rc "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/disc"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// Exercise the positive path with the raw file, descriptor or title directory
// each encoder actually accepts. Switch has a separate explicit-input test.
func TestFormatWorkflows(t *testing.T) {
	for _, item := range rc.Formats() {
		format := item.Format
		if format == rc.NSZ || format == rc.XCZ {
			continue
		}
		if format == rc.CHD {
			for _, medium := range []string{"cd", "dvd", "hdd"} {
				t.Run("chd/"+medium, func(t *testing.T) { checkFormatWorkflow(t, rc.CHD, medium) })
			}
			continue
		}
		t.Run(string(format), func(t *testing.T) { checkFormatWorkflow(t, format, "") })
	}
}

func checkFormatWorkflow(t *testing.T, format rc.Format, medium string) {
	t.Helper()
	switch format {
	case rc.ZIP, rc.GZIP, rc.CSO, rc.ZSO, rc.ECM, rc.CCI:
	default:
		if os.Getenv("ROMCOMPRESSOR_TEST_FORMAT_WORKFLOWS") != "1" {
			t.Skip("requires external engines or full-size images; see docs/development.md")
		}
	}
	ctx := t.Context()
	inputs, work := t.TempDir(), t.TempDir()
	source := matrixSource(t, inputs, format, medium)
	originals := workflowContents(t, inputs)
	opts := matrixOptions()
	opts.Format, opts.Media = format, medium
	checkPlan := func(src string, want rc.Format) {
		t.Helper()
		plan, err := rc.PlanConversions(ctx, src, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range plan.Targets {
			if target.Format == want && target.Media == medium && !target.ExtractAll && target.Available {
				return
			}
		}
		t.Fatalf("plan lacks available %s %s conversion: %+v", want, medium, plan.Targets)
	}

	checkPlan(source, format)
	packed := filepath.Join(work, "packed"+matrixExtension(format))
	jobs := rc.Batch(ctx, []rc.Job{{Source: source, Output: packed, Options: opts}})
	if len(jobs) != 1 || jobs[0].Result == nil || !jobs[0].Result.Verified || jobs[0].Result.Format != format {
		t.Fatalf("create through batch: %+v", jobs)
	}
	info, err := rc.InspectWithOptions(ctx, packed, opts)
	if err != nil || info.Format != format || info.Media != medium {
		t.Fatalf("inspect: %+v, %v", info, err)
	}
	verify := opts
	verify.TempDir = t.TempDir()
	if err := rc.Verify(ctx, packed, verify); err != nil {
		t.Fatalf("standalone verification: %v", err)
	}
	if files, err := os.ReadDir(verify.TempDir); err != nil || len(files) != 0 {
		t.Fatalf("verification scratch left entries: %v, %v", files, err)
	}
	// Keep the actual signature and opening bytes to exercise incomplete
	// containers under the same filename and verification options.
	broken := filepath.Join(t.TempDir(), filepath.Base(packed))
	header, err := os.Open(packed)
	if err != nil {
		t.Fatal(err)
	}
	prefix, readErr := io.ReadAll(io.LimitReader(header, 32))
	if err := errors.Join(readErr, header.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, prefix, 0600); err != nil {
		t.Fatal(err)
	}
	if err := rc.Verify(ctx, broken, verify); err == nil {
		t.Fatal("standalone verification accepted a truncated container")
	}
	checkPlan(packed, rc.Raw)
	opts.Format = rc.Raw
	restored := filepath.Join(work, "restored"+matrixRawExtension(format, medium))
	if _, err := rc.Convert(ctx, broken, restored, opts); err == nil {
		t.Fatal("restoration accepted a truncated container")
	}
	limited := opts
	limited.MaxOutput = 1
	if _, err := rc.Convert(ctx, packed, restored, limited); err == nil {
		t.Fatal("restore succeeded with a one-byte output limit")
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 1 {
		t.Fatalf("failed restore leaked files: %v, %v", entries, err)
	}
	out, err := rc.Convert(ctx, packed, restored, opts)
	if err != nil || !out.Verified {
		t.Fatalf("restore: %+v, %v", out, err)
	}
	if _, err := rc.Convert(ctx, packed, restored, opts); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination: %v", err)
	}
	switch {
	case format == rc.WUA:
		children, err := os.ReadDir(restored)
		if err != nil || len(children) != 1 || children[0].Name() != "000500001234abcd_v16" {
			t.Fatalf("restored title: %v, %v", children, err)
		}
		if !maps.Equal(workflowContents(t, source), workflowContents(t, filepath.Join(restored, children[0].Name()))) {
			t.Fatal("restored title paths or contents differ")
		}
	case format == rc.PBP || medium == "cd":
		var tracks []string
		err := disc.VisitReferences(restored, func(path string, _ os.FileInfo) error {
			tracks = append(tracks, path)
			return nil
		})
		if err != nil || len(tracks) != 1 {
			t.Fatalf("restored CUE references: %v, %v", tracks, err)
		}
		if err := fileops.CompareFiles(ctx, filepath.Join(inputs, "track.bin"), tracks[0]); err != nil {
			t.Fatal(err)
		}
	default:
		if err := fileops.CompareFiles(ctx, source, restored); err != nil {
			t.Fatal(err)
		}
	}
	if !maps.Equal(originals, workflowContents(t, inputs)) {
		t.Fatal("source files, descriptor companions or directories changed")
	}
}

type workflowEntry struct {
	directory bool
	digest    [32]byte
}

// Include empty directories and file hashes without loading images into memory.
func workflowContents(t *testing.T, root string) map[string]workflowEntry {
	t.Helper()
	entries := make(map[string]workflowEntry)
	err := fs.WalkDir(os.DirFS(root), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		e := workflowEntry{directory: d.IsDir()}
		if !e.directory {
			e.digest, err = fileops.Digest(t.Context(), filepath.Join(root, filepath.FromSlash(path)))
		}
		entries[path] = e
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
