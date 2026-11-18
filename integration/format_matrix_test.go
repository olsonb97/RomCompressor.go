package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	rc "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// A full run requires actual, verified containers for every installed engine.
// See docs/development.md. No sample is modified or implicitly downloaded.
func TestFormatPairMatrix(t *testing.T) {
	dir := os.Getenv("ROMCOMPRESSOR_MATRIX")
	if dir == "" && os.Getenv("ROMCOMPRESSOR_TEST_FORMAT_MATRIX") != "1" {
		t.Skip("opt in to the full format matrix; see docs/development.md")
	}
	ctx := t.Context()
	opts := matrixOptions()
	formats := []rc.Format{rc.Raw}
	for _, item := range rc.Formats() {
		formats = append(formats, item.Format)
	}
	for _, input := range formats {
		t.Run(string(input), func(t *testing.T) {
			ext := matrixExtension(input)
			source := filepath.Join(dir, "sample"+ext)
			if dir == "" {
				source = buildMatrixSample(t, t.TempDir(), input, opts)
			}
			info, err := rc.InspectWithOptions(ctx, source, opts)
			if err != nil || info.Format != input {
				t.Fatalf("inspect %s: %+v, %v", input, info, err)
			}
			original, err := fileops.Digest(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			verify := opts
			verify.TempDir = t.TempDir()
			if input == rc.NSZ || input == rc.XCZ {
				verify.Keys = os.Getenv("ROMCOMPRESSOR_NSZ_KEYS")
			}
			if err := rc.Verify(ctx, source, verify); (err == nil) != (input != rc.Raw) {
				t.Fatalf("verify %s: %v", input, err)
			}
			plan, err := rc.PlanConversions(ctx, source, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, output := range formats {
				media := []string{""}
				if output == rc.CHD || input == rc.CHD && output == rc.Raw {
					media = []string{"cd", "dvd", "hdd"}
				}
				for _, medium := range media {
					name := string(output)
					if medium != "" {
						name += "-" + medium
					}
					t.Run(name, func(t *testing.T) {
						want := matrixRoute(input, output, info.Media, medium)
						advertised := false
						for _, target := range plan.Targets {
							if target.Format == output && target.Media == medium && !target.ExtractAll {
								advertised = true
							}
						}
						if advertised != want {
							t.Errorf("plan offered=%t, expected %t", advertised, want)
						}
						choice := opts
						choice.Format, choice.Media = output, medium
						if output == rc.Raw {
							choice.Keys = verify.Keys
						}
						work := t.TempDir()
						ext := matrixExtension(output)
						if output == rc.Raw {
							ext = matrixRawExtension(input, medium)
						}
						dest := filepath.Join(work, "output"+ext)
						got, err := rc.Convert(ctx, source, dest, choice)
						if !want {
							if err == nil || got.Verified {
								t.Fatalf("unexpected successful conversion: %+v", got)
							}
							if files, err := os.ReadDir(work); err != nil || len(files) != 0 {
								t.Fatalf("refused conversion left artifacts: %v, %v", files, err)
							}
							return
						}
						if err != nil || !got.Verified || got.Format != output {
							t.Fatalf("convert: %+v, %v", got, err)
						}
						if output == rc.ZIP || output == rc.GZIP || output == rc.SevenZip {
							restored := filepath.Join(work, "original"+matrixExtension(input))
							if _, err := rc.Convert(ctx, dest, restored, rc.Options{Format: rc.Raw, Tools: opts.Tools}); err != nil {
								t.Fatal(err)
							}
							if err := fileops.CompareFiles(ctx, source, restored); err != nil {
								t.Fatal(err)
							}
						}
					})
				}
			}
			if after, err := fileops.Digest(ctx, source); err != nil || after != original {
				t.Fatalf("input changed: %v", err)
			}
		})
	}
}

func matrixOptions() rc.Options {
	opts := rc.Options{Tools: map[string]string{}}
	for name, variable := range map[string]string{
		"chdman": "CHDMAN", "azahar": "AZAHAR", "nsz": "NSZ", "7z": "7Z",
		"zarchive": "ZARCHIVE", "psxpackager": "PSXPACKAGER", "dolphin-tool": "DOLPHIN",
	} {
		if path := os.Getenv("ROMCOMPRESSOR_" + variable); path != "" {
			opts.Tools[name] = path
		}
	}
	return opts
}

// Expectations are independent of the planner. The raw sample is a small
// GameCube ISO and the WBFS sample is a single file.
func matrixRoute(input, output rc.Format, sourceMedia, media string) bool {
	if input == output {
		return false
	}
	switch output {
	case rc.Raw:
		return input != rc.CHD || media == sourceMedia
	case rc.ZIP, rc.GZIP, rc.SevenZip:
		return true
	case rc.CSO, rc.ZSO, rc.ECM:
		return input == rc.Raw
	case rc.CHD:
		return input == rc.Raw && (media == "cd" || media == "dvd")
	case rc.RVZ, rc.WIA, rc.GCZ:
		switch input {
		case rc.Raw, rc.RVZ, rc.WIA, rc.GCZ, rc.WBFS, rc.CISO:
			return true
		}
	}
	return false
}

func matrixExtension(f rc.Format) string {
	switch f {
	case rc.Raw:
		return ".iso"
	case rc.GZIP:
		return ".gz"
	}
	return "." + string(f)
}

func matrixRawExtension(input rc.Format, media string) string {
	switch input {
	case rc.CSO, rc.ZSO, rc.RVZ, rc.WIA, rc.GCZ, rc.WBFS, rc.CISO, rc.CCI:
		return ".iso"
	case rc.CHD:
		return map[string]string{"cd": ".cue", "dvd": ".iso", "hdd": ".img"}[media]
	case rc.PBP:
		return ".cue"
	case rc.NSZ:
		return ".nsp"
	case rc.XCZ:
		return ".xci"
	case rc.WUX:
		return ".wud"
	case rc.ZCCI:
		return ".cci"
	case rc.ZCIA:
		return ".cia"
	case rc.ZCXI:
		return ".cxi"
	case rc.Z3DSX:
		return ".3dsx"
	case rc.WUA:
		return ""
	default:
		return ".bin"
	}
}
