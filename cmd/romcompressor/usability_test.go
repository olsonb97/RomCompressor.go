package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestArchiveWrappingReportsUnusedOptionsAsOneJSONError(t *testing.T) {
	for _, format := range []string{"zip", "gzip", "7z"} {
		for _, tc := range []struct {
			name  string
			data  []byte
			flag  string
			value string
			want  string
		}{
			{"image.chd", testfixture.CHDMetadata(5, "CHT2"), "--media", "hdd", "media is only used for CHD conversion"},
			{"image.nsz", []byte("PFS0"), "--keys", "missing.keys", "keys are only used for Switch compression"},
		} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, tc.name)
				if err := os.WriteFile(source, tc.data, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"compress", source, "-f", format, tc.flag, tc.value, "--tool", "7z=" + filepath.Join(dir, "missing-engine"), "--json"}
				var stdout, stderr bytes.Buffer
				if code := run(context.Background(), args, &stdout, &stderr); code != 1 {
					t.Fatalf("unused option status = %d, stdout=%s, stderr=%s", code, &stdout, &stderr)
				}
				decoder := json.NewDecoder(&stdout)
				var result map[string]string
				if err := decoder.Decode(&result); err != nil || !strings.Contains(result["error"], tc.want) {
					t.Fatalf("missing actionable JSON option error: %v, %v", result, err)
				}
				if err := decoder.Decode(new(any)); err != io.EOF || stderr.Len() != 0 {
					t.Fatalf("expected one JSON document and no progress: %v, stderr=%s", err, &stderr)
				}
				if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, tc.data) {
					t.Fatalf("rejected option changed original: %v", err)
				}
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
					t.Fatalf("rejected option left output/staging: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestMissingValuesDoNotConsumeKnownOptions(t *testing.T) {
	for _, test := range []struct {
		args []string
		flag string
	}{
		{[]string{"compress", "game.iso", "-f", "-o", "copy.cso"}, "format"},
		{[]string{"compress", "game.iso", "-f", "cso", "-o", "-j"}, "output"},
		{[]string{"compress", "game.iso", "-f", "cso", "--max-output", "-o"}, "max-output"},
		{[]string{"compress", "game.nsp", "-f", "nsz", "--keys", "-j"}, "keys"},
	} {
		if _, err := parse(test.args); err == nil || !strings.Contains(err.Error(), "--"+test.flag+" requires a value") {
			t.Fatalf("%v: %v", test.args, err)
		}
	}
	args, err := parse([]string{"compress", "game.iso", "-f", "cso", "--output=-j"})
	if err != nil || args.output != "-j" || args.json {
		t.Fatalf("literal option-like filename: %+v, %v", args, err)
	}
}

func TestByteLimitsSupportBinaryUnitsWithoutOverflow(t *testing.T) {
	for _, test := range []struct {
		text string
		want int64
	}{
		{"1", 1}, {"4096", 4096}, {"2KiB", 2048}, {"3MiB", 3 << 20},
		{"4GiB", 4 << 30}, {"1TiB", 1 << 40}, {"2 gib", 2 << 30},
		{"9223372036854775807", math.MaxInt64}, {"8388607TiB", 8388607 << 40},
	} {
		actual, err := parseByteLimit(test.text)
		if err != nil || actual != test.want {
			t.Errorf("%q: got %d, %v; want %d", test.text, actual, err, test.want)
		}
	}
	for _, invalid := range []string{"", "0", "-1", "0GiB", "-2MiB", "1.5GiB", "2GB", "GiB", "8388608TiB", "9223372036854775808", "9007199254740992KiB"} {
		if _, err := parseByteLimit(invalid); err == nil {
			t.Errorf("accepted invalid or overflowing size %q", invalid)
		}
	}
	args, err := parse([]string{"compress", "game.iso", "-f", "cso", "--max-output", "4GiB"})
	if err != nil || args.maxOutput != 4<<30 {
		t.Fatalf("unit option was not passed through: %+v, %v", args, err)
	}
}

func TestInstalledVersionAndStampPrecedence(t *testing.T) {
	for _, test := range []struct {
		stamp, module, sum, want string
	}{
		{"dev", "v0.1.0", "h1:installed", "v0.1.0"}, {"v0.2.0", "v0.1.0", "", "v0.2.0"},
		{"dev", "(devel)", "", "dev"}, {"", "v0.1.0", "h1:installed", "v0.1.0"}, {"", "", "", "dev"},
		{"dev", "v0.1.0+dirty", "", "dev"}, {"dev", "v0.1.0", "", "dev"},
	} {
		info := &debug.BuildInfo{Main: debug.Module{Version: test.module, Sum: test.sum}}
		if actual := buildVersion(test.stamp, info); actual != test.want {
			t.Errorf("stamp=%q module=%q: got %q, want %q", test.stamp, test.module, actual, test.want)
		}
	}
	if buildVersion("dev", nil) != "dev" {
		t.Fatal("missing build information lost the development fallback")
	}
	before := version
	t.Cleanup(func() { version = before })
	version = "v0.2.0"
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version", "--json"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("--version: %d %s %s", code, &stdout, &stderr)
	}
	var result map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result["version"] != version {
		t.Fatalf("version response: %s, %v", &stdout, err)
	}
}

func writeCLIArchive(t *testing.T, path string, entries map[string]string) []byte {
	t.Helper()
	var contents bytes.Buffer
	writer := zip.NewWriter(&contents)
	for name, data := range entries {
		member, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(member, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return contents.Bytes()
}

func TestInspectQuotesArchiveNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.zip")
	name := "line\nbreak\x1b[31m.gba"
	writeCLIArchive(t, path, map[string]string{name: "generated data"})
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"inspect", path}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("inspect: %d %s %s", code, &stdout, &stderr)
	}
	if !strings.Contains(stdout.String(), strconv.Quote(name)) || strings.Contains(stdout.String(), "line\nbreak") || strings.ContainsRune(stdout.String(), '\x1b') {
		t.Fatalf("unsafe or unreadable member name: %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"decompress", path, "--json"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "safe output path with -o") || stderr.Len() != 0 {
		t.Fatalf("automatic output accepted a control-bearing name: %d %s %s", code, &stdout, &stderr)
	}
	output := filepath.Join(filepath.Dir(path), "safe.gba")
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"decompress", path, "-o", output, "--json"}, &stdout, &stderr); code != 0 || !json.Valid(stdout.Bytes()) || stderr.Len() != 0 {
		t.Fatalf("explicit safe output was rejected: %d %s %s", code, &stdout, &stderr)
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "generated data" {
		t.Fatalf("safe extraction changed member data: %q, %v", data, err)
	}
}

func TestArchiveSelectionGuidanceBeforeStaging(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "games.zip")
	original := writeCLIArchive(t, source, map[string]string{"first.gba": "one", "second.gba": "two"})
	for _, explicit := range []bool{false, true} {
		args := []string{"decompress", source}
		if explicit {
			args = append(args, "-o", filepath.Join(dir, "selected.gba"))
		}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
			t.Fatalf("ambiguous extraction: %d %s %s", code, &stdout, &stderr)
		}
		if !strings.Contains(stderr.String(), "--entry NAME") || !strings.Contains(stderr.String(), "--all -o DIRECTORY") || strings.Contains(stderr.String(), "converting") {
			t.Fatalf("missing selection guidance or late validation: %s", &stderr)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("selection failure staged output: %v, %v", entries, err)
	}
	if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("inspection changed original archive: %v", err)
	}
	output := filepath.Join(dir, "all games")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"decompress", source, "--all", "-o", output, "--json"}, &stdout, &stderr); code != 0 || !json.Valid(stdout.Bytes()) || stderr.Len() != 0 {
		t.Fatalf("full extraction option was blocked: %d %s %s", code, &stdout, &stderr)
	}
	for name, want := range map[string]string{"first.gba": "one", "second.gba": "two"} {
		if data, err := os.ReadFile(filepath.Join(output, name)); err != nil || string(data) != want {
			t.Fatalf("restored %q: %q, %v", name, data, err)
		}
	}
	info := romcompressor.FileInfo{Format: romcompressor.SevenZip, Entries: []romcompressor.Entry{{Name: "one"}, {Name: "two"}}}
	if err := archiveSelectionError(info, romcompressor.Options{}); err == nil || !strings.Contains(err.Error(), "--entry NAME") || strings.Contains(err.Error(), "--all") {
		t.Fatalf("7z guidance suggested unsupported full extraction: %v", err)
	}
}

func TestAutomaticArchiveOutputPreservesUnicodeNames(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "international.zip")
	name := "冒険 👩‍💻.gba"
	writeCLIArchive(t, source, map[string]string{name: "generated unicode payload"})
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"decompress", source, "--json"}, &stdout, &stderr); code != 0 || !json.Valid(stdout.Bytes()) || stderr.Len() != 0 {
		t.Fatalf("ordinary Unicode/ZWJ filename was rejected: %d %s %s", code, &stdout, &stderr)
	}
	if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != "generated unicode payload" {
		t.Fatalf("Unicode member was not preserved: %q, %v", data, err)
	}
	for _, separator := range []rune{'\u2028', '\u2029'} {
		info := romcompressor.FileInfo{Format: romcompressor.ZIP, Entries: []romcompressor.Entry{{Name: "line" + string(separator) + "break.gba"}}}
		if _, err := decompressedOutput(t.Context(), source, romcompressor.Options{}, info); err == nil || !strings.Contains(err.Error(), "safe output path with -o") {
			t.Fatalf("line separator became an implicit filename: %v", err)
		}
	}
}

func TestDefaultOutputCollisionExplainsRecovery(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "original.gba")
	original := []byte("generated cartridge bytes")
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"compress", source, "-f", "zip", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("compress: %d %s %s", code, &stdout, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"decompress", source + ".zip", "--json"}, &stdout, &stderr); code != 1 || stderr.Len() != 0 {
		t.Fatalf("collision: %d %s %s", code, &stdout, &stderr)
	}
	var failure map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil || !strings.Contains(failure["error"], source) || !strings.Contains(failure["error"], "--output") {
		t.Fatalf("collision did not identify the destination and recovery: %s, %v", &stdout, err)
	}
	if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("collision changed original: %v", err)
	}
}

func TestBatchProgressIdentifiesJobs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"first.gba", "second.gba"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("generated bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "jobs.json")
	manifest := `{"jobs":[{"source":"first.gba","output":"first.zip","format":"zip"},{"source":"second.gba","output":"second.zip","format":"zip"}]}`
	if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"batch", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("batch: %d %s %s", code, &stdout, &stderr)
	}
	for _, marker := range []string{"[1/2]", "[2/2]", "first.gba", "second.gba"} {
		if !strings.Contains(stderr.String(), marker) {
			t.Errorf("batch progress omitted %q: %s", marker, &stderr)
		}
	}
	if strings.Count(stderr.String(), "[1/2]") != 1 || strings.Count(stderr.String(), "[2/2]") != 1 {
		t.Fatalf("job headers repeated for phases: %s", &stderr)
	}
}

func TestFormatsHonorsOverridesAndReportsNativePBPOperations(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tool, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, override := range []bool{false, true} {
		args := []string{"formats", "--json"}
		if override {
			args = append(args, "--tool", "psxpackager="+tool)
		}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("formats: %d %s %s", code, &stdout, &stderr)
		}
		var capabilities []romcompressor.FormatCapability
		if err := json.Unmarshal(stdout.Bytes(), &capabilities); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, capability := range capabilities {
			if capability.Format != romcompressor.PBP {
				continue
			}
			found = true
			if capability.Available != override || capability.Compress.Available != override || !capability.Decompress.Available || !capability.Verify.Available || capability.Decompress.Engine != "native" || capability.Verify.Engine != "native" {
				t.Fatalf("misleading PBP availability: %+v", capability)
			}
		}
		if !found {
			t.Fatal("PBP is missing from the format list")
		}
		var objects []map[string]json.RawMessage
		if err := json.Unmarshal(stdout.Bytes(), &objects); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"format", "engine", "available", "description", "compress", "decompress", "verify"} {
			if _, ok := objects[0][key]; !ok {
				t.Errorf("format JSON lost flat or operation field %q", key)
			}
		}
	}
}

func TestHumanTrimPreviewReportsProgressWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.nds")
	original := testfixture.NDS(false)
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"trim", source, "--dry-run"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "removable") || stderr.Len() == 0 {
		t.Fatalf("preview: %d %s %s", code, &stdout, &stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("preview wrote files: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(source); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("preview changed original: %v", err)
	}
}
