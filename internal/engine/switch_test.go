package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSwitchStatusRequiresExplicitSuccess(t *testing.T) {
	const success = `{"type":"summary","success":true,"errorCount":0}`
	const complete = `{"type":"complete","file":"output/game.nsz"}` + "\n"
	const verify = `{"type":"info","message":"[VERIFY NSZ] game.nsz"}` + "\n"
	const checked = `{"type":"info","action":"VERIFIED","message":"generated.ncz"}` + "\n"
	const skipped = `{"type":"info","action":"TICKETLESS","message":"unchecked.ncz"}` + "\n"
	for _, tc := range []struct {
		name, data        string
		output, verify    string
		confirmed, failed bool
	}{
		{"progress only", `{"type":"progress","currentWrite":12}`, "game.nsz", "", false, false},
		{"conversion success", complete + success, "game.nsz", "", true, false},
		{"conversion skipped verification", complete + skipped + success, "game.nsz", "", true, true},
		{"verification success", verify + checked + success, "", "[VERIFY NSZ] game.nsz", true, false},
		{"XCZ verification success", `{"type":"info","message":"[VERIFY XCZ] game.xcz"}` + "\n" + checked + success, "", "[VERIFY XCZ] game.xcz", true, false},
		{"verification without checked hashes", verify + success, "", "[VERIFY NSZ] game.nsz", false, true},
		{"only unrelated hashes checked", checked + verify + success, "", "[VERIFY NSZ] game.nsz", false, true},
		{"some files skipped verification", verify + checked + skipped + success, "", "[VERIFY NSZ] game.nsz", true, true},
		{"wrong output", complete + success, "other.nsz", "", false, true},
		{"wrong verification target", verify + success, "", "[VERIFY NSZ] other.nsz", false, true},
		{"skipped verification", success, "", "[VERIFY NSZ] game.nsz", false, true},
		{"reported error", complete + `{"type":"summary","success":false,"errorCount":1}`, "game.nsz", "", false, true},
		{"incomplete summary", complete + `{"type":"summary","success":true}`, "game.nsz", "", false, true},
		{"corruption before summary", complete + "{\"type\":\"info\",\"action\":\"CORRUPTED\"}\n" + success, "game.nsz", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := &switchStatus{output: tc.output, verifyMessage: tc.verify}
			for _, b := range []byte(tc.data) {
				if _, err := status.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			if err := status.consume(); err != nil {
				t.Fatal(err)
			}
			if status.confirmed != tc.confirmed || status.failed != tc.failed {
				t.Fatalf("unexpected status: %+v", status)
			}
		})
	}
}

func TestSwitchStatusDoesNotExposeDiagnostics(t *testing.T) {
	for _, line := range []string{"private-key-value", `{"type":"error","message":"private-key-value"}`} {
		_, err := (&switchStatus{}).Write([]byte(line + "\n"))
		if err == nil || strings.Contains(err.Error(), "private-key-value") {
			t.Fatalf("unsafe diagnostic: %v", err)
		}
	}
	if _, err := (&switchStatus{}).Write(bytes.Repeat([]byte{'a'}, 64*1024+1)); err == nil {
		t.Fatal("accepted an unbounded status line")
	}
}

func TestPrepareSwitchInputNormalizesEngineSuffix(t *testing.T) {
	dir := t.TempDir()
	stage := t.TempDir()
	data := []byte("synthetic Switch data")
	for _, name := range []string{"game.NSZ", "game.xcz", ".nsp"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		ext := strings.ToLower(filepath.Ext(name))
		enginePath, err := prepareSwitchInput(context.Background(), path, ext, stage, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(enginePath) != ext || filepath.Base(enginePath) == ext {
			t.Fatalf("NSZ cannot recognize %s", enginePath)
		}
		got, err := os.ReadFile(enginePath)
		if err != nil || !bytes.Equal(data, got) {
			t.Fatalf("read engine input: %v, %q", err, got)
		}
		if name == "game.xcz" && filepath.Dir(enginePath) == stage {
			t.Fatal("created an unnecessary staging copy")
		}
		if name != "game.xcz" && filepath.Dir(enginePath) != stage {
			t.Fatal("expected a private staging copy")
		}
	}
	if _, err := prepareSwitchInput(context.Background(), filepath.Join(dir, "game.NSZ"), ".nsz", stage, 1); err == nil || !strings.Contains(err.Error(), "exceeds output limit") {
		t.Fatalf("expected staging size error, got %v", err)
	}
}

func TestSwitchKeysPathValidatedBeforeEngine(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, keys := range []string{"", dir, empty, filepath.Join(dir, "missing")} {
		_, _, err := resolveSwitch(Config{Keys: keys, Tools: map[string]string{"nsz": filepath.Join(dir, "missing-engine")}})
		if err == nil || !strings.Contains(err.Error(), "keys") {
			t.Fatalf("expected keys-file error, got %v", err)
		}
	}
	if err := os.Truncate(empty, (1<<20)+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveSwitch(Config{Keys: empty, Tools: map[string]string{"nsz": filepath.Join(dir, "missing-engine")}}); err == nil || !strings.Contains(err.Error(), "keys file exceeds") {
		t.Fatalf("expected oversized keys-file rejection, got %v", err)
	}
}
