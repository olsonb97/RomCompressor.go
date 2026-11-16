package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	romcompressor "github.com/olsonb97/romcompressor.go"
)

func TestArgumentsInterspersed(t *testing.T) {
	args, err := parse([]string{"compress", "-j", "game.iso", "--format=cso", "-o", "copy.cso", "--max-output", "1234", "--tool", "chdman=C:/Program Files/chdman.exe"})
	if err != nil {
		t.Fatal(err)
	}
	if !args.json || args.format != "cso" || args.output != "copy.cso" || args.maxOutput != 1234 || args.tools["chdman"] != "C:/Program Files/chdman.exe" {
		t.Fatalf("unexpected arguments: %#v", args)
	}
	args, err = parse([]string{"compress", "-f", "zip", "--", "-game.gba"})
	if err != nil || len(args.paths) != 1 || args.paths[0] != "-game.gba" {
		t.Fatalf("literal filename: %#v %v", args, err)
	}
}

func TestPlaylistDefaultOutput(t *testing.T) {
	for _, extension := range []string{".m3u", ".M3U", ".m3u8", ".M3U8"} {
		input := filepath.Join("games", "Two Discs"+extension)
		output, err := defaultOutput(context.Background(), input, romcompressor.Options{Format: romcompressor.CHD})
		if err != nil || output != filepath.Join("games", "Two Discs-chd.m3u") {
			t.Fatalf("playlist default: %q %v", output, err)
		}
	}
}

func TestCLIParseErrorsRespectJSONPosition(t *testing.T) {
	for _, tc := range []struct {
		args []string
		json bool
	}{
		{[]string{"compress", "--unknown", "--json"}, true},
		{[]string{"compress", "--unknown", "-j"}, true},
		{[]string{"missing-command", "--json"}, true},
		{[]string{"compress", "--unknown", "--", "--json"}, false},
		{[]string{"compress", "--unknown", "-o", "-j"}, true},
		{[]string{"compress", "--unknown", "--output=-j"}, false},
		{[]string{"compress", "--unknown", "--output=--json"}, false},
		{[]string{"compress", "--unknown", "-o", "-j", "--json"}, true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tc.args, &stdout, &stderr); code != 1 {
				t.Fatalf("exit status %d", code)
			}
			if tc.json {
				var failure map[string]string
				if stderr.Len() != 0 || json.Unmarshal(stdout.Bytes(), &failure) != nil || failure["error"] == "" {
					t.Fatalf("expected JSON error: stdout=%s stderr=%s", &stdout, &stderr)
				}
			} else if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("expected human error: stdout=%s stderr=%s", &stdout, &stderr)
			}
		})
	}
}

func TestCLIFormatsListsNativeWUX(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"formats"}, &stdout, &stderr); code != 0 {
		t.Fatalf("formats: %d %s", code, &stderr)
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "wux ") {
			if strings.Count(line, "built in") != 3 || strings.Contains(line, "JWUDTool") {
				t.Fatalf("misleading WUX availability: %s", line)
			}
			return
		}
	}
	t.Fatal("WUX missing from format list")
}

func TestArgumentsRejectMistakes(t *testing.T) {
	for _, argv := range [][]string{
		{"compress", "game.iso"},
		{"compress", "game.iso", "-f", "raw"},
		{"compress", "game.iso", "-f", "cso", "--format", "zso"},
		{"compress", "game.iso", "-f", "cso", "--overwrite"},
		{"compress", "game.iso", "-f", "madeup"},
		{"compress", "game.iso", "-f", "cso", "-o", "-"},
		{"compress", "game.iso", "-f", "cso", "--max-output", "0"},
		{"compress", "game.iso", "-f", "cso", "--media", "cartridge"},
		{"compress", "game.iso", "-f", "cso", "--tool", "bogus=exe"},
		{"compress", "game.iso", "-f", "cso", "--tool", "chdman=a", "--tool", "chdman=b"},
		{"decompress", "game.cso", "-f", "cso"},
		{"inspect", "game.zip", "-o", "out"},
		{"inspect", "game.zip", "--json=true"},
		{"verify", "game.zip", "--format", "zip"},
		{"formats", "game.iso"},
		{"batch", "jobs.json", "--entry", "game.iso"},
		{"help", "madeup"},
		{"version", "--tool", "7z=7z"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			if _, err := parse(argv); err == nil {
				t.Fatal("accepted invalid arguments")
			}
		})
	}
}

func TestCLIHelpAndJSONErrors(t *testing.T) {
	for _, command := range []string{"compress", "decompress", "inspect", "verify", "formats", "batch", "version"} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{command, "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
			t.Fatalf("%s help: %d %s %s", command, code, &stdout, &stderr)
		}
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"inspect", "--json", filepath.Join(t.TempDir(), "missing.zip")}, &stdout, &stderr)
	var result map[string]string
	if code != 1 || stderr.Len() != 0 || json.Unmarshal(stdout.Bytes(), &result) != nil || result["error"] == "" {
		t.Fatalf("invalid JSON failure: %d %s %s", code, &stdout, &stderr)
	}
}

func TestCLIRoundTripAndOverwriteProtection(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "original.gba")
	compressed := source + ".zip"
	output := filepath.Join(dir, "restored.gba")
	payload := bytes.Repeat([]byte("synthetic cartridge data\x00"), 4096)
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"compress", source, "-f", "zip", "--json"},
		{"inspect", compressed, "--json"},
		{"verify", compressed, "--json"},
		{"decompress", compressed, "-o", output, "--json"},
	}
	for _, argv := range commands {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), argv, &stdout, &stderr); code != 0 || !json.Valid(stdout.Bytes()) {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", argv, code, &stdout, &stderr)
		}
	}
	restored, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(payload, restored) {
		t.Fatalf("round trip: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"decompress", compressed, "--json"}, &stdout, &stderr); code == 0 {
		t.Fatal("overwrote original through default output")
	}
	original, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(payload, original) {
		t.Fatal("original changed")
	}
}

func TestCLIECMRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "game.bin")
	archive := filepath.Join(dir, "game.ecm")
	restored := filepath.Join(dir, "restored.bin")
	data := bytes.Repeat([]byte("generated CD data"), 4000)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"compress", source, "-f", "ecm", "--json"},
		{"inspect", archive, "--json"},
		{"verify", archive, "--json"},
		{"decompress", archive, "-o", restored, "--json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 || !json.Valid(stdout.Bytes()) {
			t.Fatalf("%v: %d, %s, %s", args, code, &stdout, &stderr)
		}
	}
	recovered, err := os.ReadFile(restored)
	if err != nil || !bytes.Equal(recovered, data) {
		t.Fatalf("restored ECM payload differs: %v", err)
	}
}

func TestManifestResolutionAndStrictness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	valid := `{"jobs":[{"source":"game.iso","output":"game.cso","format":"cso"}]}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	jobs, err := readManifest(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Source != filepath.Join(dir, "game.iso") || jobs[0].Output != filepath.Join(dir, "game.cso") {
		t.Fatalf("wrong relative paths: %#v", jobs)
	}
	for _, content := range []string{
		`{"jobs":[]}`, `{"jobs":null}`, `null`, `{"jobs":[{}]}`, valid + ` {}`,
		`{"jobs":[{"source":"game.iso","output":"out.cso","format":"cso","overwrite":true}]}`,
		`{"jobs":[],"unknown":1}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readManifest(context.Background(), path); err == nil {
			t.Fatalf("accepted malformed manifest: %s", content)
		}
	}
}

func TestBatchPartialFailureSingleJSONDocument(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "game.gba"), []byte("test ROM"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"jobs":[{"source":"game.gba","output":"game.zip","format":"zip"},{"source":"missing.gba","output":"missing.zip","format":"zip"}]}`
	path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"batch", path, "--json"}, &stdout, &stderr)
	var result struct {
		Success bool
		Results []json.RawMessage
	}
	if code != 1 || stderr.Len() != 0 || json.Unmarshal(stdout.Bytes(), &result) != nil || result.Success || len(result.Results) != 2 {
		t.Fatalf("batch: %d %s %s", code, &stdout, &stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "game.zip")); err != nil {
		t.Fatalf("lost successful output: %v", err)
	}
}

func TestBatchSwitchKeysDoNotAffectUnrelatedJobs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"game.gba", "source.nsp", "packed.NSZ"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("PFS0"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "jobs.json")
	manifest := `{"jobs":[
		{"source":"game.gba","output":"game.zip","format":"zip"},
		{"source":"source.nsp","output":"out.nsz","format":"nsz"},
		{"source":"packed.NSZ","output":"restored.nsp","format":"raw"}
	]}`
	if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"batch", path, "--keys", filepath.Join(dir, "missing.keys"), "--json"}
	var result struct {
		Results []romcompressor.JobResult
	}
	if code := run(context.Background(), args, &stdout, &stderr); code != 1 || json.Unmarshal(stdout.Bytes(), &result) != nil || len(result.Results) != 3 {
		t.Fatalf("batch: %d %s %s", code, &stdout, &stderr)
	}
	if result.Results[0].Error != "" {
		t.Fatalf("unrelated ZIP job failed: %s", result.Results[0].Error)
	}
	for _, item := range result.Results[1:] {
		if !strings.Contains(item.Error, "keys path must name an existing") {
			t.Fatalf("Switch job did not receive the key path: %#v", item)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "game.zip")); err != nil {
		t.Fatalf("missing ZIP output: %v", err)
	}
}

func TestBatchCancellationExitCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, []byte(`{"jobs":[{"source":"game.iso","output":"game.cso","format":"cso"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"batch", path, "--json"}, &stdout, &stderr); code != 130 || !json.Valid(stdout.Bytes()) {
		t.Fatalf("cancel: %d %s %s", code, &stdout, &stderr)
	}
}
