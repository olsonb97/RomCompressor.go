package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unicode"

	romcompressor "github.com/olsonb97/romcompressor.go"
)

var version = "dev"

func currentVersion() string {
	info, _ := debug.ReadBuildInfo()
	return buildVersion(version, info)
}

func buildVersion(stamped string, info *debug.BuildInfo) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	// Installed modules have a checksum. Local VCS builds can also carry an
	// inferred tag (and +dirty), but should retain the development label.
	if info != nil && info.Main.Sum != "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	output := &checkedOutput{writer: stdout}
	args, err := parse(argv)
	if err == nil {
		if args.help {
			writeHelp(output, args.command)
		} else {
			err = execute(ctx, args, output, stderr)
		}
	}
	if output.err != nil {
		fmt.Fprintln(stderr, "Error: write output:", output.err)
		return 1
	}
	if err == nil {
		return 0
	}
	var reported *reportedError
	if !errors.As(err, &reported) {
		if args.json {
			if writeErr := json.NewEncoder(output).Encode(struct {
				Error string `json:"error"`
			}{err.Error()}); writeErr != nil {
				fmt.Fprintln(stderr, "Error: write output:", writeErr)
			}
		} else {
			fmt.Fprintln(stderr, "Error:", err)
		}
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}

// Record the first output failure even when a human-output formatter ignores it.
// Stop subsequent writes so a partial JSON response cannot gain a second document.
type checkedOutput struct {
	writer io.Writer
	err    error
}

func (w *checkedOutput) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func execute(ctx context.Context, a arguments, stdout, stderr io.Writer) error {
	switch a.command {
	case "version":
		if a.json {
			return json.NewEncoder(stdout).Encode(map[string]string{"version": currentVersion()})
		}
		_, err := fmt.Fprintln(stdout, "romcompressor", currentVersion())
		return err
	case "formats":
		formats := romcompressor.Capabilities(options(a))
		if a.json {
			return json.NewEncoder(stdout).Encode(formats)
		}
		fmt.Fprintf(stdout, "%-6s %-23s %-23s %s\n", "FORMAT", "COMPRESS", "DECOMPRESS", "VERIFY")
		for _, format := range formats {
			fmt.Fprintf(stdout, "%-6s %-23s %-23s %s\n", format.Format,
				capabilityStatus(format.Compress), capabilityStatus(format.Decompress), capabilityStatus(format.Verify))
			fmt.Fprintf(stdout, "  %s\n", format.Description)
		}
		fmt.Fprintln(stdout, "\nExternal engines are installed separately. Availability checks paths only; versions, keys, and game layouts are not validated.")
		return nil
	case "inspect":
		info, err := romcompressor.InspectWithOptions(ctx, a.paths[0], options(a))
		if err != nil {
			return err
		}
		if a.json {
			return json.NewEncoder(stdout).Encode(info)
		}
		fmt.Fprintf(stdout, "%s · %d bytes\n", info.Format, info.Size)
		if info.Format == romcompressor.CHD {
			switch info.Media {
			case "cd", "dvd", "hdd":
				fmt.Fprintf(stdout, "Media: %s (restore with --media %s)\n", info.Media, info.Media)
			case "":
				fmt.Fprintln(stdout, "Media: unidentified; choose --media cd, dvd, or hdd from the source's known layout")
			default:
				fmt.Fprintf(stdout, "Media: %s (restoration is unsupported)\n", info.Media)
			}
		}
		for _, entry := range info.Entries {
			fmt.Fprintf(stdout, "%q · %d bytes\n", entry.Name, entry.Size)
		}
		return nil
	case "verify":
		if err := romcompressor.Verify(ctx, a.paths[0], options(a)); err != nil {
			return err
		}
		if a.json {
			return json.NewEncoder(stdout).Encode(map[string]any{"verified": true, "path": a.paths[0]})
		}
		_, err := fmt.Fprintln(stdout, "Verified:", a.paths[0])
		return err
	case "batch":
		return executeBatch(ctx, a, stdout, stderr)
	case "plan":
		return executePlan(ctx, a, stdout)
	case "trim":
		return executeTrim(ctx, a, stdout, stderr)
	default:
		return executeConversion(ctx, a, stdout, stderr)
	}
}

func capabilityStatus(operation romcompressor.OperationCapability) string {
	status := "missing " + operation.Engine
	if operation.Engine == "native" && operation.Available {
		status = "built in"
	} else if operation.Available {
		status = "found " + operation.Engine
	}
	if operation.RequiresKeys {
		status += " + keys"
	}
	return status
}

func options(a arguments) romcompressor.Options {
	return romcompressor.Options{
		Format: romcompressor.Format(a.format), Entry: a.entry, ExtractAll: a.seen["all"],
		Media: a.media, Keys: a.keys, MaxOutput: a.maxOutput, Tools: a.tools,
		TempDir: a.tempDir,
	}
}

func executeTrim(ctx context.Context, a arguments, stdout, stderr io.Writer) error {
	source := a.paths[0]
	if a.seen["dry-run"] {
		var opts romcompressor.TrimOptions
		if !a.json {
			opts.Progress = progressOutput(stderr)
		}
		plan, err := romcompressor.PlanTrimWithOptions(ctx, source, opts)
		if err != nil {
			return err
		}
		if a.json {
			return json.NewEncoder(stdout).Encode(plan)
		}
		_, err = fmt.Fprintf(stdout, "%s · %d → %d bytes (%d removable)\n",
			plan.Format, plan.InputBytes, plan.OutputBytes, plan.InputBytes-plan.OutputBytes)
		return err
	}
	output := a.output
	if output == "" {
		ext := filepath.Ext(source)
		output = strings.TrimSuffix(source, ext) + ".trim" + ext
	}
	var opts romcompressor.TrimOptions
	if !a.json {
		opts.Progress = progressOutput(stderr)
	}
	result, err := romcompressor.Trim(ctx, source, output, opts)
	if err != nil {
		return outputError(err)
	}
	if a.json {
		return json.NewEncoder(stdout).Encode(result)
	}
	_, err = fmt.Fprintf(stdout, "%s\n%d → %d bytes\n", result.Output, result.InputBytes, result.OutputBytes)
	return err
}

func executeConversion(ctx context.Context, a arguments, stdout, stderr io.Writer) error {
	opts := options(a)
	if !a.json {
		opts.Progress = progressOutput(stderr)
	}
	if a.command == "decompress" {
		opts.Format = romcompressor.Raw
	}
	output := a.output
	if a.command == "decompress" && playlistFile(a.paths[0]) {
		if output == "" {
			source := filepath.Clean(a.paths[0])
			output = strings.TrimSuffix(source, filepath.Ext(source)) + "-restored.m3u"
		}
	} else if a.command == "decompress" {
		info, err := romcompressor.InspectWithOptions(ctx, a.paths[0], opts)
		if err != nil {
			return err
		}
		if err := archiveSelectionError(info, opts); err != nil {
			return err
		}
		if output == "" {
			output, err = decompressedOutput(a.paths[0], opts, info)
			if err != nil {
				return err
			}
		}
	} else if output == "" {
		var err error
		output, err = defaultOutput(ctx, a.paths[0], opts)
		if err != nil {
			return err
		}
	}
	result, err := romcompressor.Convert(ctx, a.paths[0], output, opts)
	if err != nil {
		return outputError(err)
	}
	if a.json {
		return json.NewEncoder(stdout).Encode(result)
	}
	_, err = fmt.Fprintf(stdout, "%s\n%d → %d bytes\n", result.Output, result.InputBytes, result.OutputBytes)
	return err
}

func outputError(err error) error {
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w; choose a different output path with -o/--output", err)
	}
	return err
}

func defaultOutput(ctx context.Context, source string, opts romcompressor.Options) (string, error) {
	if opts.Format != romcompressor.Raw {
		source = filepath.Clean(source)
		playlist := playlistFile(source)
		// Derive a sibling name from the directory itself, including when the
		// selected spelling ends in a separator or names the working directory.
		if source == "." || source == ".." {
			var err error
			source, err = filepath.Abs(source)
			if err != nil {
				return "", err
			}
		}
		if playlist {
			return strings.TrimSuffix(source, filepath.Ext(source)) + "-" + string(opts.Format) + ".m3u", nil
		}
		extension := string(opts.Format)
		if opts.Format == romcompressor.GZIP {
			extension = "gz"
		}
		if opts.Format == romcompressor.ZIP || opts.Format == romcompressor.GZIP || opts.Format == romcompressor.SevenZip || opts.Format == romcompressor.WUA {
			return source + "." + extension, nil
		}
		return strings.TrimSuffix(source, filepath.Ext(source)) + "." + extension, nil
	}
	info, err := romcompressor.InspectWithOptions(ctx, source, opts)
	if err != nil {
		return "", err
	}
	return decompressedOutput(source, opts, info)
}

func playlistFile(source string) bool {
	source = filepath.Clean(source)
	ext := filepath.Ext(source)
	if !strings.EqualFold(ext, ".m3u") && !strings.EqualFold(ext, ".m3u8") {
		return false
	}
	info, err := os.Lstat(source)
	return err != nil || !info.IsDir()
}

func archiveSelectionError(info romcompressor.FileInfo, opts romcompressor.Options) error {
	if opts.Entry != "" || opts.ExtractAll || (info.Format != romcompressor.ZIP && info.Format != romcompressor.SevenZip) || len(info.Entries) == 1 {
		return nil
	}
	if info.Format == romcompressor.ZIP {
		return fmt.Errorf("ZIP contains %d files; select one with --entry NAME, or restore the whole folder with --all -o DIRECTORY", len(info.Entries))
	}
	if len(info.Entries) == 0 {
		return errors.New("7z archive contains no regular files to extract")
	}
	return fmt.Errorf("7z archive contains %d files; select one with --entry NAME (use inspect to list entries)", len(info.Entries))
}

func decompressedOutput(source string, opts romcompressor.Options, info romcompressor.FileInfo) (string, error) {
	if err := archiveSelectionError(info, opts); err != nil {
		return "", err
	}
	base := strings.TrimSuffix(source, filepath.Ext(source))
	switch info.Format {
	case romcompressor.CSO, romcompressor.ZSO, romcompressor.RVZ, romcompressor.WIA, romcompressor.GCZ, romcompressor.WBFS, romcompressor.CISO, romcompressor.CCI:
		return base + ".iso", nil
	case romcompressor.CHD:
		switch opts.Media {
		case "cd":
			return base + ".cue", nil
		case "dvd":
			return base + ".iso", nil
		case "hdd":
			return base + ".img", nil
		default:
			return "", errors.New("choose --media cd, dvd, or hdd for CHD extraction; use inspect to see tagged media")
		}
	case romcompressor.PBP:
		return base + ".cue", nil
	case romcompressor.ECM:
		switch strings.ToLower(filepath.Ext(base)) {
		case ".bin", ".img", ".iso", ".cdi", ".nrg":
			return base, nil
		}
		return base + ".bin", nil
	case romcompressor.NSZ:
		return base + ".nsp", nil
	case romcompressor.XCZ:
		return base + ".xci", nil
	case romcompressor.WUX:
		return base + ".wud", nil
	case romcompressor.WUA:
		return base + "-extracted", nil
	case romcompressor.ZCCI:
		return base + ".cci", nil
	case romcompressor.ZCIA:
		return base + ".cia", nil
	case romcompressor.ZCXI:
		return base + ".cxi", nil
	case romcompressor.Z3DSX:
		return base + ".3dsx", nil
	case romcompressor.GZIP:
		if strings.EqualFold(filepath.Ext(source), ".gz") && filepath.Ext(base) != "" {
			return base, nil
		}
	case romcompressor.ZIP, romcompressor.SevenZip:
		entry := opts.Entry
		if entry == "" && len(info.Entries) == 1 {
			entry = info.Entries[0].Name
		}
		if entry != "" {
			name := filepath.Base(strings.ReplaceAll(entry, "\\", "/"))
			if name != "" && name != "." && name != ".." {
				if strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' }) >= 0 {
					return "", errors.New("archive entry name contains control or line-break characters; choose a safe output path with -o")
				}
				return filepath.Join(filepath.Dir(source), name), nil
			}
		}
	}
	return "", errors.New("choose the decompressed output path with -o; use inspect to see archive entries")
}

func progressOutput(w io.Writer) func(romcompressor.Progress) {
	var phase string
	var last time.Time
	return func(progress romcompressor.Progress) {
		if progress.Phase == "complete" {
			return
		}
		if phase == progress.Phase && time.Since(last) < 5*time.Second {
			return
		}
		phase, last = progress.Phase, time.Now()
		if progress.Total > 0 && progress.Bytes > 0 {
			fmt.Fprintf(w, "%s %.0f%%\n", progress.Phase, float64(progress.Bytes)/float64(progress.Total)*100)
		} else {
			fmt.Fprintln(w, progress.Phase+"…")
		}
	}
}

func executeBatch(ctx context.Context, a arguments, stdout, stderr io.Writer) error {
	jobs, err := readManifest(ctx, a.paths[0])
	if err != nil {
		return err
	}
	for i := range jobs {
		jobs[i].Tools = a.tools
		if !a.json {
			index, source := i+1, jobs[i].Source
			notify := progressOutput(stderr)
			started := false
			jobs[i].Progress = func(p romcompressor.Progress) {
				if !started {
					fmt.Fprintf(stderr, "[%d/%d] %q\n", index, len(jobs), source)
					started = true
				}
				notify(p)
			}
		}
		if a.keys != "" && batchUsesSwitchKeys(jobs[i]) {
			jobs[i].Keys = a.keys
		}
	}
	results := romcompressor.Batch(ctx, jobs)
	failed := len(results) != len(jobs)
	for _, item := range results {
		if item.Error != "" {
			failed = true
		}
	}
	if a.json {
		if err := json.NewEncoder(stdout).Encode(struct {
			Results   []romcompressor.JobResult `json:"results"`
			Completed int                       `json:"completed"`
			Total     int                       `json:"total"`
			Success   bool                      `json:"success"`
		}{results, len(results), len(jobs), !failed}); err != nil {
			return err
		}
	} else {
		for _, item := range results {
			if item.Error != "" {
				fmt.Fprintf(stderr, "%s: %s\n", item.Source, item.Error)
			} else {
				fmt.Fprintln(stdout, item.Result.Output)
			}
		}
		if len(results) < len(jobs) {
			fmt.Fprintf(stderr, "Stopped after %d of %d jobs.\n", len(results), len(jobs))
		}
	}
	if ctx.Err() != nil {
		return &reportedError{ctx.Err()}
	}
	if failed {
		return &reportedError{errors.New("one or more jobs failed")}
	}
	return nil
}

func batchUsesSwitchKeys(job romcompressor.Job) bool {
	if job.Format == romcompressor.NSZ || job.Format == romcompressor.XCZ {
		return true
	}
	ext := strings.ToLower(filepath.Ext(job.Source))
	return job.Format == romcompressor.Raw && (ext == ".nsz" || ext == ".xcz")
}

// reportedError marks an error already represented in the batch result document.
type reportedError struct{ error }

func (e *reportedError) Unwrap() error { return e.error }

func readManifest(ctx context.Context, path string) ([]romcompressor.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const maxManifest = 8 << 20
	if !filepath.IsLocal(filepath.Base(path)) {
		return nil, errors.New("batch manifest must be a regular file")
	}
	before, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("batch manifest must be a regular file")
	}
	if before.Size() > maxManifest {
		return nil, errors.New("batch manifest exceeds 8 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New("batch manifest must be a regular file")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifest+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > maxManifest {
		return nil, errors.New("batch manifest exceeds 8 MiB")
	}
	var manifest struct {
		Jobs []romcompressor.Job `json:"jobs"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("read batch manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("batch manifest must contain one JSON object")
	}
	if len(manifest.Jobs) == 0 || len(manifest.Jobs) > 10000 {
		return nil, errors.New("batch manifest must contain 1–10000 jobs")
	}
	directory, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for i := range manifest.Jobs {
		job := &manifest.Jobs[i]
		if job.Source == "" || job.Output == "" || job.Format == "" {
			return nil, fmt.Errorf("job %d requires source, output, and format", i+1)
		}
		if !filepath.IsAbs(job.Source) {
			job.Source = filepath.Join(directory, job.Source)
		}
		if !filepath.IsAbs(job.Output) {
			job.Output = filepath.Join(directory, job.Output)
		}
		if job.Keys != "" && !filepath.IsAbs(job.Keys) {
			job.Keys = filepath.Join(directory, job.Keys)
		}
	}
	return manifest.Jobs, nil
}
