package romcompressor

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestZIPTreeRetainsOwnerExecuteWithoutSpecialPermissions(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "launchers.zip")
	contents := []byte("#!/bin/sh\nprintf 'generated launcher\\n'\n")
	writeZIPTreeFixture(t, source, func(writer *zip.Writer) error {
		for name, mode := range map[string]os.FileMode{
			"launch.sh":     0755,
			"privileged.sh": os.ModeSetuid | os.ModeSetgid | 0777,
			"readme.txt":    0666,
		} {
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(mode)
			member, err := writer.CreateHeader(header)
			if err != nil {
				return err
			}
			if _, err := member.Write(contents); err != nil {
				return err
			}
		}
		return nil
	})
	output := filepath.Join(dir, "restored")
	if _, err := Convert(context.Background(), source, output, Options{Format: Raw, ExtractAll: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"launch.sh", "privileged.sh", "readme.txt"} {
		path := filepath.Join(output, name)
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, contents) {
			t.Fatalf("restored %s: %q, %v", name, data, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Fatalf("restored special permissions: %s", info.Mode())
		}
		if runtime.GOOS != "windows" {
			want := os.FileMode(0600)
			if name != "readme.txt" {
				want = 0700
			}
			if info.Mode().Perm() != want {
				t.Errorf("%s mode = %s, want %s", name, info.Mode().Perm(), want)
			}
		}
	}
}

func TestZIPTreeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "title")
	if err := os.MkdirAll(filepath.Join(source, "assets", "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{
		"root.bin":         bytes.Repeat([]byte{9, 1, 3, 8}, 2000),
		"assets/café.txt":  []byte("nested file"),
		"assets/blank.bin": {},
	}
	for name, content := range data {
		if err := os.WriteFile(filepath.Join(source, filepath.FromSlash(name)), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	archive := filepath.Join(dir, "title.zip")
	result, err := Convert(ctx, source, archive, Options{Format: ZIP})
	if err != nil || !result.Verified || result.InputBytes != 8011 {
		t.Fatalf("pack folder = %+v, %v", result, err)
	}
	if err := Verify(ctx, archive, Options{}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored")
	result, err = Convert(ctx, archive, restored, Options{Format: Raw, ExtractAll: true})
	if err != nil || !result.Verified || result.OutputBytes != 8011 {
		t.Fatalf("extract folder = %+v, %v", result, err)
	}
	for _, tree := range []string{source, restored} {
		for name, expected := range data {
			actual, err := os.ReadFile(filepath.Join(tree, filepath.FromSlash(name)))
			if err != nil || !bytes.Equal(expected, actual) {
				t.Fatalf("incorrect path %q in %s: %v", name, tree, err)
			}
		}
		if children, err := os.ReadDir(filepath.Join(tree, "assets", "empty")); err != nil || len(children) != 0 {
			t.Fatalf("empty directory not preserved: %v, %v", children, err)
		}
	}
	// Existing callers can still restore just one selected entry.
	entry := filepath.Join(dir, "single.txt")
	if _, err := Convert(ctx, archive, entry, Options{Format: Raw, Entry: "assets/café.txt"}); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(entry); err != nil || string(actual) != "nested file" {
		t.Fatalf("selected entry = %q, %v", actual, err)
	}
}

func TestZIPTreeRejectsUnsafePaths(t *testing.T) {
	for name, entries := range map[string][]string{
		"traversal":      {"../escaped.bin"},
		"absolute":       {"/absolute.bin"},
		"drive":          {"C:/abspath.bin"},
		"backslash":      {".\\..\\escaped.bin"},
		"literal slash":  {`game\save.bin`},
		"device":         {"nested/CoN .bin"},
		"alternate data": {"nested/file.bin:stream"},
		"same path":      {"game.bin", "game.bin"},
		"case collision": {"game.bin", "GAME.BIN"},
		"unicode fold":   {"σ.bin", "ς.bin"},
		"parent clash":   {"GAME.bin", "game.bin/file"},
		"implicit clash": {"assets/a.bin", "ASSETS/b.bin"},
		"duplicate dir":  {"assets/", "assets/"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "untrusted.zip")
			writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
				for _, name := range entries {
					if _, err := w.Create(name); err != nil {
						return err
					}
				}
				return nil
			})
			requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true})
		})
	}
}

func TestZIPTreeRejectsLinkCRCAndExpansion(t *testing.T) {
	t.Run("regular and implicit directory", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "implicit.zip")
		writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
			if _, err := w.Create("sub/item"); err != nil {
				return err
			}
			_, err := w.Create("sub/")
			return err
		})
		output := filepath.Join(dir, "restored")
		if _, err := Convert(context.Background(), archive, output, Options{Format: Raw, ExtractAll: true}); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(filepath.Join(output, "sub", "item")); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("implicit parent not restored: %v, %v", info, err)
		}
	})
	t.Run("link", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "link.zip")
		writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
			header := &zip.FileHeader{Name: "link"}
			header.SetMode(os.ModeSymlink | 0777)
			_, err := w.CreateHeader(header)
			return err
		})
		requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true})
	})
	t.Run("zero CRC mismatch", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "bad-crc.zip")
		writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
			dst, err := w.CreateRaw(&zip.FileHeader{Name: "corrupt", Method: zip.Store, CompressedSize64: 3, UncompressedSize64: 3})
			if err != nil {
				return err
			}
			_, err = dst.Write([]byte("bad"))
			return err
		})
		requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true})
		requireZIPTreeBlocked(t, archive, Options{Format: Raw})
		if err := Verify(context.Background(), archive, Options{}); !errors.Is(err, zip.ErrChecksum) {
			t.Fatalf("expected ZIP checksum rejection, got %v", err)
		}
	})
	t.Run("combined expansion", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "expansion.zip")
		writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
			for i := 0; i < 2; i++ {
				dst, err := w.Create(fmt.Sprintf("f%d", i))
				if err != nil {
					return err
				}
				if _, err := dst.Write([]byte("four")); err != nil {
					return err
				}
			}
			return nil
		})
		requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true, MaxOutput: 7})
	})
}

func TestZIPRejectsUnsupportedMemberMetadata(t *testing.T) {
	ctx := context.Background()
	t.Run("encrypted file", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "encrypted.zip")
		data := []byte("game data")
		writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
			dst, err := w.CreateRaw(&zip.FileHeader{
				Name: "game.bin", Flags: 1, Method: zip.Store, CRC32: crc32.ChecksumIEEE(data),
				CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
			})
			if err != nil {
				return err
			}
			_, err = dst.Write(data)
			return err
		})
		if err := Verify(ctx, archive, Options{}); err == nil || !strings.Contains(err.Error(), "encrypted") {
			t.Fatalf("expected rejection of encrypted ZIP entry, got %v", err)
		}
		requireZIPTreeBlocked(t, archive, Options{Format: Raw})
		requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true})
	})
	t.Run("directory claiming data", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "invalid-directory.zip")
		writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
			if _, err := w.Create("game.bin"); err != nil {
				return err
			}
			_, err := w.CreateRaw(&zip.FileHeader{Name: "assets/", UncompressedSize64: 1})
			return err
		})
		if err := Verify(ctx, archive, Options{}); err == nil || !strings.Contains(err.Error(), "invalid ZIP directory") {
			t.Fatalf("expected rejection of ZIP directory claiming data, got %v", err)
		}
		if err := Verify(ctx, archive, Options{Entry: "game.bin"}); err != nil {
			t.Fatalf("verify unaffected entry: %v", err)
		}
		requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true})
	})
}

func TestZIPRejectsInvalidCompressedMemberExtent(t *testing.T) {
	data := []byte("game data")
	var compressed bytes.Buffer
	deflate, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deflate.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := deflate.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		extra []byte
		err   string
	}{
		{"overlaps directory", nil, "ZIP member extent"},
		{"trailing compressed bytes", []byte{0xfe}, "trailing data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "invalid.zip")
			writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
				dst, err := w.CreateRaw(&zip.FileHeader{
					Name: "game.bin", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(data),
					CompressedSize64: uint64(compressed.Len()) + 1, UncompressedSize64: uint64(len(data)),
				})
				if err != nil {
					return err
				}
				if _, err := dst.Write(compressed.Bytes()); err != nil {
					return err
				}
				_, err = dst.Write(test.extra)
				return err
			})
			if err := Verify(context.Background(), archive, Options{}); err == nil || !strings.Contains(err.Error(), test.err) {
				t.Fatalf("expected rejection of bad ZIP compressed size, got %v", err)
			}
			requireZIPTreeBlocked(t, archive, Options{Format: Raw})
			requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true})
		})
	}
}

func TestZIPTreeRefusesInvalidStreamingMetadata(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bad-descriptor.zip")
	writeZIPTreeFixture(t, archive, func(w *zip.Writer) error {
		file, err := w.Create("folder/game.bin")
		if err != nil {
			return err
		}
		_, err = file.Write([]byte("game data"))
		return err
	})
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	directory := int(binary.LittleEndian.Uint32(data[len(data)-6:]))
	data[directory-4]++ // Corrupt the uncompressed size in the data descriptor only.
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	requireZIPTreeBlocked(t, archive, Options{Format: Raw, ExtractAll: true})
}

func TestZIPTreeRefusesChangedSourceOrOccupiedDestination(t *testing.T) {
	t.Run("source changed at publication", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "title")
		if err := os.Mkdir(source, 0700); err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(source, "metadata")
		if err := os.WriteFile(input, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(dir, "title.zip")
		_, err := Convert(context.Background(), source, output, Options{Format: ZIP, Progress: func(p Progress) {
			if p.Phase == "publishing" {
				if err := os.WriteFile(input, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}})
		if err == nil || !strings.Contains(err.Error(), "source changed") {
			t.Fatalf("expected rejection of source mutation, got %v", err)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("published ZIP despite changed source: %v", err)
		}
	})
	t.Run("destination taken during extraction", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "empty.zip")
		writeZIPTreeFixture(t, archive, func(*zip.Writer) error { return nil })
		target := filepath.Join(dir, "taken")
		_, err := Convert(context.Background(), archive, target, Options{Format: Raw, ExtractAll: true, Progress: func(p Progress) {
			if p.Phase == "publishing" {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "owner"), []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}})
		if err == nil {
			t.Fatal("whole ZIP extraction replaced a destination directory")
		}
		if data, err := os.ReadFile(filepath.Join(target, "owner")); err != nil || string(data) != "untouched" {
			t.Fatalf("destination contents changed: %q, %v", data, err)
		}
	})
}

func TestZIPTreeRefusesStagingChangesFromProgress(t *testing.T) {
	for _, extract := range []bool{false, true} {
		t.Run(fmt.Sprintf("extract=%t", extract), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source")
			output := filepath.Join(dir, "packed.zip")
			name := filepath.Base(output)
			opts := Options{Format: ZIP}
			if extract {
				source = filepath.Join(dir, "source.zip")
				output = filepath.Join(dir, "restored")
				name = "game.bin"
				opts = Options{Format: Raw, ExtractAll: true}
				writeZIPTreeFixture(t, source, func(w *zip.Writer) error {
					file, err := w.Create(name)
					if err != nil {
						return err
					}
					_, err = file.Write([]byte("original"))
					return err
				})
			} else {
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "game.bin"), []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			opts.Progress = func(p Progress) {
				if p.Phase != "publishing" {
					return
				}
				staged, err := filepath.Glob(filepath.Join(dir, ".romcompressor-zip-*", name))
				if err != nil || len(staged) != 1 {
					t.Fatalf("locate staged ZIP output: %v, %v", staged, err)
				}
				if err := os.WriteFile(staged[0], []byte("modified"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Convert(context.Background(), source, output, opts); err == nil {
				t.Fatal("published modified staging contents")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("staged contents were published: %v", err)
			}
		})
	}
}

func TestZIPTreeCancellationAndSourcePaths(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "title")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	nested := filepath.Join(source, "nested.zip")
	if _, err := Convert(ctx, source, nested, Options{Format: ZIP}); err == nil || !strings.Contains(err.Error(), "outside the source directory") {
		t.Fatalf("expected nested output rejection, got %v", err)
	}
	t.Run("empty tree cancelled at publication", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		output := filepath.Join(dir, "cancelled.zip")
		_, err := Convert(ctx, source, output, Options{Format: ZIP, Progress: func(p Progress) {
			if p.Phase == "publishing" {
				cancel()
			}
		}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("published a cancelled archive: %v", err)
		}
	})
	t.Run("ZIP source symbolic links", func(t *testing.T) {
		target := filepath.Join(dir, "outside.bin")
		if err := os.WriteFile(target, []byte("external"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(source, "link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		output := filepath.Join(dir, "linked.zip")
		if _, err := Convert(ctx, source, output, Options{Format: ZIP}); err == nil || !strings.Contains(err.Error(), "link or special file") {
			t.Fatalf("expected source link rejection, got %v", err)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("published an archive containing a symbolic link: %v", err)
		}
	})
}

func writeZIPTreeFixture(t *testing.T, path string, fill func(*zip.Writer) error) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	if err := fill(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func requireZIPTreeBlocked(t *testing.T, archive string, opts Options) {
	t.Helper()
	dir := filepath.Dir(archive)
	output := filepath.Join(dir, "published")
	if _, err := Convert(context.Background(), archive, output, opts); err == nil {
		t.Fatal("unsafe ZIP tree was accepted")
	}
	children, err := os.ReadDir(dir)
	if err != nil || len(children) != 1 || children[0].Name() != filepath.Base(archive) {
		t.Fatalf("unexpected output or staging files after failure: %v, %v", children, err)
	}
}
