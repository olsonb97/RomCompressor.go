package romcompressor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscSidecarFollowsRenamedConversionAndRestoration(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.iso")
	sbi := []byte("SBI\x00synthetic subchannel data")
	if err := os.WriteFile(source, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "game.SBI"), sbi, 0600); err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(dir, "renamed.cso")
	if _, err := Convert(context.Background(), source, packed, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.iso")
	if _, err := Convert(context.Background(), packed, restored, Options{Format: Raw}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"game.SBI", "renamed.sbi", "restored.sbi"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, sbi) {
			t.Fatalf("companion %s changed: %v", name, err)
		}
	}
	// Keeping the output beside the source must reuse its existing companion.
	if _, err := Convert(context.Background(), source, filepath.Join(dir, "game.zso"), Options{Format: ZSO}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedSBIReplacedDuringConversion(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "original.iso")
	sidecar := filepath.Join(dir, "original.sbi")
	shared := filepath.Join(dir, "restored.sbi")
	output := filepath.Join(dir, "restored.cso")
	if err := os.WriteFile(source, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte("SBI\x00original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(sidecar, shared); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	replaced := false
	replacement := []byte("SBI\x00unrelated")
	_, err := Convert(t.Context(), source, output, Options{Format: CSO, Progress: func(p Progress) {
		if replaced || p.Phase != "converting" {
			return
		}
		replaced = true
		if err := os.Remove(shared); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shared, replacement, 0600); err != nil {
			t.Fatal(err)
		}
	}})
	if err == nil || !replaced {
		t.Fatalf("conversion accepted a changed companion: %v", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists: %v", err)
	}
	if data, err := os.ReadFile(shared); err != nil || !bytes.Equal(data, replacement) {
		t.Fatalf("replacement companion changed: %v", err)
	}
}

func TestDiscSidecarPreservesEverySuffixCase(t *testing.T) {
	for _, suffix := range []string{".sbi", ".sbI", ".sBi", ".sBI", ".Sbi", ".SbI", ".SBi", ".SBI"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game.iso")
			sidecar := filepath.Join(dir, "game"+suffix)
			raw := bytes.Repeat([]byte{0x13, 0x47}, 4096)
			sbi := []byte("SBI\x00generated subchannel data")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sidecar, sbi, 0600); err != nil {
				t.Fatal(err)
			}
			packed := filepath.Join(dir, "packed.cso")
			if _, err := Convert(context.Background(), source, packed, Options{Format: CSO}); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{sidecar, filepath.Join(dir, "packed.sbi")} {
				if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, sbi) {
					t.Fatalf("SBI companion was omitted or changed: %v", err)
				}
			}
			if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, raw) {
				t.Fatalf("source image changed: %v", err)
			}
			// Conversions that retain the stem share the existing unchanged SBI.
			if _, err := Convert(context.Background(), source, filepath.Join(dir, "game.zso"), Options{Format: ZSO}); err != nil {
				t.Fatalf("existing source companion was not shared: %v", err)
			}
		})
	}
}

func TestDiscSidecarEverySuffixCasePreventsDestinationCollision(t *testing.T) {
	for _, suffix := range []string{".sbi", ".sbI", ".sBi", ".sBI", ".Sbi", ".SbI", ".SBi", ".SBI"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game.iso")
			inputSBI := filepath.Join(dir, "game.sbi")
			output := filepath.Join(dir, "packed.cso")
			conflict := filepath.Join(dir, "packed"+suffix)
			raw := make([]byte, 8192)
			sbi := []byte("SBI\x00generated source companion")
			existing := []byte("SBI\x00existing destination companion")
			for path, data := range map[string][]byte{source: raw, inputSBI: sbi, conflict: existing} {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Convert(context.Background(), source, output, Options{Format: CSO})
			var occupied *os.PathError
			if !errors.Is(err, os.ErrExist) || !errors.As(err, &occupied) {
				t.Fatalf("SBI conflict lost filesystem error identity: %v", err)
			}
			selected, statErr := os.Stat(occupied.Path)
			original, originalErr := os.Stat(conflict)
			if statErr != nil || originalErr != nil || !os.SameFile(selected, original) {
				t.Fatalf("conflict error did not identify the occupied companion: %v", err)
			}
			for path, expected := range map[string][]byte{source: raw, inputSBI: sbi, conflict: existing} {
				if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("conflict changed an original: %v", err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 3 {
				t.Fatalf("conflict left output or staging files: %v, %v", entries, err)
			}
		})
	}
}

func TestDiscSidecarCollisionAndMutationPublishNothing(t *testing.T) {
	for _, scenario := range []string{"collision", "mutation", "invalid", "invalid shared", "limit"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game.iso")
			inputSBI := filepath.Join(dir, "game.sbi")
			output := filepath.Join(dir, "new.cso")
			outputSBI := filepath.Join(dir, "new.sbi")
			if err := os.WriteFile(source, make([]byte, 8192), 0600); err != nil {
				t.Fatal(err)
			}
			data := []byte("SBI\x00original")
			opts := Options{Format: CSO}
			switch scenario {
			case "collision":
				if err := os.WriteFile(outputSBI, []byte("SBI\x00existing"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mutation":
				opts.Progress = func(p Progress) {
					if p.Phase == "converting" {
						_ = os.WriteFile(inputSBI, []byte("SBI\x00changed length"), 0600)
					}
				}
			case "invalid", "invalid shared":
				data = []byte("bad companion")
				if scenario == "invalid shared" {
					output = filepath.Join(dir, "game.cso")
				}
			case "limit":
				data = append([]byte("SBI\x00"), make([]byte, 8188)...)
				opts.MaxOutput = 8192
			}
			if err := os.WriteFile(inputSBI, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Convert(context.Background(), source, output, opts); err == nil {
				t.Fatal("unsafe set accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("partial output published")
			}
			if scenario == "invalid shared" {
				if got, err := os.ReadFile(inputSBI); err != nil || !bytes.Equal(got, data) {
					t.Fatalf("existing shared companion changed: %v", err)
				}
			}
			if scenario == "collision" {
				got, _ := os.ReadFile(outputSBI)
				if string(got) != "SBI\x00existing" {
					t.Fatal("existing companion replaced")
				}
			} else if _, err := os.Stat(outputSBI); !os.IsNotExist(err) {
				t.Fatal("partial companion published")
			}
		})
	}
}

func TestDiscSidecarCollisionRetainsDestinationAndErrorIdentity(t *testing.T) {
	for _, withSourceSBI := range []bool{false, true} {
		t.Run(fmt.Sprint(withSourceSBI), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "game.bin")
			output := filepath.Join(dir, "restored.ecm")
			conflict := filepath.Join(dir, "restored.sbi")
			original := []byte("generated disc bytes")
			existing := []byte("SBI\x00existing")
			if err := os.WriteFile(source, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(conflict, existing, 0600); err != nil {
				t.Fatal(err)
			}
			if withSourceSBI {
				if err := os.WriteFile(filepath.Join(dir, "game.sbi"), []byte("SBI\x00source"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			progressCalled := false
			_, err := Convert(context.Background(), source, output, Options{Format: ECM, Progress: func(Progress) { progressCalled = true }})
			var pathError *os.PathError
			if !errors.Is(err, os.ErrExist) || !errors.As(err, &pathError) || pathError.Path != conflict {
				t.Fatalf("collision lost its destination or identity: %v", err)
			}
			if progressCalled {
				t.Fatal("collision was checked after conversion started")
			}
			for path, want := range map[string][]byte{source: original, conflict: existing} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("collision changed %s: %v", path, err)
				}
			}
			entries, err := os.ReadDir(dir)
			wantCount := 2
			if withSourceSBI {
				wantCount++
			}
			if err != nil || len(entries) != wantCount {
				t.Fatalf("collision left output or staging files: %v, %v", entries, err)
			}
		})
	}
}

func TestDiscSidecarRejectsSymlinkAndCaseAmbiguity(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "disc.iso")
	lower := filepath.Join(dir, "disc.sbi")
	upper := filepath.Join(dir, "disc.SBI")
	if err := os.WriteFile(lower, []byte("SBI\x00first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upper, []byte("SBI\x00second"), 0600); err != nil {
		t.Fatal(err)
	}
	first, _ := os.Stat(lower)
	second, _ := os.Stat(upper)
	if !os.SameFile(first, second) {
		if _, err := findDiscSidecar(source, filepath.Join(dir, "out.cso")); err == nil {
			t.Fatal("ambiguous companions accepted")
		}
		if err := os.Remove(upper); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(lower); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.sbi")
	if err := os.WriteFile(target, []byte("SBI\x00outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lower); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := findDiscSidecar(source, filepath.Join(dir, "out.cso")); err == nil {
		t.Fatal("sidecar symlink accepted")
	}
}

func TestPlaylistPreservesSidecarsForConvertedAndCopiedDiscs(t *testing.T) {
	source, _ := playlistFixture(t)
	dir := filepath.Dir(source)
	for _, sub := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(dir, sub, "disc.sbi"), []byte("SBI\x00"+sub), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Convert(context.Background(), filepath.Join(dir, "first/disc.iso"), filepath.Join(dir, "first/disc.cso"), Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("first/disc.cso\nsecond/disc.iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertPlaylist(context.Background(), source, filepath.Join(dir, "set.m3u"), Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"disc.sbi", "disc-2.sbi"} {
		got, err := os.ReadFile(filepath.Join(dir, "set", name))
		want := []string{"SBI\x00first", "SBI\x00second"}[i]
		if err != nil || string(got) != want {
			t.Fatalf("playlist companion %s: %v", name, err)
		}
	}
}

func TestPublicationRejectsUndeclaredEngineFile(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"game.cso", "private.log"} {
		if err := os.WriteFile(filepath.Join(stage, name), []byte("contents"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := publish(context.Background(), stage, filepath.Join(stage, "game.cso"), filepath.Join(dir, "game.cso"), 1000)
	if err == nil || !strings.Contains(err.Error(), "unexpected output file") {
		t.Fatalf("undeclared engine output accepted: %v", err)
	}
	for _, name := range []string{"game.cso", "private.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("undeclared output published")
		}
	}
}

func TestPublicationRequiresDeclaredCompanion(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(stage, "game.cue")
	if err := os.WriteFile(staged, []byte("FILE \"game.bin\" BINARY\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "game.cue")
	if _, err := publish(context.Background(), stage, staged, output, 1000, filepath.Join(stage, "game.bin")); err == nil {
		t.Fatal("missing track published")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("broken descriptor published")
	}
}
