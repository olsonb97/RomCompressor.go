package romcompressor

import (
	"os"

	"github.com/olsonb97/romcompressor.go/internal/process"
)

// OperationCapability describes an operation's external dependency.
// Available means the executable can be located, not that its version, keys,
// or a particular input have been validated. Engine is "native" when no tool is needed.
type OperationCapability struct {
	Engine       string `json:"engine"`
	Available    bool   `json:"available"`
	RequiresKeys bool   `json:"requiresKeys"`
}

// FormatCapability reports creation, restoration, and verification separately.
// Its embedded FormatInfo retains the creation-engine summary used by Formats.
type FormatCapability struct {
	FormatInfo
	Compress   OperationCapability `json:"compress"`
	Decompress OperationCapability `json:"decompress"`
	Verify     OperationCapability `json:"verify"`
}

// Capabilities reports operation-specific dependencies using opts.Tools overrides.
// It does not run engines, load keys, or validate an input. RequiresKeys is separate
// from executable availability; other Options fields do not affect this query.
// Only an explicit jwudtool path selects the optional legacy WUX adapter.
func Capabilities(opts Options) []FormatCapability {
	available := make(map[string]bool)
	locate := func(engine string) bool {
		if engine == "native" {
			return true
		}
		if found, checked := available[engine]; checked {
			return found
		}
		_, err := process.Resolve(opts.Tools, engine)
		available[engine] = err == nil
		return err == nil
	}
	legacyWUX := opts.Tools["jwudtool"] != ""
	legacyAvailable := false
	if legacyWUX {
		jar, err := os.Stat(opts.Tools["jwudtool"])
		legacyAvailable = err == nil && jar.Mode().IsRegular() && locate("java")
	}
	formats := make([]FormatCapability, 0, len(formatDescriptors))
	for _, descriptor := range formatDescriptors {
		operation := OperationCapability{
			Engine:       descriptor.Engine,
			Available:    locate(descriptor.Engine),
			RequiresKeys: descriptor.Format == NSZ || descriptor.Format == XCZ,
		}
		capability := FormatCapability{FormatInfo: descriptor, Compress: operation, Decompress: operation, Verify: operation}
		switch descriptor.Format {
		case PBP:
			capability.Decompress = OperationCapability{Engine: "native", Available: true}
			capability.Verify = capability.Decompress
		case WUA:
			capability.Verify = OperationCapability{Engine: "native", Available: true}
		case WUX:
			if legacyWUX {
				capability.Compress = OperationCapability{Engine: "jwudtool", Available: legacyAvailable}
				capability.Decompress = capability.Compress
			}
		}
		capability.Engine = capability.Compress.Engine
		capability.Available = capability.Compress.Available
		formats = append(formats, capability)
	}
	return formats
}
