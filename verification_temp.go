package romcompressor

import (
	"fmt"
	"os"
	"path/filepath"
)

// normalizeVerifyTempDir validates a caller-selected scratch parent without
// creating it. Even a native verifier that needs no scratch checks this setting.
func normalizeVerifyTempDir(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("verification temporary directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("verification temporary directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("verification temporary directory must be an existing directory")
	}
	return absolute, nil
}
