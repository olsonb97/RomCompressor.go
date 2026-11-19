package engine

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

// The test binary acts as an engine only in child processes explicitly selected
// by this test. Generated payloads exercise scratch ownership, not interoperability.
func TestMain(m *testing.M) {
	if engine := os.Getenv("ROMCOMPRESSOR_TEST_VERIFY_ENGINE"); engine != "" {
		if err := runVerificationTestEngine(engine); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(7)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runVerificationTestEngine(engine string) error {
	argument := func(name string) string {
		for i, value := range os.Args {
			if value == name && i+1 < len(os.Args) {
				return os.Args[i+1]
			}
		}
		return ""
	}
	stage := os.Getenv("HOME")
	switch engine {
	case "dolphin":
		stage = argument("-u")
	case "azahar":
		stage = argument("-o")
	}
	if stage == "" {
		return errors.New("missing verification scratch directory")
	}
	mode := os.Getenv("ROMCOMPRESSOR_TEST_VERIFY_MODE")
	scratch := os.Getenv("TMPDIR")
	for _, name := range []string{"TMP", "TEMP"} {
		if os.Getenv(name) != scratch {
			return fmt.Errorf("engine %s was not redirected to private scratch", name)
		}
	}
	if engine == "nsz" && filepath.Separator == '/' {
		homeDir, err := os.Stat(stage)
		if err != nil {
			return err
		}
		tempDir, err := os.Stat(scratch)
		if err != nil || !os.SameFile(homeDir, tempDir) {
			return errors.New("NSZ socket path does not refer to the selected stage")
		}
		dir := filepath.Join(scratch, "pymp-abcdefgh")
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		defer os.Remove(dir)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(dir, "listener-abcdefgh")})
		if err != nil {
			return err
		}
		if mode == "orphan" && filepath.Ext(os.Args[len(os.Args)-1]) == ".nsz" {
			listener.SetUnlinkOnClose(false)
		}
		defer listener.Close()
		if err := os.WriteFile(os.Getenv("ROMCOMPRESSOR_TEST_VERIFY_MARKER")+".temp", []byte(scratch), 0600); err != nil {
			return err
		}
		// Keep the live socket visible for at least one monitor scan.
		time.Sleep(200 * time.Millisecond)
	} else if scratch != stage {
		return errors.New("engine temp path differs from the selected stage")
	}
	if err := os.WriteFile(os.Getenv("ROMCOMPRESSOR_TEST_VERIFY_MARKER"), []byte(stage), 0600); err != nil {
		return err
	}
	size := 64
	if mode == "limit" {
		size = 64 << 10
	}
	if err := os.WriteFile(filepath.Join(stage, "engine-scratch"), bytes.Repeat([]byte{0x5a}, size), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(os.Getenv("ROMCOMPRESSOR_TEST_VERIFY_MARKER")+".ready", []byte{1}, 0600); err != nil {
		return err
	}
	if mode == "failure" {
		return errors.New("generated engine failure")
	}
	if mode == "cancel" || mode == "limit" {
		for {
			time.Sleep(time.Hour)
		}
	}
	switch engine {
	case "dolphin":
		fmt.Printf("%x\n", sha1.Sum([]byte("generated logical disc")))
	case "azahar":
		payload := bytes.Repeat([]byte{0x5a}, 1024)
		copy(payload, "3DSX")
		name := strings.TrimSuffix(filepath.Base(argument("-x")), filepath.Ext(argument("-x"))) + ".3dsx"
		return os.WriteFile(filepath.Join(stage, name), payload, 0600)
	case "nsz":
		name := filepath.Base(os.Args[len(os.Args)-1])
		encoder := json.NewEncoder(os.Stdout)
		if mode == "orphan" {
			// Both conversions copy the generated input exactly; the only
			// defect is the socket the last invocation leaves in its stage.
			ext := filepath.Ext(name)
			suffix := ".nsz"
			if ext == suffix {
				suffix = ".nsp"
			}
			output := filepath.Join(argument("--output"), strings.TrimSuffix(name, ext)+suffix)
			data, err := os.ReadFile(os.Args[len(os.Args)-1])
			if err != nil {
				return err
			}
			if err := os.WriteFile(output, data, 0600); err != nil {
				return err
			}
			if err := encoder.Encode(map[string]any{"type": "complete", "file": output}); err != nil {
				return err
			}
			return encoder.Encode(map[string]any{"type": "summary", "success": true, "errorCount": 0})
		}
		for _, message := range []map[string]any{
			{"type": "info", "message": "[VERIFY NSZ] " + name},
			{"type": "info", "action": "VERIFIED"},
			{"type": "summary", "success": true, "errorCount": 0},
		} {
			if err := encoder.Encode(message); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestSwitchConversionRefusesLingeringWorkerSocket(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("NSZ uses Unix sockets for its POSIX workers")
	}
	dir, outputDir := t.TempDir(), t.TempDir()
	source, keys := filepath.Join(dir, "game.nsp"), filepath.Join(dir, "test.keys")
	for _, path := range []string{source, keys} {
		if err := os.WriteFile(path, []byte("generated fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ROMCOMPRESSOR_TEST_VERIFY_ENGINE", "nsz")
	t.Setenv("ROMCOMPRESSOR_TEST_VERIFY_MODE", "orphan")
	t.Setenv("ROMCOMPRESSOR_TEST_VERIFY_MARKER", filepath.Join(dir, "marker"))
	opts := Config{Format: kind.NSZ, Keys: keys, MaxOutput: 4096, Tools: map[string]string{"nsz": os.Args[0]}}
	err := convertSwitch(t.Context(), source, filepath.Join(outputDir, "packed.nsz"), kind.Raw, opts)
	if err == nil || !strings.Contains(err.Error(), "link or special file") {
		t.Fatalf("expected the final scratch check to reject a leftover socket, got %v", err)
	}
	if files, err := os.ReadDir(outputDir); err != nil || len(files) != 0 {
		t.Fatalf("output or staging survived rejection: %v, %v", files, err)
	}
}

func TestStandaloneVerificationUsesSelectedScratchParent(t *testing.T) {
	for _, engine := range []string{"dolphin", "azahar", "nsz"} {
		for _, mode := range []string{"success", "failure", "cancel", "limit"} {
			t.Run(engine+"/"+mode, func(t *testing.T) {
				directory := t.TempDir()
				parent, fallback := filepath.Join(directory, "selected"), filepath.Join(directory, "default")
				if engine == "nsz" && filepath.Separator == '/' {
					parent = filepath.Join(directory, strings.Repeat("selected", 12))
				}
				for _, path := range []string{parent, fallback} {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				sentinel := filepath.Join(parent, "keep.txt")
				if err := os.WriteFile(sentinel, []byte("existing user data"), 0600); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
					t.Setenv(name, fallback)
				}
				marker := filepath.Join(directory, "observed-stage")
				t.Setenv("ROMCOMPRESSOR_TEST_VERIFY_ENGINE", engine)
				t.Setenv("ROMCOMPRESSOR_TEST_VERIFY_MODE", mode)
				t.Setenv("ROMCOMPRESSOR_TEST_VERIFY_MARKER", marker)
				format, source, original := verificationTempFixture(t, directory, engine)
				keys := filepath.Join(directory, "generated.keys")
				if err := os.WriteFile(keys, []byte("generated test fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				opts := Config{TempDir: parent, MaxOutput: 4096, Keys: keys,
					Tools: map[string]string{"dolphin-tool": os.Args[0], "azahar": os.Args[0], "nsz": os.Args[0]}}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var cancelDone chan struct{}
				if mode == "cancel" {
					cancelDone = make(chan struct{})
					go func() {
						defer close(cancelDone)
						for {
							if _, err := os.Stat(marker + ".ready"); err == nil {
								cancel()
								return
							}
							select {
							case <-ctx.Done():
								return
							case <-time.After(10 * time.Millisecond):
							}
						}
					}()
				}
				err := Verify(ctx, source, format, opts)
				cancel()
				if cancelDone != nil {
					<-cancelDone
				}
				switch mode {
				case "success":
					if err != nil {
						t.Fatal(err)
					}
				case "cancel":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("expected cancellation, got %v", err)
					}
				case "limit":
					if err == nil || !strings.Contains(err.Error(), "output limit") {
						t.Fatalf("expected monitored scratch limit, got %v", err)
					}
				default:
					if err == nil {
						t.Fatal("engine failure accepted")
					}
				}
				observed, readErr := os.ReadFile(marker)
				if readErr != nil || filepath.Dir(string(observed)) != parent {
					t.Fatalf("engine scratch escaped selected parent: %q, %v", observed, readErr)
				}
				if _, err := os.Lstat(string(observed)); !os.IsNotExist(err) {
					t.Fatalf("verification scratch was not cleaned: %v", err)
				}
				if engine == "nsz" && filepath.Separator == '/' {
					alias, err := os.ReadFile(marker + ".temp")
					if err != nil || string(alias) == string(observed) {
						t.Fatalf("missing short socket path: %v", err)
					}
					if _, err := os.Lstat(filepath.Dir(string(alias))); !os.IsNotExist(err) {
						t.Fatalf("short NSZ path survived %s: %v", mode, err)
					}
				}
				entries, readErr := os.ReadDir(parent)
				if readErr != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
					t.Fatalf("verification changed preexisting scratch-parent entries: %v, %v", entries, readErr)
				}
				if entries, err := os.ReadDir(fallback); err != nil || len(entries) != 0 {
					t.Fatalf("verification wrote in OS default temp: %v, %v", entries, err)
				}
				for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
					if os.Getenv(name) != fallback {
						t.Fatalf("verification changed parent-process %s", name)
					}
				}
				if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, original) {
					t.Fatalf("verification changed original: %v", err)
				}
				if actual, err := os.ReadFile(sentinel); err != nil || string(actual) != "existing user data" {
					t.Fatalf("verification changed parent sentinel: %v", err)
				}
			})
		}
	}
}

func verificationTempFixture(t *testing.T, directory, engine string) (string, string, []byte) {
	t.Helper()
	format, extension, data := kind.RVZ, ".rvz", testfixture.RVZHeader(2048)
	switch engine {
	case "azahar":
		payload := bytes.Repeat([]byte{0x5a}, 1024)
		copy(payload, "3DSX")
		frame := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0, 1, 0x20, 0}, payload...)
		format, extension, data = kind.Z3DSX, ".z3dsx", testfixture.Z3DSWithFrame("3DSX", frame, nil)
	case "nsz":
		format, extension, data = kind.NSZ, ".NSZ", []byte("PFS0generated container")
	}
	path := filepath.Join(directory, "game"+extension)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return format, path, data
}

func TestVerificationStageDefaultAndReplacementOwnership(t *testing.T) {
	var original string
	err := withVerificationStage(context.Background(), Config{}, "romcompressor-verify-test-", 1024, func(_ context.Context, stage fileops.Stage) error {
		original = stage.Path()
		if filepath.Clean(filepath.Dir(original)) != filepath.Clean(os.TempDir()) {
			t.Fatalf("empty TempDir changed the OS default: %s", original)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(original); !os.IsNotExist(err) {
		t.Fatalf("default scratch remains: %v", err)
	}
	parent := t.TempDir()
	var replaced string
	err = withVerificationStage(context.Background(), Config{TempDir: parent}, "scratch-", 1024, func(_ context.Context, stage fileops.Stage) error {
		replaced = stage.Path()
		if err := os.Rename(replaced, replaced+"-moved"); err != nil {
			return err
		}
		if err := os.Mkdir(replaced, 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(replaced, "keep.txt"), []byte("replacement user data"), 0600)
	})
	if err == nil || !strings.Contains(err.Error(), "staging directory") {
		t.Fatalf("replacement staging directory accepted: %v", err)
	}
	if actual, err := os.ReadFile(filepath.Join(replaced, "keep.txt")); err != nil || string(actual) != "replacement user data" {
		t.Fatalf("cleanup removed replacement directory: %v", err)
	}
}
