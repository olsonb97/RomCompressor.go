package process

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveMissingEngineHasTypedGuidance(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, configured := range []bool{false, true} {
		var tools map[string]string
		if configured {
			tools = map[string]string{"chdman": filepath.Join(t.TempDir(), "missing.exe")}
		}
		_, err := Resolve(tools, "chdman")
		var missing *ToolError
		if !errors.As(err, &missing) || missing.Engine != "chdman" || missing.Configured != configured {
			t.Fatalf("lookup category lost: %#v, %v", missing, err)
		}
		if !configured && !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("lookup cause lost: %v", err)
		}
		if !strings.Contains(err.Error(), "Options.Tools") || !strings.Contains(err.Error(), "--tool chdman=PATH") {
			t.Fatalf("lookup error is not actionable: %v", err)
		}
	}
}
