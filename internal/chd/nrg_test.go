package chd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/olsonb97/romcompressor.go/internal/testfixture"
)

func TestCHDNRGRejectsAmbiguousExtents(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"partial sector", testfixture.NRG(make([]byte, 2049), 0, 0)},
		{"unassigned leading sector", testfixture.NRG(make([]byte, 4096), 2048, 2048)},
		{"truncated footer", []byte("NER5")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "disc.nrg")
			if err := os.WriteFile(source, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateNRG(context.Background(), source, testMaxOutput); err == nil {
				t.Fatal("accepted invalid NRG image")
			}
		})
	}
}
