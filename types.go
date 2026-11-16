// Package romcompressor converts game images through native streaming codecs
// and explicitly configured external format engines.
package romcompressor

import (
	"context"

	kind "github.com/olsonb97/romcompressor.go/internal/format"
)

// Format identifies a container or raw, uncompressed output.
type Format string

const (
	Raw      Format = kind.Raw
	ZIP      Format = kind.ZIP
	GZIP     Format = kind.GZIP
	CSO      Format = kind.CSO
	ZSO      Format = kind.ZSO
	ECM      Format = kind.ECM
	CHD      Format = kind.CHD
	PBP      Format = kind.PBP
	RVZ      Format = kind.RVZ
	WIA      Format = kind.WIA
	GCZ      Format = kind.GCZ
	WBFS     Format = kind.WBFS
	CISO     Format = kind.CISO
	CCI      Format = kind.CCI
	NSZ      Format = kind.NSZ
	XCZ      Format = kind.XCZ
	SevenZip Format = kind.SevenZip
	WUX      Format = kind.WUX
	WUA      Format = kind.WUA
	ZCCI     Format = kind.ZCCI
	ZCIA     Format = kind.ZCIA
	ZCXI     Format = kind.ZCXI
	Z3DSX    Format = kind.Z3DSX
)

// Options configures one conversion. Sources and existing destinations are preserved.
type Options struct {
	Format Format `json:"format"`
	// Entry selects a member for ZIP or 7z extraction, or ZIP verification.
	Entry string `json:"entry,omitempty"`
	// ExtractAll restores the complete ZIP tree to a new directory.
	ExtractAll bool   `json:"extractAll,omitempty"`
	Media      string `json:"media,omitempty"`
	Keys       string `json:"keys,omitempty"`
	// MaxOutput bounds native expanded bytes, final output, and monitored external
	// staging including verification scratch files. Zero uses 1 TiB.
	MaxOutput int64 `json:"maxOutput,omitempty"`
	// TempDir selects an existing parent directory for standalone verification
	// scratch files. Empty uses the operating system's temporary directory.
	// Convert and ConvertPlaylist do not accept this verification-only option.
	TempDir string `json:"-"`
	// Tools overrides engine locations. Keys are chdman, dolphin-tool, nsz, 7z,
	// azahar, psxpackager, zarchive, java, and jwudtool. JWUDTool names a JAR.
	Tools map[string]string `json:"-"`
	// Progress is invoked synchronously. Callers must return promptly.
	Progress func(Progress) `json:"-"`
}

// Progress reports the current phase and byte counts when known. Total zero means unknown.
type Progress struct {
	Phase string `json:"phase"`
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
}

// Result describes a successfully published output and its format verification.
type Result struct {
	Output      string `json:"output"`
	Format      Format `json:"format"`
	InputBytes  int64  `json:"inputBytes"`
	OutputBytes int64  `json:"outputBytes"`
	Verified    bool   `json:"verified"`
}

// FileInfo describes a signature-detected file and optional archive members.
type FileInfo struct {
	Format Format `json:"format"`
	Size   int64  `json:"size"`
	// Media reports explicit CHD metadata (cd, dvd, hdd, or laserdisc).
	// An empty value means the container does not identify its media type.
	Media   string  `json:"media,omitempty"`
	Entries []Entry `json:"entries,omitempty"`
}

// Entry describes a regular archive member and its expanded size.
type Entry struct {
	Name string `json:"name"`
	Size uint64 `json:"size"`
}

// Job describes a conversion for a sequential batch. Each successful output is committed independently.
type Job struct {
	Source string `json:"source"`
	Output string `json:"output"`
	Options
}

// JobResult records either the published output or the failure for an attempted job.
type JobResult struct {
	Source string  `json:"source"`
	Result *Result `json:"result,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// Batch runs jobs in order and keeps per-job failures. Cancellation stops new work.
func Batch(ctx context.Context, jobs []Job) []JobResult {
	results := make([]JobResult, 0, len(jobs))
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		result, err := Convert(ctx, job.Source, job.Output, job.Options)
		item := JobResult{Source: job.Source}
		if err != nil {
			item.Error = err.Error()
		} else {
			item.Result = &result
		}
		results = append(results, item)
	}
	return results
}
