package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingCLIOutput struct {
	calls int
	short bool
}

func (w *failingCLIOutput) Write(p []byte) (int, error) {
	w.calls++
	if w.short {
		return len(p) / 2, nil
	}
	return len(p) / 2, io.ErrClosedPipe
}

func TestHumanOutputFailuresReturnNonzero(t *testing.T) {
	for _, command := range []string{"help", "formats", "inspect", "batch"} {
		for _, short := range []bool{false, true} {
			name := command + "/error"
			if short {
				name = command + "/short write"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, "game.gba")
				original := []byte("generated game bytes")
				if err := os.WriteFile(source, original, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{command}
				switch command {
				case "inspect":
					args = append(args, source)
				case "batch":
					manifest := filepath.Join(dir, "jobs.json")
					if err := os.WriteFile(manifest, []byte(`{"jobs":[{"source":"game.gba","output":"game.zip","format":"zip"}]}`), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, manifest)
				}
				output := &failingCLIOutput{short: short}
				var stderr bytes.Buffer
				if code := run(context.Background(), args, output, &stderr); code != 1 {
					t.Fatalf("output failure returned status %d: %s", code, &stderr)
				}
				want := io.ErrClosedPipe
				if short {
					want = io.ErrShortWrite
				}
				if output.calls != 1 || !strings.Contains(stderr.String(), want.Error()) {
					t.Fatalf("missing output diagnosis or retried failed stream: calls=%d, stderr=%s", output.calls, &stderr)
				}
				if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
					t.Fatalf("output failure changed original: %v", err)
				}
				if command == "batch" {
					if _, err := os.Stat(filepath.Join(dir, "game.zip")); err != nil {
						t.Fatalf("reporting failure removed a completed output: %v", err)
					}
				}
			})
		}
	}
}

func TestFailedJSONOutputDoesNotAppendAnotherDocument(t *testing.T) {
	for _, args := range [][]string{{"formats", "--json"}, {"unknown", "--json"}} {
		output := &failingCLIOutput{}
		var stderr bytes.Buffer
		if code := run(context.Background(), args, output, &stderr); code != 1 || output.calls != 1 {
			t.Fatalf("failed JSON stream was retried: args=%v, status=%d, writes=%d", args, code, output.calls)
		}
		if !strings.Contains(stderr.String(), io.ErrClosedPipe.Error()) {
			t.Fatalf("output failure was not diagnosed: %s", &stderr)
		}
	}
}

func TestOutputWriterPreservesFailureIdentity(t *testing.T) {
	output := &checkedOutput{writer: &failingCLIOutput{}}
	if _, err := output.Write([]byte("one")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost first writer error: %v", err)
	}
	if _, err := output.Write([]byte("two")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost original writer error after failure: %v", err)
	}
}

func TestZIPDirectoryDefaultOutputUsesSibling(t *testing.T) {
	for _, spelling := range []string{"trailing separator", "dot", "parent", "dot with playlist suffix"} {
		t.Run(spelling, func(t *testing.T) {
			dir := t.TempDir()
			folderName := "Game folder"
			if spelling == "dot with playlist suffix" {
				folderName = "Game.m3u"
			}
			folder := filepath.Join(dir, folderName)
			if err := os.Mkdir(folder, 0700); err != nil {
				t.Fatal(err)
			}
			sourceFile := filepath.Join(folder, "game.gba")
			original := []byte("generated cartridge bytes")
			if err := os.WriteFile(sourceFile, original, 0600); err != nil {
				t.Fatal(err)
			}
			source := folder + string(os.PathSeparator)
			switch spelling {
			case "dot", "dot with playlist suffix":
				t.Chdir(folder)
				source = "."
			case "parent":
				nested := filepath.Join(folder, "nested")
				if err := os.Mkdir(nested, 0700); err != nil {
					t.Fatal(err)
				}
				t.Chdir(nested)
				source = ".."
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{"compress", source, "-f", "zip", "--json"}, &stdout, &stderr); code != 0 {
				t.Fatalf("directory spelling %q failed: status=%d, stdout=%s, stderr=%s", source, code, &stdout, &stderr)
			}
			if _, err := os.Stat(folder + ".zip"); err != nil {
				t.Fatalf("default output was not the source's sibling: %v", err)
			}
			if actual, err := os.ReadFile(sourceFile); err != nil || !bytes.Equal(actual, original) {
				t.Fatalf("directory compression changed original: %v", err)
			}
		})
	}
}

func TestDescriptorNamedFoldersHaveArchiveDefaults(t *testing.T) {
	for _, suffix := range []string{".m3u", ".m3u8", ".cue", ".gdi"} {
		for _, trailing := range []bool{false, true} {
			name := suffix + "/direct"
			if trailing {
				name = suffix + "/trailing separator"
			}
			t.Run(name, func(t *testing.T) {
				folder := filepath.Join(t.TempDir(), "Game"+suffix)
				if err := os.Mkdir(folder, 0700); err != nil {
					t.Fatal(err)
				}
				original := []byte("generated game bytes")
				sourceFile := filepath.Join(folder, "game.gba")
				if err := os.WriteFile(sourceFile, original, 0600); err != nil {
					t.Fatal(err)
				}
				source := folder
				if trailing {
					source += string(os.PathSeparator)
				}
				var stdout, stderr bytes.Buffer
				if code := run(context.Background(), []string{"compress", source, "-f", "zip", "--json"}, &stdout, &stderr); code != 0 {
					t.Fatalf("folder suffix selected a file workflow: status=%d, stdout=%s, stderr=%s", code, &stdout, &stderr)
				}
				if _, err := os.Stat(folder + ".zip"); err != nil {
					t.Fatalf("archive default did not preserve the directory name: %v", err)
				}
				if actual, err := os.ReadFile(sourceFile); err != nil || !bytes.Equal(actual, original) {
					t.Fatalf("directory conversion changed original: %v", err)
				}
			})
		}
	}
}
