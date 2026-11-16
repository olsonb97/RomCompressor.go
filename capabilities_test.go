package romcompressor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func capabilityFor(t *testing.T, values []FormatCapability, format Format) FormatCapability {
	t.Helper()
	for _, value := range values {
		if value.Format == format {
			return value
		}
	}
	t.Fatalf("missing capability for %s", format)
	return FormatCapability{}
}

func TestCapabilitiesDistinguishNativeOperations(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	values := Capabilities(Options{})
	for _, format := range []Format{ZIP, GZIP, CSO, ZSO, ECM, WBFS, CISO, CCI, WUX} {
		capability := capabilityFor(t, values, format)
		for _, operation := range []OperationCapability{capability.Compress, capability.Decompress, capability.Verify} {
			if operation.Engine != "native" || !operation.Available || operation.RequiresKeys {
				t.Errorf("%s unexpectedly needs an external tool: %+v", format, operation)
			}
		}
	}
	pbp := capabilityFor(t, values, PBP)
	if pbp.Available || pbp.Compress.Available || pbp.Compress.Engine != "psxpackager" ||
		!pbp.Decompress.Available || pbp.Decompress.Engine != "native" || !pbp.Verify.Available || pbp.Verify.Engine != "native" {
		t.Fatalf("PBP incorrectly requires PSXPackager for native operations: %+v", pbp)
	}
	wua := capabilityFor(t, values, WUA)
	if wua.Compress.Available || wua.Decompress.Available || !wua.Verify.Available || wua.Verify.Engine != "native" {
		t.Fatalf("WUA verification incorrectly needs ZArchive: %+v", wua)
	}
	for _, format := range []Format{ZCCI, ZCIA, ZCXI, Z3DSX} {
		capability := capabilityFor(t, values, format)
		if capability.Verify.Engine != "azahar" || capability.Verify.Available {
			t.Fatalf("%s verification must still need Azahar: %+v", format, capability.Verify)
		}
	}
	legacy := Formats()
	if len(legacy) != len(values) {
		t.Fatal("legacy Formats list changed")
	}
	for i := range legacy {
		if legacy[i] != values[i].FormatInfo {
			t.Fatalf("legacy summary differs at %d: %+v, %+v", i, legacy[i], values[i].FormatInfo)
		}
	}
}

func TestCapabilitiesUsesConfiguredPathsAndRefreshes(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	opts := Options{Tools: map[string]string{
		"azahar": filepath.Join(t.TempDir(), "missing-engine"),
		"nsz":    os.Args[0],
	}}
	before := Capabilities(opts)
	if capabilityFor(t, before, ZCCI).Compress.Available {
		t.Fatal("missing configured engine reported available")
	}
	for _, format := range []Format{NSZ, XCZ} {
		capability := capabilityFor(t, before, format)
		for _, operation := range []OperationCapability{capability.Compress, capability.Decompress, capability.Verify} {
			if !operation.Available || !operation.RequiresKeys {
				t.Fatalf("engine location and keys requirement must remain distinct: %+v", operation)
			}
		}
	}
	opts.Tools["azahar"] = os.Args[0]
	after := Capabilities(opts)
	for _, format := range []Format{ZCCI, ZCIA, ZCXI, Z3DSX} {
		capability := capabilityFor(t, after, format)
		if !capability.Compress.Available || !capability.Decompress.Available || !capability.Verify.Available {
			t.Fatalf("configured %s path was ignored: %+v", format, capability)
		}
	}
	if capabilityFor(t, before, ZCCI).Compress.Available {
		t.Fatal("refresh changed an earlier capability snapshot")
	}
	encoded, err := json.Marshal(capabilityFor(t, after, ZCCI))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"format", "engine", "description", "available", "compress", "decompress", "verify"} {
		if _, ok := document[key]; !ok {
			t.Errorf("missing compatibility/capability field %q", key)
		}
	}
}

func TestCapabilitiesLegacyWUXRequiresExplicitJAR(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	opts := Options{Tools: map[string]string{"java": filepath.Join(t.TempDir(), "missing-java")}}
	native := capabilityFor(t, Capabilities(opts), WUX)
	if native.Engine != "native" || !native.Compress.Available || !native.Decompress.Available {
		t.Fatal("a shared Java setting disabled native WUX")
	}
	opts.Tools["jwudtool"] = filepath.Join(t.TempDir(), "JWUDTool.jar")
	legacy := capabilityFor(t, Capabilities(opts), WUX)
	if legacy.Engine != "jwudtool" || legacy.Compress.Available || legacy.Decompress.Available || !legacy.Verify.Available {
		t.Fatalf("missing legacy prerequisites were not reported: %+v", legacy)
	}
	if err := os.WriteFile(opts.Tools["jwudtool"], nil, 0600); err != nil {
		t.Fatal(err)
	}
	opts.Tools["java"] = os.Args[0]
	legacy = capabilityFor(t, Capabilities(opts), WUX)
	if !legacy.Compress.Available || !legacy.Decompress.Available || legacy.Verify.Engine != "native" {
		t.Fatalf("legacy WUX lookup differs from native verification: %+v", legacy)
	}
}
