package romcompressor

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestPlanConversionsUsesInputRulesWithoutWriting(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "game.nds")
	original := testfixture.NDS(false)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{}
	for _, name := range []string{"chdman", "dolphin-tool", "azahar", "nsz", "psxpackager", "zarchive", "7z"} {
		tools[name] = os.Args[0]
	}
	plan, err := PlanConversions(context.Background(), source, Options{Tools: tools, Keys: "unread-keys", TempDir: "unused", Format: "ignored", Entry: "ignored", Media: "ignored", Progress: func(Progress) { t.Fatal("planning invoked progress") }})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != source || plan.Input.Format != Raw || plan.Directory || plan.Playlist {
		t.Fatalf("bad source description: %+v", plan)
	}
	formats := make(map[Format]bool)
	for _, target := range plan.Targets {
		formats[target.Format] = true
		if len(target.Dependencies) != 1 || !target.Available {
			t.Fatalf("invalid dependency: %+v", target)
		}
	}
	for _, format := range []Format{ZIP, GZIP, ECM, SevenZip} {
		if !formats[format] {
			t.Fatalf("missing lossless file route %s", format)
		}
	}
	for _, format := range []Format{Raw, CSO, ZSO, CHD, PBP, RVZ, WIA, GCZ, WBFS, CISO, CCI, WUX, WUA, NSZ, XCZ, ZCCI, ZCIA, ZCXI, Z3DSX} {
		if formats[format] {
			t.Fatalf("offered incompatible %s route", format)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("planner created files: %v, %v", entries, err)
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("planner changed source")
	}
}

func TestPlanConversionsChecksDiscGeometryAndSignatures(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "game.iso")
	data := make([]byte, 34*2048)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanConversions(context.Background(), source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !planHas(plan, CSO, "") || !planHas(plan, ZSO, "") || !planHas(plan, CHD, "dvd") || planHas(plan, CCI, "") || planHas(plan, WBFS, "") || planHas(plan, CISO, "") || planHas(plan, RVZ, "") {
		t.Fatalf("incorrect plain ISO choices: %+v", plan.Targets)
	}
	sector := data[32*2048 : 33*2048]
	copy(sector, "MICROSOFT*XBOX*MEDIA")
	copy(sector[2048-len("MICROSOFT*XBOX*MEDIA"):], "MICROSOFT*XBOX*MEDIA")
	binary.LittleEndian.PutUint32(sector[20:], 33)
	binary.LittleEndian.PutUint32(sector[24:], 1)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanConversions(context.Background(), source, Options{})
	if err != nil || !planHas(plan, CCI, "") {
		t.Fatalf("XISO choice missing: %+v, %v", plan, err)
	}
	for _, target := range plan.Targets {
		if target.Format == CCI {
			output := filepath.Join(root, "game.cci")
			if _, err := Convert(context.Background(), source, output, Options{Format: CCI}); err != nil {
				t.Fatalf("planned native route failed: %v", err)
			}
			if err := Verify(context.Background(), output, Options{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	plan, err = PlanConversions(context.Background(), source, Options{MaxOutput: int64(len(data) - 1)})
	if err != nil || planHas(plan, CCI, "") || planHas(plan, CSO, "") {
		t.Fatalf("limit ignored: %+v, %v", plan, err)
	}
	_, err = Convert(context.Background(), source, filepath.Join(root, "missing-parent", "game.wbfs"), Options{Format: WBFS})
	if err == nil || !strings.Contains(err.Error(), "full single- or dual-layer") {
		t.Fatalf("execution skipped shared geometry preflight: %v", err)
	}
}

func TestPlanConversionsZIPSelectionAndTreeSafety(t *testing.T) {
	for _, names := range [][]string{{"one.bin"}, {"one.bin", "two.bin"}, {"../outside.bin"}, {"empty/"}} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "game.zip")
			file, err := os.Create(source)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			for _, name := range names {
				member, err := writer.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasSuffix(name, "/") {
					if _, err := member.Write([]byte("payload")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanConversions(context.Background(), source, Options{})
			if err != nil {
				t.Fatal(err)
			}
			selected, all := false, false
			for _, target := range plan.Targets {
				if target.Format != Raw {
					continue
				}
				if target.ExtractAll {
					all = true
					if !target.Directory || target.RequiresEntry {
						t.Fatalf("bad tree target: %+v", target)
					}
				} else {
					selected = true
					if target.RequiresEntry != (len(names) > 1) {
						t.Fatalf("bad entry selection: %+v", target)
					}
				}
			}
			if selected != !strings.HasSuffix(names[0], "/") || all != !strings.HasPrefix(names[0], "../") {
				t.Fatalf("unsafe/empty archive handling: selected=%v all=%v %+v", selected, all, plan)
			}
		})
	}
}

func TestPlanConversionsDependenciesAndBoundedRequirements(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name    string
		data    []byte
		want    Format
		keys    bool
		listing bool
	}{
		{"game.nsp", []byte("PFS0"), NSZ, true, false},
		{"game.3dsx", []byte("3DSX"), Z3DSX, false, false},
		{"game.7z", []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, Raw, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(root, tc.name)
			if err := os.WriteFile(source, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanConversions(context.Background(), source, Options{Tools: map[string]string{"nsz": filepath.Join(root, "missing"), "azahar": filepath.Join(root, "missing"), "7z": filepath.Join(root, "missing")}})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, target := range plan.Targets {
				if target.Format == tc.want {
					found = true
					if target.Available || target.RequiresKeys != tc.keys || target.NeedsArchiveListing != tc.listing {
						t.Fatalf("requirements not reported: %+v", target)
					}
				}
			}
			if !found {
				t.Fatalf("missing eligible target: %+v", plan)
			}
		})
	}
}

func TestPlanConversionsFoldersAndMixedPlaylists(t *testing.T) {
	root := t.TempDir()
	title := filepath.Join(root, "title")
	makeWUATitleFixture(t, title, "000500001234abcd", "0010")
	plan, err := PlanConversions(context.Background(), title, Options{})
	if err != nil || !plan.Directory || !planHas(plan, ZIP, "") || !planHas(plan, WUA, "") {
		t.Fatalf("folder routes: %+v, %v", plan, err)
	}
	for name, data := range map[string][]byte{"dvd.chd": testfixture.CHDMetadata(5, "DVD "), "disc.iso": make([]byte, 2048), "game.m3u": []byte("dvd.chd\ndisc.iso\n")} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err = PlanConversions(context.Background(), filepath.Join(root, "game.m3u"), Options{Tools: map[string]string{"chdman": filepath.Join(root, "missing")}})
	if err != nil || !plan.Playlist {
		t.Fatalf("playlist: %+v, %v", plan, err)
	}
	found := false
	for _, target := range plan.Targets {
		if target.Format == Raw {
			found = true
			if target.Media != "" || target.Extension != ".m3u" || target.Available || !reflect.DeepEqual(target.Dependencies, []OperationCapability{{Engine: "chdman"}, {Engine: "native", Available: true}}) {
				t.Fatalf("mixed restoration dependencies: %+v", target)
			}
		}
		if target.Format == CHD && target.Media == "cd" {
			t.Fatalf("offered conflicting CHD media: %+v", target)
		}
	}
	if !found {
		t.Fatal("mixed playlist restoration missing")
	}
}

func TestPlanConversionsRejectsInvalidQueries(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "game.bin")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PlanConversions(ctx, source, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	for _, tc := range []struct {
		source string
		opts   Options
	}{{"", Options{}}, {source, Options{MaxOutput: -1}}, {source, Options{Tools: map[string]string{"unknown": "unused"}}}, {filepath.Join(root, "missing"), Options{}}} {
		if _, err := PlanConversions(context.Background(), tc.source, tc.opts); err == nil {
			t.Fatalf("accepted invalid query: %+v", tc)
		}
	}
}

func TestPlanConversionsRestorationHonorsKnownExpansionAndVersion(t *testing.T) {
	root := t.TempDir()
	source, compressed := filepath.Join(root, "game.iso"), filepath.Join(root, "game.cso")
	if err := os.WriteFile(source, make([]byte, 64<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(context.Background(), source, compressed, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanConversions(context.Background(), compressed, Options{MaxOutput: 1 << 10})
	if err != nil || planHas(plan, Raw, "") {
		t.Fatalf("offered known over-limit restoration: %+v %v", plan, err)
	}
	_, err = Convert(context.Background(), compressed, filepath.Join(root, "missing", "restored.iso"), Options{Format: Raw, MaxOutput: 1 << 10})
	if err == nil || !strings.Contains(err.Error(), "expanded size limit") {
		t.Fatalf("execution did not share limit preflight: %v", err)
	}
	file, err := os.OpenFile(compressed, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{2}, 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanConversions(context.Background(), compressed, Options{})
	if err != nil || planHas(plan, Raw, "") {
		t.Fatalf("offered unsupported CSO version: %+v %v", plan, err)
	}
}

func TestPlanConversionsPlaylistBoundsKnownAggregateBytes(t *testing.T) {
	root := t.TempDir()
	first, second, source := filepath.Join(root, "first.iso"), filepath.Join(root, "second.iso"), filepath.Join(root, "game.m3u")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, make([]byte, 2048), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(source, []byte("first.iso\nsecond.iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanConversions(context.Background(), source, Options{MaxOutput: 3 << 10})
	if err != nil || planHas(plan, Raw, "") {
		t.Fatalf("offered raw copies over aggregate limit: %+v %v", plan, err)
	}
	compressed := filepath.Join(root, "second.cso")
	if _, err := Convert(context.Background(), second, compressed, Options{Format: CSO}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("first.iso\nsecond.cso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanConversions(context.Background(), source, Options{MaxOutput: 3 << 10})
	if err != nil || planHas(plan, Raw, "") {
		t.Fatalf("offered known native expansion over aggregate limit: %+v %v", plan, err)
	}
}

func TestPlanConversionsRejectsKnownUnsupportedZIPMembers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   uint16
		expanded uint64
		want     error
	}{{"algorithm", 99, 1, zip.ErrAlgorithm}, {"store size", zip.Store, 2, zip.ErrFormat}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "game.zip")
			file, err := os.Create(source)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			member, err := writer.CreateRaw(&zip.FileHeader{Name: "game.bin", Method: tc.method, CompressedSize64: 1, UncompressedSize64: tc.expanded})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := member.Write([]byte{0}); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanConversions(context.Background(), source, Options{})
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range plan.Targets {
				if target.Format == Raw {
					t.Fatalf("offered impossible ZIP restoration: %+v", target)
				}
			}
			if err := Verify(context.Background(), source, Options{}); !errors.Is(err, tc.want) {
				t.Fatalf("shared member validation lost error identity: %v", err)
			}
		})
	}
}

func TestPlanConversionsPBPAndWUALimits(t *testing.T) {
	root := t.TempDir()
	packed, raw := testfixture.PBP(t)
	pbp := filepath.Join(root, "disc.pbp")
	if err := os.WriteFile(pbp, packed, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanConversions(context.Background(), pbp, Options{MaxOutput: int64(len(raw) - 1)})
	if err != nil || planHas(plan, Raw, "") {
		t.Fatalf("offered over-limit PBP restoration: %+v %v", plan, err)
	}
	wua := filepath.Join(root, "title.wua")
	archive := rawWUAFixture(t, "asset.bin")
	if err := os.WriteFile(wua, archive, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(context.Background(), wua)
	if err != nil {
		t.Fatal(err)
	}
	expanded := int64(0)
	for _, entry := range info.Entries {
		expanded += int64(entry.Size)
	}
	plan, err = PlanConversions(context.Background(), wua, Options{MaxOutput: int64(len(archive)) + expanded - 1})
	if err != nil || planHas(plan, Raw, "") {
		t.Fatalf("offered known WUA scratch overflow: %+v %v", plan, err)
	}
	plan, err = PlanConversions(context.Background(), wua, Options{})
	if err != nil || !planHas(plan, Raw, "") {
		t.Fatalf("missing valid WUA restoration: %+v %v", plan, err)
	}
}

func TestPlanConversionsRegularFileSymlinkMatchesConvert(t *testing.T) {
	root := t.TempDir()
	source, link := filepath.Join(root, "original.iso"), filepath.Join(root, "alias.iso")
	if err := os.WriteFile(source, make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	plan, err := PlanConversions(context.Background(), link, Options{})
	if err != nil || !planHas(plan, CSO, "") {
		t.Fatalf("file symlink rejected by planner: %+v %v", plan, err)
	}
	if _, err := Convert(context.Background(), link, filepath.Join(root, "game.cso"), Options{Format: CSO}); err != nil {
		t.Fatalf("planned file symlink route failed: %v", err)
	}
}

func TestPlanConversionsMixedCHDCopiesKeepAutomaticMedia(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string][]byte{"cd.chd": testfixture.CHDMetadata(5, "CHT2"), "dvd.chd": testfixture.CHDMetadata(5, "DVD "), "game.m3u": []byte("cd.chd\ndvd.chd\n")} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PlanConversions(context.Background(), filepath.Join(root, "game.m3u"), Options{})
	if err != nil || !planHas(plan, CHD, "") {
		t.Fatalf("mixed already-CHD set lost verified-copy choice: %+v %v", plan, err)
	}
	for _, target := range plan.Targets {
		if target.Format == CHD && target.Media != "" {
			t.Fatalf("mixed CD/DVD set offered conflicting forced media: %+v", target)
		}
	}
}

func planHas(plan ConversionPlan, format Format, media string) bool {
	for _, target := range plan.Targets {
		if target.Format == format && target.Media == media {
			return true
		}
	}
	return false
}
