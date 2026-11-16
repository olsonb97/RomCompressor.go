package romcompressor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/olsonb97/romcompressor.go/internal/chd"
	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// ConversionPlan describes source-specific conversion choices. It is a read-only
// preflight, not payload verification or an emulator compatibility guarantee.
// Convert checks the source again and verifies its output before publication.
type ConversionPlan struct {
	Source    string             `json:"source"`
	Input     FileInfo           `json:"input"`
	Directory bool               `json:"directory"`
	Playlist  bool               `json:"playlist"`
	Targets   []ConversionTarget `json:"targets"`
}

// ConversionTarget is an eligible preflight choice. Format, Media and ExtractAll
// select the operation; Extension is the recommended destination suffix. Empty
// Extension means a caller-chosen filename or a directory. Directory describes
// the destination, while playlist targets create an M3U and companion folder.
// RequiresEntry means a ZIP member must be selected. NeedsArchiveListing means
// 7z entries must be listed with InspectWithOptions before choosing a member.
// Available only reports executable discovery; versions, runtimes, keys and full
// payload integrity are not established by a plan. Playlist byte checks are a
// known lower bound; names, companions and unknown sizes can exceed the limit
// during execution, which enforces the complete output and scratch budget.
type ConversionTarget struct {
	Format              Format                `json:"format"`
	Media               string                `json:"media,omitempty"`
	ExtractAll          bool                  `json:"extractAll,omitempty"`
	Extension           string                `json:"extension,omitempty"`
	Directory           bool                  `json:"directory,omitempty"`
	RequiresEntry       bool                  `json:"requiresEntry,omitempty"`
	NeedsArchiveListing bool                  `json:"needsArchiveListing,omitempty"`
	Dependencies        []OperationCapability `json:"dependencies"`
	Available           bool                  `json:"available"`
	RequiresKeys        bool                  `json:"requiresKeys"`
}

// PlanConversions reports choices that pass the source preflight shared with
// Convert. Only opts.Tools and opts.MaxOutput affect discovery. It reads bounded
// headers, indexes, descriptors and directory metadata. It does not decode or
// scan full game payloads, run engines, read keys, invoke progress callbacks,
// or create outputs or temporary files.
// The result may change if the source changes. An empty Targets slice means no
// supported operation passed preflight within the selected byte limit.
func PlanConversions(ctx context.Context, source string, opts Options) (ConversionPlan, error) {
	if err := ctx.Err(); err != nil {
		return ConversionPlan{}, err
	}
	if source == "" {
		return ConversionPlan{}, fmt.Errorf("source is required")
	}
	if err := validateTools(opts.Tools); err != nil {
		return ConversionPlan{}, err
	}
	if opts.MaxOutput < 0 {
		return ConversionPlan{}, fmt.Errorf("max output must be positive")
	}
	opts = Options{Tools: opts.Tools, MaxOutput: opts.MaxOutput}
	if opts.MaxOutput == 0 {
		opts.MaxOutput = defaultMaxOutput
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return ConversionPlan{}, err
	}
	stat, err := fileops.SnapshotStat(source, os.Lstat)
	if err != nil {
		return ConversionPlan{}, err
	}
	plan := ConversionPlan{Source: source, Directory: stat.IsDir(), Targets: []ConversionTarget{}}
	capabilities := make(map[Format]FormatCapability)
	for _, item := range Capabilities(opts) {
		capabilities[item.Format] = item
	}
	if stat.IsDir() {
		return planDirectory(ctx, plan, opts, capabilities)
	}
	stat, err = fileops.SnapshotStat(source, os.Stat)
	if err != nil {
		return ConversionPlan{}, err
	}
	if isPlaylist(source) {
		return planPlaylist(ctx, plan, opts, capabilities)
	}
	plan.Input, err = Inspect(ctx, source)
	if err != nil {
		return ConversionPlan{}, err
	}
	inputs, err := snapshotInputs(source)
	if err != nil {
		return ConversionPlan{}, err
	}
	if !fileops.SameSnapshot(stat, inputs[source]) {
		return ConversionPlan{}, fmt.Errorf("source changed during inspection")
	}
	if plan.Input.Format == WBFS {
		input, err := openWBFSInput(ctx, source)
		if err != nil {
			return ConversionPlan{}, err
		}
		input.addToSnapshot(inputs)
		input.close()
	}
	for _, candidate := range conversionCandidates(plan.Input, false) {
		choice := opts
		choice.Format, choice.Media = candidate.Format, candidate.Media
		if err := preflightSource(ctx, source, plan.Input, choice); err != nil {
			continue
		}
		candidate.Dependencies = []OperationCapability{conversionDependency(plan.Input.Format, candidate.Format, capabilities)}
		candidate.Extension = conversionExtension(plan.Input.Format, candidate.Format, candidate.Media)
		if candidate.Format == Raw && plan.Input.Format == ZIP {
			selected, all, err := planZIP(ctx, source, opts.MaxOutput)
			if err != nil {
				return ConversionPlan{}, err
			}
			if selected {
				candidate.RequiresEntry = len(plan.Input.Entries) > 1
				plan.Targets = append(plan.Targets, finishTarget(candidate))
			}
			if all {
				candidate.ExtractAll, candidate.Directory, candidate.RequiresEntry = true, true, false
				plan.Targets = append(plan.Targets, finishTarget(candidate))
			}
			continue
		}
		if candidate.Format == Raw && plan.Input.Format == SevenZip {
			candidate.NeedsArchiveListing = true
		}
		if candidate.Format == Raw && plan.Input.Format == WUA {
			candidate.Directory = true
		}
		plan.Targets = append(plan.Targets, finishTarget(candidate))
	}
	if err := checkInputs(inputs); err != nil {
		return ConversionPlan{}, err
	}
	return plan, ctx.Err()
}

func conversionCandidates(info FileInfo, playlist bool) []ConversionTarget {
	var result []ConversionTarget
	formats := append([]Format{Raw}, supportedPlanFormats()...)
	for _, format := range formats {
		if playlist {
			switch format {
			case Raw, CSO, ZSO, CHD, PBP, RVZ, WIA, GCZ, WBFS, CISO:
			default:
				continue
			}
		}
		if format == CHD || format == Raw && info.Format == CHD {
			for _, media := range []string{"cd", "dvd", "hdd"} {
				if playlist && media == "hdd" {
					continue
				}
				result = append(result, ConversionTarget{Format: format, Media: media})
			}
		} else {
			result = append(result, ConversionTarget{Format: format})
		}
	}
	return result
}

func supportedPlanFormats() []Format {
	formats := make([]Format, 0, len(formatDescriptors))
	for _, item := range formatDescriptors {
		formats = append(formats, item.Format)
	}
	return formats
}

func conversionDependency(input, output Format, capabilities map[Format]FormatCapability) OperationCapability {
	if output == Raw {
		return capabilities[input].Decompress
	}
	return capabilities[output].Compress
}

func finishTarget(target ConversionTarget) ConversionTarget {
	dependencies := make(map[string]OperationCapability)
	for _, dependency := range target.Dependencies {
		if existing, ok := dependencies[dependency.Engine]; ok {
			dependency.Available = dependency.Available && existing.Available
			dependency.RequiresKeys = dependency.RequiresKeys || existing.RequiresKeys
		}
		dependencies[dependency.Engine] = dependency
	}
	target.Dependencies = make([]OperationCapability, 0, len(dependencies))
	for _, dependency := range dependencies {
		target.Dependencies = append(target.Dependencies, dependency)
	}
	sort.Slice(target.Dependencies, func(i, j int) bool { return target.Dependencies[i].Engine < target.Dependencies[j].Engine })
	target.Available = true
	for _, dependency := range target.Dependencies {
		target.Available = target.Available && dependency.Available
		target.RequiresKeys = target.RequiresKeys || dependency.RequiresKeys
	}
	return target
}

func conversionExtension(input, output Format, media string) string {
	if output == GZIP {
		return ".gz"
	}
	if output != Raw {
		return "." + string(output)
	}
	switch input {
	case CSO, ZSO, RVZ, WIA, GCZ, WBFS, CISO, CCI:
		return ".iso"
	case PBP:
		return ".cue"
	case WUX:
		return ".wud"
	case NSZ:
		return ".nsp"
	case XCZ:
		return ".xci"
	case ZCCI:
		return ".cci"
	case ZCIA:
		return ".cia"
	case ZCXI:
		return ".cxi"
	case Z3DSX:
		return ".3dsx"
	case CHD:
		return map[string]string{"cd": ".cue", "dvd": ".iso", "hdd": ".img"}[media]
	}
	return ""
}

func planZIP(ctx context.Context, source string, limit int64) (bool, bool, error) {
	file, stat, err := fileops.OpenRegular(source)
	if err != nil {
		return false, false, err
	}
	defer file.Close()
	archive, err := codec.OpenZIP(ctx, file, stat.Size())
	if err != nil {
		return false, false, err
	}
	selected := false
	for index, member := range archive.File {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		if !member.FileInfo().IsDir() && member.UncompressedSize64 <= uint64(limit) && archive.ValidateMember(index) == nil {
			selected = true
		}
	}
	_, _, _, treeErr := readZIPTree(ctx, file, stat.Size(), limit)
	return selected, treeErr == nil, ctx.Err()
}

func planDirectory(ctx context.Context, plan ConversionPlan, opts Options, capabilities map[Format]FormatCapability) (ConversionPlan, error) {
	layout := newZIPTreeLayout()
	inputs := make(map[string]os.FileInfo)
	var total int64
	err := visitZIPTree(ctx, plan.Source, "", func(name string, info os.FileInfo) error {
		inputs[filepath.Join(plan.Source, filepath.FromSlash(name))] = info
		if name == "" {
			return nil
		}
		if err := layout.declare(name, info.IsDir()); err != nil {
			return err
		}
		if !info.IsDir() {
			if info.Size() > opts.MaxOutput-total {
				return fmt.Errorf("ZIP folder exceeds expanded size limit")
			}
			total += info.Size()
		}
		return nil
	})
	if err == nil {
		if err := checkInputs(inputs); err != nil {
			return ConversionPlan{}, err
		}
		plan.Targets = append(plan.Targets, finishTarget(ConversionTarget{Format: ZIP, Extension: ".zip", Dependencies: []OperationCapability{capabilities[ZIP].Compress}}))
	}
	if _, inputs, _, err := prepareWUATitleTree(ctx, plan.Source, opts.MaxOutput); err == nil {
		if err := checkInputs(inputs); err != nil {
			return ConversionPlan{}, err
		}
		plan.Targets = append(plan.Targets, finishTarget(ConversionTarget{Format: WUA, Extension: ".wua", Dependencies: []OperationCapability{capabilities[WUA].Compress}}))
	}
	plan.Input = FileInfo{Format: Raw, Size: total}
	return plan, ctx.Err()
}

func planPlaylist(ctx context.Context, plan ConversionPlan, opts Options, capabilities map[Format]FormatCapability) (ConversionPlan, error) {
	discs, before, err := readPlaylist(ctx, plan.Source)
	if err != nil {
		return ConversionPlan{}, err
	}
	plan.Playlist, plan.Input = true, FileInfo{Format: Raw, Size: before.Size()}
	inputs := map[string]os.FileInfo{plan.Source: before}
	infos := make([]FileInfo, len(discs))
	for i, disc := range discs {
		infos[i], err = Inspect(ctx, disc.path)
		if err != nil {
			return ConversionPlan{}, fmt.Errorf("inspect playlist disc %d: %w", i+1, err)
		}
		snapshots, err := snapshotInputs(disc.path)
		if err != nil {
			return ConversionPlan{}, err
		}
		if err := rememberPlaylistInputs(inputs, snapshots); err != nil {
			return ConversionPlan{}, err
		}
		if infos[i].Format == WBFS {
			input, err := openWBFSInput(ctx, disc.path)
			if err != nil {
				return ConversionPlan{}, err
			}
			input.addToSnapshot(inputs)
			input.close()
		}
	}
	candidates := conversionCandidates(FileInfo{}, true)
	// The default raw choice uses each CHD's recorded media independently.
	// Explicit choices additionally serve untagged CD/DVD sets.
	candidates = append(candidates, ConversionTarget{Format: Raw, Media: "cd"}, ConversionTarget{Format: Raw, Media: "dvd"}, ConversionTarget{Format: CHD})
	autoRaw := false
	explicitCHD := false
	for _, candidate := range candidates {
		if candidate.Format == Raw && candidate.Media != "" && autoRaw {
			continue
		}
		if candidate.Format == CHD && candidate.Media == "" && explicitCHD {
			continue
		}
		valid := true
		// Even before its destination name is known, a playlist needs at least
		// one newline per reference. Unknown compressed sizes are not estimated.
		knownBytes := int64(len(discs))
		for i, disc := range discs {
			choice := opts
			choice.Format, choice.Media = candidate.Format, candidate.Media
			if candidate.Format == Raw {
				choice, _, err = playlistRestoreOptions(disc.path, infos[i], choice)
				if err != nil {
					valid = false
					break
				}
			}
			var dependency OperationCapability
			minimumBytes := int64(0)
			if infos[i].Format == candidate.Format {
				minimumBytes = infos[i].Size
				if infos[i].Size > opts.MaxOutput {
					valid = false
					break
				}
				if native(candidate.Format) {
					if _, err := preflightNativeInput(ctx, disc.path, infos[i], choice); err != nil {
						valid = false
						break
					}
				}
				if candidate.Format == CHD {
					if _, err := chd.MetadataFile(disc.path, opts.MaxOutput, choice.Media); err != nil {
						valid = false
						break
					}
				}
				if candidate.Format == Raw {
					dependency = OperationCapability{Engine: "native", Available: true}
				} else {
					dependency = capabilities[candidate.Format].Verify
				}
			} else {
				if err := preflightSource(ctx, disc.path, infos[i], choice); err != nil {
					valid = false
					break
				}
				dependency = conversionDependency(infos[i].Format, candidate.Format, capabilities)
				if candidate.Format == Raw && (native(infos[i].Format) || infos[i].Format == PBP) {
					minimumBytes, err = preflightNativeInput(ctx, disc.path, infos[i], choice)
					if err != nil {
						valid = false
						break
					}
					if minimumBytes < 0 {
						minimumBytes = 0
					}
				}
			}
			if minimumBytes > opts.MaxOutput-knownBytes {
				valid = false
				break
			}
			knownBytes += minimumBytes
			candidate.Dependencies = append(candidate.Dependencies, dependency)
		}
		if valid {
			if candidate.Format == Raw && candidate.Media == "" {
				autoRaw = true
			}
			if candidate.Format == CHD && candidate.Media != "" {
				explicitCHD = true
			}
			candidate.Extension = ".m3u"
			plan.Targets = append(plan.Targets, finishTarget(candidate))
		}
	}
	if err := checkInputs(inputs); err != nil {
		return ConversionPlan{}, err
	}
	return plan, ctx.Err()
}
