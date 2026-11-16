package romcompressor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDescriptorNamedDirectoriesUseFolderConversion(t *testing.T) {
	for _, suffix := range []string{".m3u", ".m3u8", ".cue", ".gdi"} {
		for _, trailing := range []bool{false, true} {
			name := suffix + "/direct"
			if trailing {
				name = suffix + "/trailing separator"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				folder := filepath.Join(dir, "Game"+suffix)
				if err := os.Mkdir(folder, 0700); err != nil {
					t.Fatal(err)
				}
				payload := []byte("generated game bytes")
				input := filepath.Join(folder, "game.bin")
				if err := os.WriteFile(input, payload, 0600); err != nil {
					t.Fatal(err)
				}
				source := folder
				if trailing {
					source += string(os.PathSeparator)
				}
				archive := filepath.Join(dir, "game.zip")
				result, err := Convert(context.Background(), source, archive, Options{Format: ZIP})
				if err != nil || !result.Verified || result.InputBytes != int64(len(payload)) {
					t.Fatalf("folder suffix selected a file workflow: %+v, %v", result, err)
				}
				info, err := Inspect(context.Background(), archive)
				if err != nil || info.Format != ZIP || len(info.Entries) != 1 || info.Entries[0].Name != "game.bin" {
					t.Fatalf("directory contents were not archived: %+v, %v", info, err)
				}
				if actual, err := os.ReadFile(input); err != nil || !bytes.Equal(actual, payload) {
					t.Fatalf("directory conversion changed original: %v", err)
				}
			})
		}
	}
}

func TestDescriptorNamedDirectoriesReachWUALayoutValidation(t *testing.T) {
	for _, suffix := range []string{".m3u", ".m3u8", ".cue", ".gdi"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "Game"+suffix)
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "game.wua")
			_, err := Convert(context.Background(), source, output, Options{Format: WUA})
			if err == nil || !strings.Contains(err.Error(), "WUA needs at least one decrypted Wii U title") {
				t.Fatalf("folder did not reach WUA layout validation: %v", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("invalid WUA folder left an output: %v", err)
			}
		})
	}
}

func TestDescriptorFilesRetainCompleteSetRequirements(t *testing.T) {
	for _, suffix := range []string{".m3u", ".m3u8", ".cue", ".gdi"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game"+suffix)
			original := []byte("generated descriptor bytes")
			if err := os.WriteFile(source, original, 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "game.zip")
			if _, err := Convert(context.Background(), source, output, Options{Format: ZIP}); err == nil {
				t.Fatal("descriptor was silently archived without its referenced files")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("refused descriptor left an output: %v", err)
			}
			if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
				t.Fatalf("descriptor conversion changed original: %v", err)
			}
		})
	}
}

func TestDescriptorNamedDirectorySymlinksRemainRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".m3u", ".m3u8", ".cue", ".gdi"} {
		link := filepath.Join(dir, "link"+suffix)
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		output := filepath.Join(dir, "output"+suffix+".zip")
		if _, err := Convert(context.Background(), link, output, Options{Format: ZIP}); err == nil {
			t.Fatalf("directory symlink %s was accepted", suffix)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("directory symlink left an output: %v", err)
		}
	}
}

func TestPlaylistFileSuffixCannotBeHiddenBySeparator(t *testing.T) {
	for _, suffix := range []string{".m3u", ".m3u8"} {
		for _, format := range []Format{ZIP, WUA} {
			t.Run(suffix+"/"+string(format), func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, "game"+suffix)
				original := []byte("disc.iso\n")
				if err := os.WriteFile(source, original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "disc.iso"), make([]byte, 4096), 0600); err != nil {
					t.Fatal(err)
				}
				output := filepath.Join(dir, "output."+string(format))
				_, err := Convert(context.Background(), source+string(os.PathSeparator), output, Options{Format: format})
				if err == nil || !strings.Contains(err.Error(), "playlist conversion requires") {
					t.Fatalf("separator bypassed complete-playlist rules: %v", err)
				}
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatalf("refused playlist left an output: %v", err)
				}
				if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
					t.Fatalf("refused playlist changed original: %v", err)
				}
			})
		}
	}
}

func TestConvertStillDispatchesPlaylistFiles(t *testing.T) {
	source, payloads := playlistFixture(t)
	output := filepath.Join(filepath.Dir(source), "converted.m3u")
	result, err := Convert(context.Background(), source, output, Options{Format: CSO})
	if err != nil || !result.Verified {
		t.Fatalf("playlist file was not converted as a complete set: %+v, %v", result, err)
	}
	contents, err := os.ReadFile(output)
	if err != nil || len(strings.Fields(string(contents))) != len(payloads) {
		t.Fatalf("playlist conversion lost disc references: %q, %v", contents, err)
	}
}
