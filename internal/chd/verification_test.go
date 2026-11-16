package chd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/olsonb97/romcompressor.go/internal/process"
)

func TestCHDVerificationRequiresApplicableChecksums(t *testing.T) {
	const raw = "Raw SHA1 verification successful!\n"
	const overall = "Overall SHA1 verification successful!\n"
	for _, tc := range []struct {
		name    string
		version uint32
		log     string
		ok      bool
	}{
		{"legacy", 3, raw, true},
		{"modern", 5, "Verifying, 100.0% complete...\r" + raw + overall, true},
		{"skipped", 5, "No verification to be done; CHD has no checksum", false},
		{"raw mismatch", 5, "Error: Raw SHA1 in header = bad\n", false},
		{"metadata mismatch", 4, raw + "Error: Overall SHA1 in header = bad\n", false},
		{"no overall confirmation", 5, raw, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkCHDVerification(tc.version, tc.log); (err == nil) != tc.ok {
				t.Fatalf("verification result: %v, want success %t", err, tc.ok)
			}
		})
	}
}

func TestCHDVerificationDiagnosticHelper(t *testing.T) {
	if os.Getenv("ROMCOMPRESSOR_TEST_CHD_DIAGNOSTICS") != "1" {
		return
	}
	_, _ = os.Stdout.WriteString("engine log begins\n" + strings.Repeat("x", 3<<20) + "\nNo verification to be done; CHD has no checksum\n")
	os.Exit(0)
}

func TestCHDVerificationFailureDiagnosticsAreBounded(t *testing.T) {
	t.Setenv("ROMCOMPRESSOR_TEST_CHD_DIAGNOSTICS", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log, err := process.Run(ctx, os.Args[0], "-test.run=^TestCHDVerificationDiagnosticHelper$")
	if err != nil || len(log) < 3<<20 {
		t.Fatalf("expected successful engine with large captured log, got %d bytes, %v", len(log), err)
	}
	err = checkCHDVerification(5, log)
	if err == nil {
		t.Fatal("accepted a successful engine exit without checksum confirmation")
	}
	if len(err.Error()) > (16<<10)+200 || !strings.Contains(err.Error(), "diagnostics truncated; last 16 KiB") {
		t.Fatalf("unbounded or silently truncated diagnostics: %d bytes", len(err.Error()))
	}
	if strings.Contains(err.Error(), "engine log begins") || !strings.HasSuffix(err.Error(), "No verification to be done; CHD has no checksum") {
		t.Fatal("failure diagnostics did not retain the final engine diagnosis")
	}
}
