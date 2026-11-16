// Package format owns container names and format-family classification.
package format

const (
	Raw      = "raw"
	ZIP      = "zip"
	GZIP     = "gzip"
	CSO      = "cso"
	ZSO      = "zso"
	ECM      = "ecm"
	CHD      = "chd"
	PBP      = "pbp"
	RVZ      = "rvz"
	WIA      = "wia"
	GCZ      = "gcz"
	WBFS     = "wbfs"
	CISO     = "ciso"
	CCI      = "cci"
	NSZ      = "nsz"
	XCZ      = "xcz"
	SevenZip = "7z"
	WUX      = "wux"
	WUA      = "wua"
	ZCCI     = "zcci"
	ZCIA     = "zcia"
	ZCXI     = "zcxi"
	Z3DSX    = "z3dsx"
)

// DefaultMaxOutput is the default byte budget for conversion and verification.
const DefaultMaxOutput int64 = 1 << 40

// Native reports formats encoded and decoded without an external engine.
func Native(name string) bool {
	switch name {
	case ZIP, GZIP, CSO, ZSO, ECM, WBFS, CISO, CCI, WUX:
		return true
	}
	return false
}

// Dolphin reports formats handled by Dolphin Tool.
func Dolphin(name string) bool { return name == RVZ || name == WIA || name == GCZ }

// ThreeDS reports the Azahar compressed container family.
func ThreeDS(name string) bool { return name == ZCCI || name == ZCIA || name == ZCXI || name == Z3DSX }

// ThreeDSRawExtension returns the canonical restored Azahar content extension.
func ThreeDSRawExtension(name string) string {
	switch name {
	case ZCCI:
		return ".cci"
	case ZCIA:
		return ".cia"
	case ZCXI:
		return ".cxi"
	case Z3DSX:
		return ".3dsx"
	}
	return ""
}
