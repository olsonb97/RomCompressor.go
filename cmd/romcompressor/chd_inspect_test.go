package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestInspectExplainsCHDMediaWithoutGuessing(t *testing.T) {
	for tag, want := range map[string]string{
		"DVD ": "restore with --media dvd", "CHT2": "restore with --media cd", "GDDD": "restore with --media hdd",
		"AVAV": "laserdisc (restoration is unsupported)", "": "Media: unidentified",
	} {
		t.Run(tag, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.chd")
			data := testfixture.CHDMetadata(5)
			if tag != "" {
				data = testfixture.CHDMetadata(5, tag)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{"inspect", path}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), want) || stderr.Len() != 0 {
				t.Fatalf("inspect: %d %s %s; want %q", code, &stdout, &stderr, want)
			}
			stdout.Reset()
			if code := run(context.Background(), []string{"inspect", path, "--json"}, &stdout, &stderr); code != 0 || !json.Valid(stdout.Bytes()) || stderr.Len() != 0 {
				t.Fatalf("inspect JSON: %d %s %s", code, &stdout, &stderr)
			}
			var info map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
				t.Fatal(err)
			}
			if tag == "" && info["media"] != nil || tag != "" && info["media"] == nil {
				t.Fatalf("JSON media field does not reflect explicit tags: %s", &stdout)
			}
		})
	}
}
