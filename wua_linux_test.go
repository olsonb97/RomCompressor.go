package romcompressor

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"golang.org/x/sys/unix"
)

func TestWUARejectsArchiveChangedByExtractionEngine(t *testing.T) {
	dir := t.TempDir()
	title := filepath.Join(dir, testWUATitle)
	files := map[string]string{
		"code/app.xml":      "<app><title_id>000500001234abcd</title_id><title_version>0010</title_version></app>",
		"code/cos.xml":      "<app/>",
		"content/asset.bin": "generated game assets",
		"meta/meta.xml":     "<menu/>",
	}
	for name, data := range files {
		path := filepath.Join(title, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	archive := rawWUAFixture(t, "asset.bin")
	fixture := filepath.Join(dir, "fixture.wua")
	if err := os.WriteFile(fixture, archive, 0600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(dir, "zarchive")
	script := `#!/bin/sh
set -eu
if [ -d "$1" ]; then
    cp "$ROMCOMPRESSOR_TEST_WUA_ARCHIVE" "$2"
else
    mkdir "$2"
    cp -R "$ROMCOMPRESSOR_TEST_WUA_TREE" "$2/"
    touch -r "$1" "$ROMCOMPRESSOR_TEST_WUA_TIMES"
    printf '!' | dd of="$1" bs=1 conv=notrunc 2>/dev/null
    touch -r "$ROMCOMPRESSOR_TEST_WUA_TIMES" "$1"
    rm "$ROMCOMPRESSOR_TEST_WUA_TIMES"
fi
`
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROMCOMPRESSOR_TEST_WUA_ARCHIVE", fixture)
	t.Setenv("ROMCOMPRESSOR_TEST_WUA_TREE", title)
	t.Setenv("ROMCOMPRESSOR_TEST_WUA_TIMES", filepath.Join(dir, "timestamp"))
	for _, test := range []struct {
		name, source, output string
		format               Format
	}{
		{"pack", title, "output.wua", WUA},
		{"unpack", fixture, "output", Raw},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(dir, test.output)
			_, err := Convert(context.Background(), test.source, output, Options{
				Format: test.format, Tools: map[string]string{"zarchive": tool},
			})
			if err == nil || !strings.Contains(err.Error(), "WUA archive checksum mismatch") {
				t.Fatalf("Convert = %v; want changed staged archive rejection", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("unexpected publication: %v", err)
			}
			if data, err := os.ReadFile(fixture); err != nil || !bytes.Equal(data, archive) {
				t.Fatalf("source fixture changed: %v", err)
			}
		})
	}
}

func TestWUASnapshotReadsRejectFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.xml")
	if err := os.WriteFile(path, []byte("app"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := fileops.SnapshotStat(path, os.Lstat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name string
		read func(context.Context) error
	}{
		{"snapshot streaming", func(ctx context.Context) error {
			return streamSnapshotFile(ctx, io.Discard, path, info, make([]byte, 64))
		}},
		{"title metadata", func(ctx context.Context) error {
			_, err := readWUATitleName(ctx, path)
			return err
		}},
	} {
		t.Run(check.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- check.read(ctx) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "regular file") {
					t.Fatalf("read = %v; want regular file rejection", err)
				}
			case <-ctx.Done():
				t.Fatal("WUA read blocked while opening a FIFO")
			}
		})
	}
}
