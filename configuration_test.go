package romcompressor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestArchiveWrappingRejectsUnusedMediaAndKeys(t *testing.T) {
	xcz := make([]byte, 512)
	copy(xcz[0x100:], "HEAD")
	for _, wrapper := range []struct {
		format Format
		ext    string
	}{{ZIP, ".zip"}, {GZIP, ".gz"}, {SevenZip, ".7z"}} {
		for _, input := range []struct {
			name string
			data []byte
			opts Options
			want string
		}{
			{"image.chd", testfixture.CHDMetadata(5, "CHT2"), Options{Media: "hdd"}, "media is only used for CHD conversion"},
			{"image.nsz", []byte("PFS0"), Options{Keys: "missing.keys"}, "keys are only used for Switch compression"},
			{"image.xcz", xcz, Options{Keys: "missing.keys"}, "keys are only used for Switch compression"},
		} {
			t.Run(input.name+"/"+string(wrapper.format), func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, input.name)
				output := filepath.Join(dir, "wrapped"+wrapper.ext)
				if err := os.WriteFile(source, input.data, 0600); err != nil {
					t.Fatal(err)
				}
				opts := input.opts
				opts.Format = wrapper.format
				opts.Tools = map[string]string{"7z": filepath.Join(dir, "missing-engine")}
				var progressed bool
				opts.Progress = func(Progress) { progressed = true }
				_, err := Convert(context.Background(), source, output, opts)
				if err == nil || !strings.Contains(err.Error(), input.want) {
					t.Fatalf("unused option did not fail before wrapping/dependency lookup: %v", err)
				}
				if progressed {
					t.Fatal("unused option started expensive work")
				}
				if actual, err := os.ReadFile(source); err != nil || string(actual) != string(input.data) {
					t.Fatalf("rejected wrapping changed original: %v", err)
				}
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
					t.Fatalf("rejected wrapping left output or staging files: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestCHDAndSwitchConversionsRetainRelevantOptionValidation(t *testing.T) {
	xci := make([]byte, 512)
	copy(xci[0x100:], "HEAD")
	for _, tc := range []struct {
		name   string
		data   []byte
		output string
		opts   Options
		engine string
	}{
		{"image.chd", testfixture.CHDMetadata(5, "CHT2"), "restored.cue", Options{Format: Raw, Media: "cd"}, "chdman"},
		{"image.iso", make([]byte, 2048), "packed.chd", Options{Format: CHD, Media: "dvd"}, "chdman"},
		{"image.nsz", []byte("PFS0"), "restored.nsp", Options{Format: Raw}, "nsz"},
		{"image.xcz", xci, "restored.xci", Options{Format: Raw}, "nsz"},
		{"image.nsp", []byte("PFS0"), "packed.nsz", Options{Format: NSZ}, "nsz"},
		{"image.xci", xci, "packed.xcz", Options{Format: XCZ}, "nsz"},
	} {
		t.Run(tc.name+"/"+string(tc.opts.Format), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, tc.name)
			if err := os.WriteFile(source, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			opts := tc.opts
			opts.Tools = map[string]string{tc.engine: filepath.Join(dir, "missing-engine")}
			if tc.engine == "nsz" {
				opts.Keys = filepath.Join(dir, "test.keys")
				if err := os.WriteFile(opts.Keys, []byte("generated non-secret fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(dir, tc.output)
			_, err := Convert(context.Background(), source, output, opts)
			var dependency *DependencyError
			if !errors.As(err, &dependency) || dependency.Engine != tc.engine {
				t.Fatalf("relevant options no longer reach normal dependency validation: %v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("failed dependency lookup published output: %v", err)
			}
			if actual, err := os.ReadFile(source); err != nil || string(actual) != string(tc.data) {
				t.Fatalf("dependency rejection changed original: %v", err)
			}
		})
	}
}

func TestPublicEntryPointsRejectUnknownToolsBeforeIO(t *testing.T) {
	opts := Options{Format: CSO, Tools: map[string]string{"chdmann": "missing"}}
	for name, run := range map[string]func() error{
		"convert": func() error { _, err := Convert(context.Background(), "missing.iso", "out.cso", opts); return err },
		"playlist": func() error {
			_, err := ConvertPlaylist(context.Background(), "missing.m3u", "out.m3u", opts)
			return err
		},
		"verify":  func() error { return Verify(context.Background(), "missing.cso", opts) },
		"inspect": func() error { _, err := InspectWithOptions(context.Background(), "missing.7z", opts); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil || !strings.Contains(err.Error(), `unknown tool "chdmann"`) {
				t.Fatalf("expected configuration error before opening source, got %v", err)
			}
		})
	}
}

func TestDependencyErrorsAreActionableAcrossPublicOperations(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "test.7z")
	if err := os.WriteFile(archive, []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, 0600); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "disc.iso")
	if err := os.WriteFile(image, make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(dir, "game.m3u")
	if err := os.WriteFile(playlist, []byte("disc.iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing-engine.exe")
	archiveOptions := Options{Tools: map[string]string{"7z": missing}}
	chdOptions := Options{Format: CHD, Media: "dvd", Tools: map[string]string{"chdman": missing}}
	for name, test := range map[string]struct {
		engine string
		run    func() error
	}{
		"inspect": {"7z", func() error { _, err := InspectWithOptions(context.Background(), archive, archiveOptions); return err }},
		"verify":  {"7z", func() error { return Verify(context.Background(), archive, archiveOptions) }},
		"convert": {"chdman", func() error {
			_, err := Convert(context.Background(), image, filepath.Join(dir, "disc.chd"), chdOptions)
			return err
		}},
		"playlist": {"chdman", func() error {
			_, err := ConvertPlaylist(context.Background(), playlist, filepath.Join(dir, "converted.m3u"), chdOptions)
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			err := test.run()
			var dependency *DependencyError
			if !errors.As(err, &dependency) || dependency.Engine != test.engine || !dependency.Configured {
				t.Fatalf("dependency type/context lost: %#v, %v", dependency, err)
			}
			if !strings.Contains(err.Error(), "Options.Tools") || !strings.Contains(err.Error(), "--tool") {
				t.Fatalf("dependency error does not guide library and CLI users: %v", err)
			}
			if strings.Contains(err.Error(), missing) {
				t.Fatal("lookup error exposed its configured executable path")
			}
		})
	}
}

func TestOutputPreflightPreservesExistenceError(t *testing.T) {
	for _, playlist := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "playlist"}[playlist], func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "existing.zip")
			if playlist {
				output = filepath.Join(dir, "existing.m3u")
			}
			if err := os.WriteFile(output, []byte("original output"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if playlist {
				_, err = ConvertPlaylist(context.Background(), filepath.Join(dir, "source.m3u"), output, Options{Format: CSO})
			} else {
				_, err = Convert(context.Background(), filepath.Join(dir, "source.gba"), output, Options{Format: ZIP})
			}
			if !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), output) {
				t.Fatalf("collision is not actionable: %v", err)
			}
			var collision *os.PathError
			if !errors.As(err, &collision) || collision.Path != output {
				t.Fatalf("collision destination is unavailable to callers: %v", err)
			}
			if data, err := os.ReadFile(output); err != nil || string(data) != "original output" {
				t.Fatalf("existing output changed: %v", err)
			}
		})
	}
}

func TestSharedJavaConfigurationKeepsNativeWUX(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "small.wud")
	if err := os.WriteFile(source, make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Convert(context.Background(), source, filepath.Join(dir, "small.wux"), Options{
		Format: WUX, Tools: map[string]string{"java": filepath.Join(dir, "missing-java")},
	})
	if err == nil || !strings.Contains(err.Error(), "WUX compression requires a complete") || strings.Contains(err.Error(), "JWUDTool") {
		t.Fatalf("shared Java setting selected the legacy adapter: %v", err)
	}
}
