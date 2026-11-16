package romcompressor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func playlistFixture(t *testing.T) (string, [][]byte) {
	t.Helper()
	dir := t.TempDir()
	payloads := [][]byte{bytes.Repeat([]byte{0x31}, 8192), bytes.Repeat([]byte{0x61}, 4096)}
	for i, sub := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "disc.iso"), payloads[i], 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(dir, "source.m3u")
	if err := os.WriteFile(source, []byte("#EXTM3U\nfirst/disc.iso\nsecond/disc.iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return source, payloads
}

func TestPlaylistConvertsOrderAndDuplicateNames(t *testing.T) {
	source, payloads := playlistFixture(t)
	output := filepath.Join(filepath.Dir(source), "converted.m3u")
	result, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified {
		t.Fatal("result unverified")
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "./converted/disc.cso\n./converted/disc-2.cso\n" {
		t.Fatalf("unexpected playlist %q", contents)
	}
	for i, line := range strings.Fields(string(contents)) {
		decoded := filepath.Join(filepath.Dir(source), line+".iso")
		if _, err := Convert(context.Background(), filepath.Join(filepath.Dir(source), filepath.FromSlash(line)), decoded, Options{Format: Raw}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Fatal("disc order or payload changed")
		}
	}
	if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO}); err == nil {
		t.Fatal("existing output accepted")
	}
}

func TestPlaylistOutputNameCannotBecomeComment(t *testing.T) {
	source, _ := playlistFixture(t)
	output := filepath.Join(filepath.Dir(source), "# Game.m3u")
	if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	discs, _, err := readPlaylist(context.Background(), output)
	if err != nil || len(discs) != 2 {
		t.Fatalf("generated playlist cannot be read: %v (%d discs)", err, len(discs))
	}
	if _, err := ConvertPlaylist(context.Background(), source, filepath.Join(filepath.Dir(source), "bad\nname.m3u"), Options{Format: CSO}); err == nil {
		t.Fatal("accepted unrepresentable playlist name")
	}
}

func TestPlaylistUsesActualCompanionName(t *testing.T) {
	for _, suffix := range []string{".", " "} {
		t.Run(suffix, func(t *testing.T) {
			source, _ := playlistFixture(t)
			parent := filepath.Dir(source)
			stem := "converted" + suffix
			output := filepath.Join(parent, stem+".m3u")
			if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO}); err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS == "windows" {
				stem = "converted"
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				found = found || entry.IsDir() && entry.Name() == stem
			}
			if !found {
				t.Fatalf("no companion directory named %q", stem)
			}
			contents, err := os.ReadFile(output)
			if want := "./" + stem + "/disc.cso\n./" + stem + "/disc-2.cso\n"; err != nil || string(contents) != want {
				t.Fatalf("playlist = %q, %v, want %q", contents, err, want)
			}
		})
	}
}

func TestPlaylistResolvesLinkedDirectories(t *testing.T) {
	source, payloads := playlistFixture(t)
	sourceDir := filepath.Dir(source)
	newRoot := t.TempDir()
	alias := filepath.Join(newRoot, "source")
	testfixture.DirectoryLink(t, alias, sourceDir)
	testfixture.DirectoryLink(t, filepath.Join(sourceDir, "mounted"), filepath.Join(sourceDir, "second"))
	if err := os.WriteFile(source, []byte("first/disc.iso\nmounted/disc.iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	output := filepath.Join(newRoot, "converted.m3u")
	if _, err := ConvertPlaylist(ctx, filepath.Join(alias, filepath.Base(source)), output, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	disc := filepath.Join(newRoot, "converted", "disc-2.cso")
	restored := filepath.Join(newRoot, "restored.iso")
	if _, err := Convert(ctx, disc, restored, Options{Format: Raw}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(restored); err != nil || !bytes.Equal(got, payloads[1]) {
		t.Fatalf("linked disc payload changed: %v", err)
	}
	testfixture.DirectoryLink(t, filepath.Join(sourceDir, "outside"), filepath.Dir(disc))
	if err := os.WriteFile(source, []byte("outside/disc-2.cso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output = filepath.Join(newRoot, "escaped.m3u")
	if _, err := ConvertPlaylist(ctx, filepath.Join(alias, filepath.Base(source)), output, Options{Format: CSO}); err == nil || !strings.Contains(err.Error(), "outside its folder") {
		t.Fatalf("outside link error = %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("published playlist from outside link: %v", err)
	}
}

func TestPlaylistFailurePublishesNothing(t *testing.T) {
	source, _ := playlistFixture(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), "source.m3u"), []byte("first/disc.iso\nsecond/disc.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(filepath.Dir(source), "second/disc.iso"), filepath.Join(filepath.Dir(source), "second/disc.txt")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(source), "converted.m3u")
	if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO}); err == nil {
		t.Fatal("unsupported second disc accepted")
	}
	for _, path := range []string{output, strings.TrimSuffix(output, ".m3u")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("partial set published")
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(source))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".romcompressor-") {
			t.Fatal("staging leaked")
		}
	}
}

func TestPlaylistCancellation(t *testing.T) {
	source, _ := playlistFixture(t)
	output := filepath.Join(filepath.Dir(source), "converted.m3u")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := ConvertPlaylist(ctx, source, output, Options{Format: ZSO, Progress: func(p Progress) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("cancelled playlist published")
	}
}

func TestPlaylistRechecksEarlierDiscAndSidecar(t *testing.T) {
	for _, name := range []string{"disc.iso", "disc.sbi"} {
		t.Run(name, func(t *testing.T) {
			source, _ := playlistFixture(t)
			dir := filepath.Dir(source)
			output := filepath.Join(dir, "converted.m3u")
			sidecar := filepath.Join(dir, "first", "disc.sbi")
			if err := os.WriteFile(sidecar, []byte{'S', 'B', 'I', 0, 1}, 0600); err != nil {
				t.Fatal(err)
			}
			var changed bool
			opts := Options{Format: CSO, Progress: func(p Progress) {
				if !changed && strings.HasPrefix(p.Phase, "disc 2/2 ") {
					changed = true
					if err := os.WriteFile(filepath.Join(dir, "first", name), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}}
			_, err := ConvertPlaylist(context.Background(), source, output, opts)
			if !changed || err == nil || !strings.Contains(err.Error(), "source changed") {
				t.Fatalf("ConvertPlaylist returned %v, fixture changed = %v", err, changed)
			}
			for _, path := range []string{output, strings.TrimSuffix(output, ".m3u")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("partial disc set published: %v", err)
				}
			}
		})
	}
}

func TestPlaylistRejectsReplacementBeforeUpcomingDisc(t *testing.T) {
	source, payloads := playlistFixture(t)
	dir := filepath.Dir(source)
	upcoming := filepath.Join(dir, "second", "disc.iso")
	saved := filepath.Join(dir, "saved.iso")
	output := filepath.Join(dir, "converted.m3u")
	replaced := false
	_, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO, Progress: func(p Progress) {
		if replaced || !strings.HasPrefix(p.Phase, "disc 1/2 ") {
			return
		}
		if err := os.Rename(upcoming, saved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(upcoming, bytes.Repeat([]byte{0x99}, len(payloads[1])), 0600); err != nil {
			t.Fatal(err)
		}
		replaced = true
	}})
	if !replaced || err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("ConvertPlaylist = %v, upcoming disc replaced = %v", err, replaced)
	}
	for _, path := range []string{output, strings.TrimSuffix(output, ".m3u")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected publication of %s: %v", path, err)
		}
	}
	original, err := os.ReadFile(saved)
	if err != nil || !bytes.Equal(original, payloads[1]) {
		t.Fatalf("original disc was changed: %v", err)
	}
}

func TestPlaylistRejectsRetargetedReference(t *testing.T) {
	source, _ := playlistFixture(t)
	dir := filepath.Dir(source)
	alias := filepath.Join(dir, "alias.iso")
	if err := os.Symlink("second/disc.iso", alias); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := os.WriteFile(source, []byte("first/disc.iso\nalias.iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "converted.m3u")
	retargeted := false
	_, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO, Progress: func(p Progress) {
		if retargeted || !strings.HasPrefix(p.Phase, "disc 1/2 ") {
			return
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("first/disc.iso", alias); err != nil {
			t.Fatal(err)
		}
		retargeted = true
	}})
	if !retargeted || err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("ConvertPlaylist = %v, reference retargeted = %v", err, retargeted)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("unexpected playlist publication: %v", err)
	}
}

func TestPlaylistRejectsUnrepresentableFolderName(t *testing.T) {
	source, _ := playlistFixture(t)
	dir := filepath.Dir(source)
	for _, name := range []string{"set:one", `set\one`, "set\xffone"} {
		if validPlaylistComponent(name) {
			t.Errorf("accepted invalid playlist component %q", name)
		}
	}
	output := filepath.Join(dir, "set:one.m3u")
	if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO}); err == nil {
		t.Fatalf("published playlist with unrepresentable folder name %q", output)
	}
}

func TestPlaylistStageCannotOverwriteSourceHardLink(t *testing.T) {
	source, _ := playlistFixture(t)
	dir := filepath.Dir(source)
	output := filepath.Join(dir, "converted.m3u")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "hard-link-test.m3u")
	if err := os.Link(source, probe); err != nil {
		t.Skipf("file system cannot create hard links: %v", err)
	}
	var linked bool
	_, err = ConvertPlaylist(context.Background(), source, output, Options{Format: CSO, Progress: func(p Progress) {
		if linked || !strings.HasPrefix(p.Phase, "disc 2/2 ") {
			return
		}
		stages, err := filepath.Glob(filepath.Join(dir, ".romcompressor-playlist-*"))
		if err != nil || len(stages) != 1 {
			t.Fatalf("find playlist stage: %v, %v", stages, err)
		}
		if err := os.Link(source, filepath.Join(stages[0], filepath.Base(output))); err != nil {
			t.Fatal(err)
		}
		linked = true
	}})
	if !linked || !os.IsExist(err) {
		t.Fatalf("ConvertPlaylist = %v, hard link made = %v", err, linked)
	}
	if after, err := os.ReadFile(source); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("source playlist changed: %q, %v", after, err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("unexpected output: %v", err)
	}
}

func TestPlaylistRejectsChangedStagedDiscSet(t *testing.T) {
	for _, change := range []string{"extra file", "replaced disc", "rewritten disc"} {
		t.Run(change, func(t *testing.T) {
			source, _ := playlistFixture(t)
			dir := filepath.Dir(source)
			output := filepath.Join(dir, "converted.m3u")
			var changed bool
			_, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO, Progress: func(p Progress) {
				if changed || !strings.HasPrefix(p.Phase, "disc 2/2 ") {
					return
				}
				matches, err := filepath.Glob(filepath.Join(dir, ".romcompressor-playlist-*"))
				if err != nil || len(matches) != 1 {
					t.Fatalf("find playlist stage: %v, %v", matches, err)
				}
				stage := matches[0]
				switch change {
				case "extra file":
					if err := os.WriteFile(filepath.Join(stage, "surprise.cso"), []byte("unverified"), 0600); err != nil {
						t.Fatal(err)
					}
				case "replaced disc", "rewritten disc":
					disc := filepath.Join(stage, "disc.cso")
					info, err := os.Stat(disc)
					if err != nil {
						t.Fatal(err)
					}
					if change == "replaced disc" {
						if err := os.Rename(disc, filepath.Join(dir, "moved.cso")); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(disc, bytes.Repeat([]byte{0x7f}, int(info.Size())), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(disc, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
				changed = true
			}})
			if !changed || err == nil || !strings.Contains(err.Error(), "disc set") {
				t.Fatalf("ConvertPlaylist = %v, staging changed = %v", err, changed)
			}
			for _, path := range []string{output, strings.TrimSuffix(output, ".m3u")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("unexpected published file %s: %v", path, err)
				}
			}
		})
	}
}

func TestPlaylistReusesAlreadyCompressedDisc(t *testing.T) {
	source, _ := playlistFixture(t)
	dir := filepath.Dir(source)
	compressed := filepath.Join(dir, "first/disc.cso")
	if _, err := Convert(context.Background(), filepath.Join(dir, "first/disc.iso"), compressed, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("first/disc.cso\nsecond/disc.iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "converted.m3u")
	if _, err := ConvertPlaylist(context.Background(), source, output, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(compressed)
	got, _ := os.ReadFile(filepath.Join(dir, "converted/disc.cso"))
	if !bytes.Equal(got, want) {
		t.Fatal("existing compressed payload changed")
	}
}

func TestPlaylistRejectsExternalAndNestedPaths(t *testing.T) {
	source, _ := playlistFixture(t)
	for _, entry := range []string{"../outside.iso", "C:/game.iso", "https://example.com/game.iso", "/tmp/game.iso", "source.m3u"} {
		if err := os.WriteFile(source, []byte(entry), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readPlaylist(context.Background(), source); err == nil {
			t.Fatalf("accepted %q", entry)
		}
	}
}

func TestPlaylistPublicationCollisionRollsBack(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "disc.cso"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	stagedPlaylist := filepath.Join(stage, "staged.m3u")
	if err := os.WriteFile(stagedPlaylist, []byte("converted/disc.cso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "converted.m3u")
	if err := os.WriteFile(output, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	companion := filepath.Join(dir, "converted")
	stagedDiscs := make(map[string]playlistStagedFile)
	if err := rememberStagedDisc(context.Background(), filepath.Join(stage, "disc.cso"), stagedDiscs, nil); err != nil {
		t.Fatal(err)
	}
	if err := publishPlaylist(context.Background(), stagedPlaylist, companion, output, stagedDiscs, nil); err == nil {
		t.Fatal("collision accepted")
	}
	got, _ := os.ReadFile(output)
	if string(got) != "existing" {
		t.Fatal("existing playlist overwritten")
	}
	if _, err := os.Stat(companion); !os.IsNotExist(err) {
		t.Fatal("partial companion folder retained")
	}
}
