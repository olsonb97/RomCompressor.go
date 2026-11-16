package testfixture

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// DirectoryLink creates a directory link, or skips when the platform does not
// permit one. Windows junctions do not require the symlink privilege.
func DirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Junctions can be created without the Windows symlink privilege and
		// some Go versions classify them as irregular, rather than symlinks.
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"New-Item -ItemType Junction -Path $env:ROMCOMPRESSOR_TEST_JUNCTION -Target $env:ROMCOMPRESSOR_TEST_JUNCTION_TARGET -ErrorAction Stop | Out-Null")
		cmd.Env = append(os.Environ(), "ROMCOMPRESSOR_TEST_JUNCTION="+link, "ROMCOMPRESSOR_TEST_JUNCTION_TARGET="+target)
		if err := cmd.Run(); err != nil {
			t.Skipf("junctions unavailable: %v", err)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}
