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
	for _, tc := range []struct {
		names         []string
		selected, all bool
	}{
		{[]string{"one.bin"}, true, true},
		{[]string{"one.bin", "two.bin"}, true, true},
		{[]string{"../outside.bin"}, true, false},
		{[]string{"empty/"}, false, true},
		{[]string{"same.bin", "same.bin"}, false, false},
		{[]string{"same.bin", "same.bin", "other.bin"}, true, false},
	} {
		names := tc.names
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
			if selected != tc.selected || all != tc.all {
				t.Fatalf("unsafe/empty archive handling: selected=%v all=%v %+v", selected, all, plan)
			}
		})
	}
}

func TestExternalCompressionPreflightHonorsExpandedLimit(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "game.iso")
	data := make([]byte, 65536)
	binary.BigEndian.PutUint32(data[28:], 0xc2339f3d)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opts := Options{MaxOutput: 4096, Tools: map[string]string{"7z": "unused", "dolphin-tool": "unused"}}
	plan, err := PlanConversions(ctx, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []Format{SevenZip, RVZ, WIA, GCZ} {
		if planHas(plan, format, "") {
			t.Errorf("offered %s despite the expanded size limit", format)
		}
		opts.Format = format
		if _, err := Convert(ctx, source, filepath.Join(root, "game."+string(format)), opts); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Errorf("%s did not reject input before engine lookup: %v", format, err)
		}
	}
	for _, tc := range []struct {
		ext, magic string
		offset     int
		format     Format
	}{
		{".nsp", "PFS0", 0, NSZ}, {".xci", "HEAD", 0x100, XCZ},
	} {
		path := filepath.Join(root, "game"+tc.ext)
		clear(data)
		copy(data[tc.offset:], tc.magic)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanConversions(ctx, path, opts)
		if err != nil || planHas(plan, tc.format, "") {
			t.Fatalf("offered oversized %s source: %+v, %v", tc.ext, plan.Targets, err)
		}
	}
}

func TestSwitchGamecardHeaderPreservation(t *testing.T) {
	for _, ext := range []string{".xci", ".xcz"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "input"+ext)
			target, dest := XCZ, filepath.Join(dir, "out.xcz")
			if ext == ".xcz" {
				target, dest = Raw, filepath.Join(dir, "out.xci")
			}
			header := make([]byte, 0xf004)
			copy(header[0x100:], "HEAD")
			binary.LittleEndian.PutUint64(header[0x130:], 0xf000)
			copy(header[0xf000:], "HFS0")
			for _, at := range []int{0x1ff, 0x200, 0x7000, 0xefff} {
				header[at] = 1
				if err := os.WriteFile(src, header, 0600); err != nil {
					t.Fatal(err)
				}
				plan, err := PlanConversions(context.Background(), src, Options{})
				if err != nil || planHas(plan, target, "") != (at < 0x200) || !planHas(plan, ZIP, "") {
					t.Fatalf("offset %#x: plan %+v, %v", at, plan, err)
				}
				if at >= 0x200 {
					_, err := Convert(context.Background(), src, dest, Options{Format: target})
					if err == nil || !strings.Contains(err.Error(), "cannot preserve") {
						t.Fatalf("offset %#x: expected refusal before engine or keys, got %v", at, err)
					}
					if _, err := os.Lstat(dest); !os.IsNotExist(err) {
						t.Fatalf("refused conversion left an output: %v", err)
					}
				}
				header[at] = 0
			}
		})
	}
}

func TestPlanBareCDSectorGeometry(t *testing.T) {
	for _, tc := range []struct {
		size, limit int
		valid       bool
	}{
		{0, 1 << 20, false}, {2049, 1 << 20, false},
		{2048, 9791, false}, {2048, 9792, true},
		{2336, 9792, true}, {2352, 9792, true},
	} {
		source := filepath.Join(t.TempDir(), "game.iso")
		if err := os.WriteFile(source, make([]byte, tc.size), 0600); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanConversions(context.Background(), source, Options{MaxOutput: int64(tc.limit)})
		if err != nil || planHas(plan, CHD, "cd") != tc.valid {
			t.Errorf("CD with %d bytes and limit %d: %+v, %v", tc.size, tc.limit, plan.Targets, err)
		}
	}
}

func TestPlanCDDescriptorLogicalLimit(t *testing.T) {
	for _, tc := range []struct {
		name, descriptor string
		frames           int64
	}{
		{"disc.cue", "FILE a.bin BINARY\nTRACK 01 MODE1/2048\nINDEX 01 00:00:00\nFILE b.bin BINARY\nTRACK 02 MODE1/2048\nINDEX 01 00:00:00\n", 8},
		{"disc.gdi", "2\n1 0 4 2048 a.bin 0\n2 9 4 2048 b.bin 0\n", 16},
		{"disc.nrg", "", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, tc.name)
			data := []byte(tc.descriptor)
			if tc.descriptor == "" {
				data = testfixture.NRG(make([]byte, 5*2048), 0, 0)
			}
			for name, contents := range map[string][]byte{tc.name: data, "a.bin": make([]byte, 2048), "b.bin": make([]byte, 2*2048)} {
				if err := os.WriteFile(filepath.Join(dir, name), contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, delta := range []int64{-1, 0} {
				limit := tc.frames*2448 + delta
				plan, err := PlanConversions(context.Background(), source, Options{MaxOutput: limit})
				if err != nil || planHas(plan, CHD, "cd") != (delta == 0) {
					t.Errorf("limit %d: %+v, %v", limit, plan.Targets, err)
				}
			}
		})
	}
}

func TestPlanCHDTrackRestoration(t *testing.T) {
	for _, tc := range []struct {
		title, fields string
		allowed       bool
	}{
		{"mixed mode", "TYPE:MODE2_FORM_MIX SUBTYPE:NONE FRAMES:4 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0", true},
		{"subcode", "TYPE:AUDIO SUBTYPE:RW_RAW FRAMES:4 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0", false},
		{"separate stored pregap mode", "TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:4 PREGAP:1 PGTYPE:VAUDIO PGSUB:NONE POSTGAP:0", false},
		{"partial track table", "TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0", false},
	} {
		t.Run(tc.title, func(t *testing.T) {
			dir := t.TempDir()
			image := filepath.Join(dir, "game.chd")
			if err := os.WriteFile(image, testfixture.CDTrackHeader("TRACK:1 "+tc.fields), 0600); err != nil {
				t.Fatal(err)
			}
			list := filepath.Join(dir, "game.m3u")
			if err := os.WriteFile(list, []byte("game.chd\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{image, list} {
				plan, err := PlanConversions(context.Background(), source, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if (planHas(plan, Raw, "cd") || planHas(plan, Raw, "")) != tc.allowed {
					t.Fatalf("wrong restoration choice for %s: %+v", source, plan.Targets)
				}
			}
		})
	}
}

func TestMalformedGDROMDoesNotOfferRestoration(t *testing.T) {
	for _, tag := range []string{"CHGD", "CHGT"} {
		t.Run(tag, func(t *testing.T) {
			dir := t.TempDir()
			image, playlist := filepath.Join(dir, "disc.chd"), filepath.Join(dir, "set.m3u")
			if err := os.WriteFile(image, testfixture.CHDMetadata(5, tag), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(playlist, []byte("disc.chd\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, input := range []string{image, playlist} {
				plan, err := PlanConversions(t.Context(), input, Options{})
				if err != nil || planHas(plan, Raw, "cd") || planHas(plan, Raw, "") {
					t.Fatalf("plan promised single-BIN restoration: %+v, %v", plan, err)
				}
			}
			opts := Options{Format: Raw, Media: "cd", Tools: map[string]string{"chdman": filepath.Join(dir, "absent")}}
			if _, err := Convert(t.Context(), image, filepath.Join(dir, "out.cue"), opts); err == nil || !strings.Contains(err.Error(), "GD-ROM") {
				t.Fatalf("expected a layout error before engine lookup: %v", err)
			}
			if files, err := os.ReadDir(dir); err != nil || len(files) != 2 {
				t.Fatalf("failed restoration left files: %v, %v", files, err)
			}
		})
	}
}

func TestPlanGDROMRestoresAsGDI(t *testing.T) {
	for _, tag := range []string{"CHGD", "CHGT"} {
		t.Run(tag, func(t *testing.T) {
			dir := t.TempDir()
			image := filepath.Join(dir, "disc.chd")
			data := testfixture.CDTrackHeader("TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:4 PAD:0 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0")
			copy(data[124:128], tag)
			if err := os.WriteFile(image, data, 0600); err != nil {
				t.Fatal(err)
			}
			opts := Options{Format: Raw, Media: "cd", Tools: map[string]string{"chdman": filepath.Join(dir, "absent")}}
			plan, err := PlanConversions(t.Context(), image, opts)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, target := range plan.Targets {
				if target.Format == Raw && target.Media == "cd" {
					found = target.Extension == ".gdi" && !target.Available
				}
			}
			if !found {
				t.Fatal("missing GDI target when chdman is unavailable")
			}
			if _, err := Convert(t.Context(), image, filepath.Join(dir, "out.cue"), opts); err == nil || !strings.Contains(err.Error(), ".gdi") {
				t.Fatalf("single-BIN output was not refused before engine lookup: %v", err)
			}
			opts.MaxOutput = 1024
			if _, err := Convert(t.Context(), image, filepath.Join(dir, "out.gdi"), opts); err == nil || !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("GD-ROM over its expansion budget: %v", err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
				t.Fatalf("preflight left extra files: %v, %v", entries, err)
			}
		})
	}
}

func TestPlanCHDPlaylistCombinedBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		raw  int64
	}{
		{"cd", testfixture.CDTrackHeader("TRACK:1 TYPE:AUDIO SUBTYPE:NONE FRAMES:4 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0"), 4 * 2352},
		{"dvd", testfixture.CHDMetadata(5, "DVD "), 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"one.chd", "two.chd"} {
				if err := os.WriteFile(filepath.Join(dir, name), tc.data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			list := filepath.Join(dir, "set.m3u")
			if err := os.WriteFile(list, []byte("one.chd\ntwo.chd\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int64{2*tc.raw - 1, 3 * tc.raw} {
				plan, err := PlanConversions(context.Background(), list, Options{MaxOutput: limit})
				if err != nil || planHas(plan, Raw, "") != (limit >= 2*tc.raw) {
					t.Fatalf("limit %d: %+v, %v", limit, plan.Targets, err)
				}
			}
		})
	}
}

func TestPlanCHDHDDRequiresRestorableGeometry(t *testing.T) {
	for _, version := range []uint32{4, 5} {
		source := filepath.Join(t.TempDir(), "disk.chd")
		if err := os.WriteFile(source, testfixture.CHDMetadata(version, "GDDD"), 0600); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanConversions(context.Background(), source, Options{})
		if err != nil || planHas(plan, Raw, "hdd") {
			t.Fatalf("offered unusable v%d HDD restoration: %+v, %v", version, plan.Targets, err)
		}
	}
}

func TestPlanZ3DSMetadata(t *testing.T) {
	for _, size := range []int{5, 6, 16} {
		metadata := make([]byte, size)
		metadata[0] = 1
		source := filepath.Join(t.TempDir(), "game.z3dsx")
		data := testfixture.Z3DSWithFrame("3DSX", testfixture.Z3DS("3DSX")[32:43], metadata)
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanConversions(context.Background(), source, Options{})
		if err != nil || planHas(plan, Raw, "") != (size != 6) {
			t.Errorf("metadata size %d: %+v, %v", size, plan.Targets, err)
		}
	}
}

func TestMultisessionCUERefusedBeforePublication(t *testing.T) {
	dir := t.TempDir()
	cue, output := filepath.Join(dir, "disc.cue"), filepath.Join(dir, "disc.chd")
	sources := map[string][]byte{
		cue:                             []byte("REM SESSION 01\nFILE track.bin BINARY\nTRACK 01 MODE1/2048\nINDEX 01 00:00:00\nREM LEAD-OUT 00:01:00\nREM SESSION 02\nTRACK 02 MODE1/2048\nINDEX 01 00:01:25\n"),
		filepath.Join(dir, "track.bin"): bytes.Repeat([]byte{0x55}, 256*2048),
	}
	for path, contents := range sources {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	opts := Options{Format: CHD, Tools: map[string]string{"chdman": filepath.Join(dir, "missing")}}
	plan, err := PlanConversions(ctx, cue, opts)
	if err != nil || planHas(plan, CHD, "cd") {
		t.Fatalf("offered multisession CUE: %+v, %v", plan.Targets, err)
	}
	if _, err := Convert(ctx, cue, output, opts); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("conversion returned %v", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected output: %v", err)
	}
	for path, want := range sources {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("source changed: %s, %v", path, err)
		}
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
	if err := os.WriteFile(filepath.Join(root, "second.rvz"), testfixture.RVZHeader(2048), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("first.iso\nsecond.rvz\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanConversions(context.Background(), source, Options{MaxOutput: 3 << 10})
	if err != nil || planHas(plan, Raw, "") {
		t.Fatalf("offered known Dolphin expansion over aggregate limit: %+v %v", plan, err)
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
