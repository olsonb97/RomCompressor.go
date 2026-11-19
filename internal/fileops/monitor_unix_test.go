//go:build !windows

package fileops

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestMonitorNSZManagerSockets(t *testing.T) {
	for _, tc := range []struct {
		parent string
		name   string
		allow  bool
	}{
		{"pymp-4hm8xy_j", "listener-to27jkz4", true},
		{"nsz-123/pymp-4hm8xy_j", "listener-to27jkz4", true},
		{"other/pymp-4hm8xy_j", "listener-to27jkz4", false},
		{"nsz-123/nested/pymp-4hm8xy_j", "listener-to27jkz4", false},
		{"not-a-manager", "listener-to27jkz4", false},
		{"pymp-4hm8xy_j", "other", false},
	} {
		t.Run(filepath.Join(tc.parent, tc.name), func(t *testing.T) {
			stage := t.TempDir()
			// Relative socket addresses keep even a deeply nested test temp
			// directory below the platform's sockaddr_un path length.
			t.Chdir(stage)
			if err := os.MkdirAll(tc.parent, 0700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(tc.parent, tc.name))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := scan(t.Context(), stage, 0, 12, true); (err == nil) != tc.allow {
				t.Fatalf("live NSZ scan: %v", err)
			}
			if err := Scan(t.Context(), stage, 0, 12); err == nil {
				t.Fatal("ordinary scan accepted a socket")
			}
			if err := RunMonitoredNSZ(t.Context(), stage, 0, func(context.Context) error { return nil }); err == nil {
				t.Fatal("final scan accepted a socket")
			}
			if tc.allow && scan(t.Context(), stage, 0, 2, true) == nil {
				t.Fatal("listener bypassed the entry limit")
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if err := RunMonitoredNSZ(t.Context(), stage, 0, func(context.Context) error { return nil }); err != nil {
				t.Fatalf("final scan after socket cleanup: %v", err)
			}
		})
	}
}

func TestNSZListenerNameDoesNotExemptOtherFileTypes(t *testing.T) {
	stage := t.TempDir()
	dir := filepath.Join(stage, "pymp-0b9d5g7i")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "listener-3r5f_7t9")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := scan(t.Context(), stage, 1, 20, true); err == nil {
		t.Fatal("regular file bypassed the byte limit")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", path); err != nil {
		t.Fatal(err)
	}
	if err := scan(t.Context(), stage, 1, 20, true); err == nil {
		t.Fatal("live NSZ scan accepted a symlink")
	}
}
