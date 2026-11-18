package romcompressor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

func convertWUA(ctx context.Context, source, output string, opts Options) (Result, error) {
	if opts.Format != Raw && opts.Format != WUA {
		return Result{}, fmt.Errorf("WUA archives extract to a title directory")
	}
	sourceInfo, err := fileops.SnapshotStat(source, os.Lstat)
	if err != nil {
		return Result{}, err
	}
	var manifest wuaManifest
	var inputs map[string]os.FileInfo
	var prefix string
	if opts.Format == WUA {
		if !sourceInfo.IsDir() {
			return Result{}, fmt.Errorf("WUA compression requires a decrypted Wii U title directory")
		}
		if err := checkTreeOutput(source, output); err != nil {
			return Result{}, err
		}
		manifest, inputs, prefix, err = prepareWUATitleTree(ctx, source, opts.MaxOutput)
		if err != nil {
			return Result{}, err
		}
	} else {
		if !sourceInfo.Mode().IsRegular() || sourceInfo.Size() > opts.MaxOutput {
			return Result{}, fmt.Errorf("WUA source must be a regular file within the output limit")
		}
		inputs = map[string]os.FileInfo{source: sourceInfo}
	}
	tool, err := process.Resolve(opts.Tools, "zarchive")
	if err != nil {
		return Result{}, err
	}
	tool, err = filepath.Abs(tool)
	if err != nil {
		return Result{}, err
	}
	stage, err := fileops.NewStage(filepath.Dir(output), ".romcompressor-wua-")
	if err != nil {
		return Result{}, err
	}
	defer stage.Cleanup()

	monitorCtx, cancel := context.WithCancelCause(ctx)
	done, stopped := make(chan struct{}), make(chan struct{})
	go fileops.Monitor(monitorCtx, cancel, stage.Path(), opts.MaxOutput, wuaMaxNodes+3, done, stopped)
	defer func() {
		close(done)
		<-stopped
		cancel(nil)
	}()
	notify := progressReporter(opts.Progress)
	inputBytes := sourceInfo.Size()
	if opts.Format == WUA {
		inputBytes = manifest.size
	}
	notify("converting", 0, inputBytes)
	if err := stage.Check(); err != nil {
		return Result{}, err
	}
	var outputBytes int64
	if opts.Format == WUA {
		outputBytes, err = packWUA(monitorCtx, tool, source, output, stage, prefix, opts.MaxOutput, manifest, inputs, notify)
	} else {
		outputBytes, err = unpackWUA(monitorCtx, tool, source, output, stage, opts.MaxOutput, sourceInfo, notify)
	}
	if err != nil {
		if cause := context.Cause(monitorCtx); cause != nil {
			return Result{}, cause
		}
		return Result{}, err
	}
	notify("complete", outputBytes, outputBytes)
	return Result{Format: opts.Format, Output: output, InputBytes: inputBytes, OutputBytes: outputBytes, Verified: true}, nil
}

func packWUA(ctx context.Context, tool, source, output string, stage fileops.Stage, prefix string, limit int64, manifest wuaManifest, inputs map[string]os.FileInfo, notify func(string, int64, int64)) (int64, error) {
	packDir := filepath.Join(stage.Path(), "pack")
	if err := os.Mkdir(packDir, 0700); err != nil {
		return 0, err
	}
	digests := make(map[string][32]byte)
	buffer := make([]byte, 128<<10)
	for _, name := range wuaPaths(manifest.entries) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		dest := filepath.Join(packDir, filepath.FromSlash(name))
		if manifest.entries[name] == wuaDirectory {
			if err := os.Mkdir(dest, 0700); err != nil {
				return 0, err
			}
			continue
		}
		input := filepath.Join(source, filepath.FromSlash(strings.TrimPrefix(name, prefix)))
		digest, err := copyWUAFile(ctx, input, dest, inputs[input], buffer)
		if err != nil {
			return 0, err
		}
		digests[name] = digest
	}
	if err := runZArchive(ctx, tool, stage, "pack", "archive.wua"); err != nil {
		return 0, err
	}
	if err := os.RemoveAll(packDir); err != nil {
		return 0, err
	}
	archivePath := filepath.Join(stage.Path(), "archive.wua")
	notify("verifying", 0, manifest.size)
	notify("publishing", 0, 0)
	if err := stage.Check(); err != nil {
		return 0, err
	}
	archived, archive, err := verifyWUAFile(ctx, archivePath, limit)
	if err != nil {
		return 0, err
	}
	if !sameWUAEntries(archived, manifest) {
		return 0, fmt.Errorf("ZArchive changed the title directory while packing it")
	}
	restored, stats, err := extractWUATree(ctx, tool, stage, "archive.wua", limit-archive.info.Size(), archived)
	if err != nil {
		return 0, err
	}
	for name, expected := range digests {
		path := filepath.Join(restored, filepath.FromSlash(name))
		hash := sha256.New()
		if err := streamSnapshotFile(ctx, hash, path, stats[path], buffer); err != nil {
			return 0, err
		}
		var actual [32]byte
		copy(actual[:], hash.Sum(nil))
		if actual != expected {
			return 0, fmt.Errorf("WUA round trip changed %q", name)
		}
	}
	if err := os.RemoveAll(restored); err != nil {
		return 0, err
	}
	if err := checkInputs(inputs); err != nil {
		return 0, err
	}
	if err := stage.Check(); err != nil {
		return 0, err
	}
	entries, err := readPublishEntries(stage.Path(), 2)
	if err != nil {
		return 0, err
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(archivePath) {
		return 0, fmt.Errorf("unexpected staged WUA output")
	}
	if err := archive.check(ctx, archivePath); err != nil {
		return 0, err
	}
	if _, err := publishRegularFile(ctx, archivePath, output, archive.info); err != nil {
		return 0, err
	}
	return archive.info.Size(), nil
}

func unpackWUA(ctx context.Context, tool, source, output string, stage fileops.Stage, limit int64, sourceInfo os.FileInfo, notify func(string, int64, int64)) (int64, error) {
	notify("verifying", 0, sourceInfo.Size())
	notify("publishing", 0, 0)
	if err := stage.Check(); err != nil {
		return 0, err
	}
	archivePath := filepath.Join(stage.Path(), "source.wua")
	// The engine opens its input independently. Verify a private copy so a
	// changed source cannot bypass path validation before external extraction.
	if _, err := copyWUAFile(ctx, source, archivePath, sourceInfo, make([]byte, 128<<10)); err != nil {
		return 0, err
	}
	manifest, archive, err := verifyWUAFile(ctx, archivePath, limit)
	if err != nil {
		return 0, err
	}
	restored, stats, err := extractWUATree(ctx, tool, stage, "source.wua", limit-archive.info.Size(), manifest)
	if err != nil {
		return 0, err
	}
	if err := checkWUATitleMetadata(ctx, restored, manifest); err != nil {
		return 0, err
	}
	for path, info := range stats {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if info.IsDir() {
			continue
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return 0, err
		}
		syncErr := file.Sync()
		if err := errors.Join(syncErr, file.Close()); err != nil {
			return 0, err
		}
	}
	if err := archive.check(ctx, archivePath); err != nil {
		return 0, err
	}
	if err := checkInputs(map[string]os.FileInfo{source: sourceInfo}); err != nil {
		return 0, err
	}
	if err := checkInputs(stats); err != nil {
		return 0, err
	}
	if err := stage.Check(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := fileops.Publish(restored, output); err != nil {
		return 0, fmt.Errorf("publish WUA title directory without overwriting: %w", err)
	}
	return manifest.size, nil
}

func extractWUATree(ctx context.Context, tool string, stage fileops.Stage, archive string, limit int64, expected wuaManifest) (string, map[string]os.FileInfo, error) {
	if expected.size > limit {
		return "", nil, fmt.Errorf("WUA archive and expanded files exceed the staging limit")
	}
	if err := runZArchive(ctx, tool, stage, archive, "restored"); err != nil {
		return "", nil, err
	}
	entries, err := readPublishEntries(stage.Path(), 3)
	if err != nil {
		return "", nil, err
	}
	if len(entries) != 2 {
		return "", nil, fmt.Errorf("ZArchive did not leave exactly the expected staged archive and tree")
	}
	for _, entry := range entries {
		if (entry.Name() != archive || !entry.Type().IsRegular()) && (entry.Name() != "restored" || entry.Type() != os.ModeDir) {
			return "", nil, fmt.Errorf("ZArchive left an unexpected staged entry")
		}
	}
	restored := filepath.Join(stage.Path(), "restored")
	actual, stats, err := readWUATree(ctx, restored, limit)
	if err != nil {
		return "", nil, err
	}
	if !sameWUAEntries(actual, expected) {
		return "", nil, fmt.Errorf("ZArchive did not restore all title directories and file sizes")
	}
	return restored, stats, nil
}

func runZArchive(ctx context.Context, tool string, stage fileops.Stage, source, output string) error {
	if err := stage.Check(); err != nil {
		return err
	}
	// ZArchive writes one progress line per member. The staged tree, digests
	// and archive checksum provide verification independent of that listing.
	if _, err := process.Execute(ctx, io.Discard, nil, stage.Path(), tool, source, output); err != nil {
		return err
	}
	return stage.Check()
}

func verifyWUA(ctx context.Context, path string, opts Options) error {
	_, _, err := verifyWUAFile(ctx, path, opts.MaxOutput)
	return err
}

type wuaArchiveSnapshot struct {
	info   os.FileInfo
	footer [wuaFooterSize]byte
}

// The extraction engine reopens this file. Check the original footer and
// digest before publication, even if the file's size and mtime still match.
func (archive wuaArchiveSnapshot) check(ctx context.Context, path string) error {
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if !fileops.SameSnapshot(archive.info, info) {
		return fmt.Errorf("staged WUA archive changed after verification")
	}
	var footer [wuaFooterSize]byte
	if _, err := file.ReadAt(footer[:], info.Size()-wuaFooterSize); err != nil {
		return err
	}
	if footer != archive.footer {
		return fmt.Errorf("staged WUA archive footer changed after verification")
	}
	if err := checkWUAChecksum(ctx, file, info.Size(), footer); err != nil {
		return fmt.Errorf("recheck staged WUA archive: %w", err)
	}
	return checkInputs(map[string]os.FileInfo{path: archive.info})
}

func verifyWUAFile(ctx context.Context, path string, limit int64) (wuaManifest, wuaArchiveSnapshot, error) {
	file, info, err := fileops.OpenRegular(path)
	if err != nil {
		return wuaManifest{}, wuaArchiveSnapshot{}, err
	}
	defer file.Close()
	if !info.Mode().IsRegular() || info.Size() > limit {
		return wuaManifest{}, wuaArchiveSnapshot{}, fmt.Errorf("WUA source must be a regular file within the output limit")
	}
	manifest, footer, err := readWUA(ctx, file, info.Size(), limit, true)
	if err != nil {
		return wuaManifest{}, wuaArchiveSnapshot{}, err
	}
	if err := checkWUAChecksum(ctx, file, info.Size(), footer); err != nil {
		return wuaManifest{}, wuaArchiveSnapshot{}, err
	}
	return manifest, wuaArchiveSnapshot{info: info, footer: footer}, checkInputs(map[string]os.FileInfo{path: info})
}

func prepareWUATitleTree(ctx context.Context, root string, limit int64) (wuaManifest, map[string]os.FileInfo, string, error) {
	manifest, stats, err := readWUATree(ctx, root, limit)
	if err != nil {
		return wuaManifest{}, nil, "", err
	}
	var prefix string
	if manifest.entries["code/app.xml"] > 0 {
		name, err := readWUATitleName(ctx, filepath.Join(root, "code", "app.xml"))
		if err != nil {
			return wuaManifest{}, nil, "", err
		}
		prefix = name + "/"
		entries := make(map[string]int64, len(manifest.entries)+1)
		entries[name] = wuaDirectory
		for path, size := range manifest.entries {
			if len(prefix)+len(path) > wuaMaxPath {
				return wuaManifest{}, nil, "", fmt.Errorf("WUA title path exceeds supported length")
			}
			entries[prefix+path] = size
		}
		manifest.entries = entries
	}
	if err := validateWUALayout(manifest); err != nil {
		return wuaManifest{}, nil, "", err
	}
	if len(manifest.entries) >= wuaMaxNodes {
		return wuaManifest{}, nil, "", fmt.Errorf("WUA title tree exceeds 100000 nodes")
	}
	if prefix == "" {
		if err := checkWUATitleMetadata(ctx, root, manifest); err != nil {
			return wuaManifest{}, nil, "", err
		}
	}
	return manifest, stats, prefix, nil
}

func checkWUATitleMetadata(ctx context.Context, root string, manifest wuaManifest) error {
	for name := range manifest.entries {
		if strings.Contains(name, "/") {
			continue
		}
		metadata, err := readWUATitleName(ctx, filepath.Join(root, name, "code", "app.xml"))
		if err != nil {
			return err
		}
		if !strings.EqualFold(name, metadata) {
			return fmt.Errorf("WUA title folder %q does not match app.xml title and version", name)
		}
	}
	return nil
}

func readWUATitleName(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, _, err := fileops.OpenRegular(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	const maxXML = 1 << 20
	data, err := io.ReadAll(io.LimitReader(&fileops.ContextReader{Context: ctx, Reader: file}, maxXML+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxXML {
		return "", fmt.Errorf("Wii U app.xml exceeds 1 MiB")
	}
	var app struct {
		XMLName  xml.Name      `xml:"app"`
		IDs      []wuaXMLValue `xml:"title_id"`
		Versions []wuaXMLValue `xml:"title_version"`
	}
	// Cemu matches literal names such as app and title_id. Preserve XML
	// prefixes so a same-local-name element from elsewhere cannot match.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	decoder := xml.NewTokenDecoder(wuaXMLTokens{xml.NewDecoder(bytes.NewReader(data))})
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read Wii U app.xml: %w", err)
		}
		switch node := token.(type) {
		case xml.StartElement:
			if app.XMLName.Local != "" {
				return "", fmt.Errorf("Wii U app.xml contains multiple roots")
			}
			if err := decoder.DecodeElement(&app, &node); err != nil {
				return "", fmt.Errorf("read Wii U app.xml: %w", err)
			}
		case xml.CharData:
			if len(bytes.Trim(node, " \t\r\n")) != 0 {
				return "", fmt.Errorf("unexpected text outside the Wii U app.xml root")
			}
		}
	}
	if len(app.IDs) != 1 || len(app.Versions) != 1 {
		return "", fmt.Errorf("Wii U app.xml requires exactly one title_id and one title_version")
	}
	idText := string(app.IDs[0])
	versionText := string(app.Versions[0])
	if len(idText) != 16 || !unsignedWUAValue(idText, 16, 64) || !unsignedWUAValue(versionText, 16, 16) {
		return "", fmt.Errorf("invalid title ID or version in Wii U app.xml")
	}
	version, _ := strconv.ParseUint(versionText, 16, 16)
	return fmt.Sprintf("%s_v%d", strings.ToLower(idText), version), nil
}

type wuaXMLValue string

// Cemu reads the first text node. Joining fragments could produce a different
// title ID or version from the one Cemu reads from the archived app.xml.
func (value *wuaXMLValue) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var seenText bool
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch node := token.(type) {
		case xml.EndElement:
			return nil
		case xml.CharData:
			if seenText {
				return fmt.Errorf("Wii U %s contains multiple text fragments", start.Name.Local)
			}
			*value = wuaXMLValue(bytes.Trim(node, " \t\r\n"))
			seenText = true
		case xml.StartElement:
			return fmt.Errorf("Wii U %s contains a nested element", start.Name.Local)
		}
	}
}

type wuaXMLTokens struct{ *xml.Decoder }

func (r wuaXMLTokens) Token() (xml.Token, error) {
	token, err := r.RawToken()
	switch node := token.(type) {
	case xml.StartElement:
		node.Name = literalXMLName(node.Name)
		return node, err
	case xml.EndElement:
		node.Name = literalXMLName(node.Name)
		return node, err
	default:
		return token, err
	}
}

func literalXMLName(name xml.Name) xml.Name {
	if name.Space != "" {
		return xml.Name{Local: name.Space + ":" + name.Local}
	}
	return name
}

func readWUATree(ctx context.Context, root string, limit int64) (wuaManifest, map[string]os.FileInfo, error) {
	manifest := wuaManifest{entries: make(map[string]int64)}
	stats := make(map[string]os.FileInfo)
	info, err := fileops.SnapshotStat(root, os.Lstat)
	if err != nil {
		return manifest, nil, err
	}
	if !info.IsDir() {
		return manifest, nil, fmt.Errorf("WUA title tree must be a directory")
	}
	stats[root] = info
	var visit func(string) error
	visit = func(prefix string) error {
		dir, err := os.Open(filepath.Join(root, filepath.FromSlash(prefix)))
		if err != nil {
			return err
		}
		defer dir.Close()
		if prefix != "" {
			prefix += "/"
		}
		seen := make(map[string]struct{})
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, readErr := dir.ReadDir(128)
			for _, entry := range entries {
				name := entry.Name()
				rel := prefix + name
				if !safeWUANodeName(name) || len(rel) > wuaMaxPath {
					return fmt.Errorf("unsupported WUA path %q", rel)
				}
				key := strings.ToLower(name)
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("case-insensitive WUA name collision for %q", rel)
				}
				seen[key] = struct{}{}
				if len(stats) >= wuaMaxNodes {
					return fmt.Errorf("WUA title tree exceeds 100000 nodes")
				}
				path := filepath.Join(root, filepath.FromSlash(rel))
				// Use Lstat for the snapshot. Windows directory enumeration can
				// report a different directory size than the follow-up path stat.
				info, err := fileops.SnapshotStat(path, os.Lstat)
				if err != nil {
					return err
				}
				stats[path] = info
				switch {
				case info.IsDir():
					manifest.entries[rel] = wuaDirectory
					if err := visit(rel); err != nil {
						return err
					}
				case info.Mode().IsRegular():
					if info.Size() > limit-manifest.size {
						return fmt.Errorf("WUA title tree exceeds expansion limit")
					}
					manifest.entries[rel] = info.Size()
					manifest.size += info.Size()
				default:
					return fmt.Errorf("WUA title tree includes a link or special file: %q", rel)
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	err = visit("")
	return manifest, stats, err
}

func wuaPaths(entries map[string]int64) []string {
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func sameWUAEntries(a, b wuaManifest) bool {
	if a.size != b.size || len(a.entries) != len(b.entries) {
		return false
	}
	for name, size := range a.entries {
		if other, ok := b.entries[name]; !ok || other != size {
			return false
		}
	}
	return true
}

func copyWUAFile(ctx context.Context, source, dest string, info os.FileInfo, buffer []byte) ([32]byte, error) {
	var digest [32]byte
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return digest, err
	}
	hash := sha256.New()
	err = streamSnapshotFile(ctx, io.MultiWriter(out, hash), source, info, buffer)
	if err := errors.Join(err, out.Close()); err != nil {
		return digest, err
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func streamSnapshotFile(ctx context.Context, dst io.Writer, path string, info os.FileInfo, buffer []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, actual, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if info == nil || !fileops.SameSnapshot(info, actual) {
		return fmt.Errorf("source file changed during staging: %s", path)
	}
	n, err := io.CopyBuffer(dst, io.LimitReader(&fileops.ContextReader{Context: ctx, Reader: file}, actual.Size()), buffer)
	if err != nil {
		return err
	}
	if n != actual.Size() {
		return fmt.Errorf("source file changed during staging: %s", path)
	}
	return ctx.Err()
}
