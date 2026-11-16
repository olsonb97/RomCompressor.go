package romcompressor

import (
	"fmt"

	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

// FormatInfo describes a supported output format and its engine requirement.
type FormatInfo struct {
	Format      Format `json:"format"`
	Engine      string `json:"engine"`
	Description string `json:"description"`
	Available   bool   `json:"available"`
}

var formatDescriptors = []FormatInfo{
	{ZIP, "native", "File and folder ZIP compression; selected-entry or full-tree extraction", true},
	{GZIP, "native", "Single-file gzip compression", true},
	{CSO, "native", "PSP and PS2 compressed ISO (CSO v1)", true},
	{ZSO, "native", "LZ4 compressed ISO (ZSO v1)", true},
	{ECM, "native", "Lossless CD sector modeling for individual disc image files", true},
	{CHD, "chdman", "CD, DVD, and hard-drive images, including CUE/BIN and GDI sets", false},
	{PBP, "psxpackager", "Single-disc PlayStation CUE/BIN packaging", false},
	{RVZ, "dolphin-tool", "GameCube and Wii disc images", false},
	{WIA, "dolphin-tool", "GameCube and Wii disc images", false},
	{GCZ, "dolphin-tool", "GameCube and Wii disc images", false},
	{WBFS, "native", "Single-disc Wii ISO compression by omitting all-zero blocks", true},
	{CISO, "native", "GameCube and Wii Compact ISO compression by omitting all-zero blocks", true},
	{CCI, "native", "Single-part Original Xbox compressed XISO", true},
	{NSZ, "nsz", "Switch NSP compression; requires user-provided keys", false},
	{XCZ, "nsz", "Switch XCI compression; requires user-provided keys", false},
	{SevenZip, "7z", "Single-file 7z compression and selected-entry extraction", false},
	{WUX, "native", "Wii U WUD compression by deduplicating identical sectors", true},
	{WUA, "zarchive", "Decrypted Wii U title directories, including updates and DLC", false},
	{ZCCI, "azahar", "Decrypted Nintendo 3DS cartridge images", false},
	{ZCIA, "azahar", "Decrypted Nintendo 3DS installable archives", false},
	{ZCXI, "azahar", "Decrypted Nintendo 3DS executable containers", false},
	{Z3DSX, "azahar", "Nintendo 3DS homebrew", false},
}

// Formats reports formats and whether their default creation engine is on PATH.
// Restoration and verification may be native even when Available is false.
// Use Capabilities for per-operation availability and explicit Options.Tools paths.
func Formats() []FormatInfo {
	capabilities := Capabilities(Options{})
	formats := make([]FormatInfo, 0, len(capabilities))
	for _, capability := range capabilities {
		formats = append(formats, capability.FormatInfo)
	}
	return formats
}

func native(format Format) bool { return kind.Native(string(format)) }

func known(format Format) bool {
	if format == Raw {
		return true
	}
	for _, item := range formatDescriptors {
		if item.Format == format {
			return true
		}
	}
	return false
}

func dolphinFormat(format Format) bool { return kind.Dolphin(string(format)) }

func threeDSFormat(format Format) bool { return kind.ThreeDS(string(format)) }

func validateTools(tools map[string]string) error {
	for tool := range tools {
		switch tool {
		case "chdman", "dolphin-tool", "nsz", "7z", "java", "jwudtool", "azahar", "psxpackager", "zarchive":
		default:
			return fmt.Errorf("unknown tool %q", tool)
		}
	}
	return nil
}
