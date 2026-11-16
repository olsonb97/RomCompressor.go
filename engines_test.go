package romcompressor

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSparseDolphinRestoreProgressPrecedesStaging(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.ciso")
	if err := os.WriteFile(source, []byte("CISO"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reported bool
	opts := Options{Format: RVZ, Tools: map[string]string{"dolphin-tool": os.Args[0]}, MaxOutput: defaultMaxOutput, Progress: func(p Progress) {
		if p.Phase != "restoring disc image" {
			return
		}
		reported = true
		paths, err := filepath.Glob(filepath.Join(dir, "restored-*.iso"))
		if err != nil || len(paths) != 0 {
			t.Fatalf("restore exposed a temporary disc to the callback: %v, %v", paths, err)
		}
		cancel()
	}}
	err := convertSparseISOForDolphin(ctx, source, filepath.Join(dir, "output.rvz"), CISO, opts, nil)
	if !reported || !errors.Is(err, context.Canceled) {
		t.Fatalf("convertSparseISOForDolphin = %v, progress reported = %v", err, reported)
	}
}

func TestSparseDolphinMissingDependencyPrecedesRestoration(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.ciso")
	const blockSize = 512 << 10
	volume := make([]byte, 32768+blockSize)
	copy(volume, "CISO")
	binary.LittleEndian.PutUint32(volume[4:], blockSize)
	volume[8] = 1
	binary.BigEndian.PutUint32(volume[32768+0x1c:], 0xc2339f3d)
	if err := os.WriteFile(source, volume, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(context.Background(), source); err != nil || info.Format != CISO {
		t.Fatalf("invalid synthetic CISO: %v, %v", info, err)
	}
	var restoring bool
	_, err := Convert(context.Background(), source, filepath.Join(dir, "output.rvz"), Options{
		Format: RVZ, MaxOutput: defaultMaxOutput,
		Tools: map[string]string{"dolphin-tool": filepath.Join(dir, "missing-dolphin-tool")},
		Progress: func(p Progress) {
			if p.Phase == "restoring disc image" {
				restoring = true
			}
		},
	})
	var missing *DependencyError
	if !errors.As(err, &missing) || missing.Engine != "dolphin-tool" || !missing.Configured {
		t.Fatalf("expected configured dependency error before restoration, got %v", err)
	}
	if restoring {
		t.Fatal("began restoring a disc without the required engine")
	}
	_, err = Convert(context.Background(), source, filepath.Join(dir, "output.rvz"), Options{
		Format: RVZ, MaxOutput: 1, Tools: map[string]string{"dolphin-tool": os.Args[0]},
		Progress: func(Progress) { t.Fatal("known size preflight invoked progress") },
	})
	if err == nil || !strings.Contains(err.Error(), "expanded size limit") {
		t.Fatalf("expected known expansion rejection before restoration: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(source) {
		t.Fatalf("failed preflight left output or scratch files: %v, %v", entries, err)
	}
}
