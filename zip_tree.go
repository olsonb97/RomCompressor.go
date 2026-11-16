package romcompressor

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

func compressZIPTree(ctx context.Context, source, output string, opts Options) (Result, error) {
	if err := checkTreeOutput(source, output); err != nil {
		return Result{}, err
	}
	stage, err := fileops.NewStage(filepath.Dir(output), ".romcompressor-zip-")
	if err != nil {
		return Result{}, err
	}
	defer stage.Cleanup()
	staged := filepath.Join(stage.Path(), filepath.Base(output))
	notify := progressReporter(opts.Progress)
	stats, inputBytes, err := writeZIPTree(ctx, source, stage, staged, opts.MaxOutput, notify)
	if err != nil {
		return Result{}, err
	}
	if err := checkInputs(stats); err != nil {
		return Result{}, err
	}
	if err := stage.Check(); err != nil {
		return Result{}, err
	}
	size, err := publish(ctx, stage.Path(), staged, output, opts.MaxOutput)
	if err != nil {
		return Result{}, err
	}
	notify("complete", size, size)
	return Result{Output: output, Format: ZIP, InputBytes: inputBytes, OutputBytes: size, Verified: true}, nil
}

// Staging inside the source would change the tree during enumeration. Resolve
// both paths before comparing them so an aliased parent cannot hide that case.
func checkTreeOutput(source, output string) error {
	root, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, parent)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("choose an archive output outside the source directory")
	}
	return nil
}

func writeZIPTree(ctx context.Context, source string, stage fileops.Stage, staged string, limit int64, notify func(string, int64, int64)) (map[string]os.FileInfo, int64, error) {
	file, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	writer := zip.NewWriter(fileops.NewBoundedStageFile(file, limit))
	layout := newZIPTreeLayout()
	digests := make(map[string][sha256.Size]byte)
	stats := make(map[string]os.FileInfo)
	buffer := make([]byte, 128<<10)
	var total int64
	err = visitZIPTree(ctx, source, "", func(name string, info os.FileInfo) error {
		localPath := filepath.Join(source, filepath.FromSlash(name))
		stats[localPath] = info
		if name == "" {
			return nil
		}
		if err := layout.declare(name, info.IsDir()); err != nil {
			return err
		}
		if !info.IsDir() && info.Size() > limit-total {
			return fmt.Errorf("ZIP folder exceeds expanded size limit")
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = name
		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}
		dst, err := writer.CreateHeader(header)
		if err != nil || info.IsDir() {
			return err
		}
		digest := sha256.New()
		if err := streamSnapshotFile(ctx, io.MultiWriter(dst, digest), localPath, info, buffer); err != nil {
			return err
		}
		var value [sha256.Size]byte
		digest.Sum(value[:0])
		digests[name] = value
		total += info.Size()
		notify("converting", total, 0)
		return nil
	})
	if err := errors.Join(err, writer.Close()); err != nil {
		return nil, 0, err
	}
	notify("verifying", 0, total)
	notify("publishing", 0, 0)
	if err := stage.Check(); err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	current, err := os.Lstat(staged)
	if err != nil || !os.SameFile(current, info) {
		return nil, 0, fmt.Errorf("staged ZIP changed before verification")
	}
	archive, restored, expanded, err := readZIPTree(ctx, file, info.Size(), limit)
	if err != nil {
		return nil, 0, err
	}
	if expanded != total || !layout.matches(restored) {
		return nil, 0, fmt.Errorf("staged ZIP does not reproduce source tree")
	}
	for _, member := range archive.File {
		if member.Mode().IsDir() {
			continue
		}
		digest := sha256.New()
		if err := codec.CopyZIPMember(ctx, digest, member, buffer, nil); err != nil {
			return nil, 0, err
		}
		var actual [sha256.Size]byte
		digest.Sum(actual[:0])
		if actual != digests[member.Name] {
			return nil, 0, fmt.Errorf("staged ZIP changed file %q", member.Name)
		}
	}
	return stats, total, nil
}

// Read small directory batches so the entry cap applies before an entire
// large directory can be allocated. The visitor validates each path before
// recursion. Files and directories are checked against their opened handles.
func visitZIPTree(ctx context.Context, root, name string, visit func(string, os.FileInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	local := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Lstat(local)
	if err != nil {
		return err
	}
	if !info.IsDir() && (name == "" || !info.Mode().IsRegular()) {
		return fmt.Errorf("ZIP tree contains a link or special file: %q", name)
	}
	if err := visit(name, info); err != nil || !info.IsDir() {
		return err
	}
	dir, err := os.Open(local)
	if err != nil {
		return err
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil {
		return err
	}
	if !opened.IsDir() || !os.SameFile(info, opened) {
		return fmt.Errorf("source directory changed during ZIP creation: %q", name)
	}
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			child := entry.Name()
			if name != "" {
				child = name + "/" + child
			}
			if err := visitZIPTree(ctx, root, child, visit); err != nil {
				return err
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

func extractZIPTree(ctx context.Context, source, output string, opts Options) (Result, error) {
	file, info, err := fileops.OpenRegular(source)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	archive, layout, total, err := readZIPTree(ctx, file, info.Size(), opts.MaxOutput)
	if err != nil {
		return Result{}, err
	}
	stage, err := fileops.NewStage(filepath.Dir(output), ".romcompressor-zip-")
	if err != nil {
		return Result{}, err
	}
	defer stage.Cleanup()
	for _, dir := range layout.directories() {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if err := os.Mkdir(filepath.Join(stage.Path(), filepath.FromSlash(dir)), 0700); err != nil {
			return Result{}, err
		}
	}
	notify := progressReporter(opts.Progress)
	notify("converting", 0, total)
	buffer := make([]byte, 128<<10)
	var copied map[string]zipTreeFileDigest
	if opts.Progress != nil {
		copied = make(map[string]zipTreeFileDigest)
	}
	var done int64
	for _, member := range archive.File {
		if member.Mode().IsDir() {
			continue
		}
		if copied != nil {
			if err := stage.Check(); err != nil {
				return Result{}, err
			}
			if err := checkZIPStageParents(stage.Path(), member.Name); err != nil {
				return Result{}, err
			}
		}
		if err := writeZIPMember(ctx, stage.Path(), member, buffer, copied); err != nil {
			return Result{}, err
		}
		done += int64(member.UncompressedSize64)
		notify("converting", done, total)
	}
	notify("publishing", 0, 0)
	if err := stage.Check(); err != nil {
		return Result{}, err
	}
	if copied != nil {
		if err := verifyExtractedZIPTree(ctx, stage.Path(), layout, copied, buffer); err != nil {
			return Result{}, err
		}
	}
	if err := checkInputs(map[string]os.FileInfo{source: info}); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := fileops.Publish(stage.Path(), output); err != nil {
		return Result{}, fmt.Errorf("publish ZIP directory without replacing files: %w", err)
	}
	notify("complete", total, total)
	return Result{Output: output, Format: Raw, InputBytes: info.Size(), OutputBytes: total, Verified: true}, nil
}

func checkZIPStageParents(root, name string) error {
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil || info.Mode().Type() != os.ModeDir {
			return fmt.Errorf("staged ZIP directory %q changed during extraction", dir)
		}
	}
	return nil
}

type zipTreeFileDigest struct {
	size int64
	sum  [sha256.Size]byte
}

func writeZIPMember(ctx context.Context, root string, member *zip.File, buffer []byte, copied map[string]zipTreeFileDigest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dst, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(member.Name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	var writer io.Writer = dst
	var digest hash.Hash
	if copied != nil {
		digest = sha256.New()
		writer = io.MultiWriter(dst, digest)
	}
	err = codec.CopyZIPMember(ctx, writer, member, buffer, nil)
	if err == nil && member.Mode().Perm()&0100 != 0 {
		// Keep extracted files private, but retain the owner's executable bit
		// so Unix launch scripts remain usable. Never restore special bits.
		err = dst.Chmod(0700)
	}
	if err == nil && digest != nil {
		var value zipTreeFileDigest
		value.size = int64(member.UncompressedSize64)
		digest.Sum(value.sum[:0])
		copied[member.Name] = value
	}
	if err == nil {
		err = dst.Sync()
	}
	return errors.Join(err, dst.Close())
}

func verifyExtractedZIPTree(ctx context.Context, root string, layout *zipTreeLayout, copied map[string]zipTreeFileDigest, buffer []byte) error {
	var seen int
	err := visitZIPTree(ctx, root, "", func(name string, info os.FileInfo) error {
		if name == "" {
			return nil
		}
		node, ok := layout.nodes[strings.Map(zipFoldRune, name)]
		if !ok || name != node.name || info.IsDir() != node.directory {
			return fmt.Errorf("staged ZIP tree changed at %q", name)
		}
		seen++
		if info.IsDir() {
			return nil
		}
		expected, ok := copied[name]
		if !ok || info.Size() != expected.size {
			return fmt.Errorf("staged ZIP file %q changed size", name)
		}
		digest := sha256.New()
		if err := streamSnapshotFile(ctx, digest, filepath.Join(root, filepath.FromSlash(name)), info, buffer); err != nil {
			return err
		}
		var actual [sha256.Size]byte
		digest.Sum(actual[:0])
		if actual != expected.sum {
			return fmt.Errorf("staged ZIP file %q changed contents", name)
		}
		return nil
	})
	if err == nil && seen != len(layout.nodes) {
		return fmt.Errorf("staged ZIP tree is missing entries")
	}
	return err
}

// Bound compressed metadata before zip.NewReader allocates entries. Validate
// every destination path and combined expansion before visiting the payload.
func readZIPTree(ctx context.Context, reader io.ReaderAt, size, limit int64) (*codec.ZIPArchive, *zipTreeLayout, int64, error) {
	archive, err := codec.OpenZIP(ctx, reader, size)
	if err != nil {
		return nil, nil, 0, err
	}
	layout := newZIPTreeLayout()
	var total int64
	for index, member := range archive.File {
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		if err := archive.ValidateMember(index); err != nil {
			return nil, nil, 0, err
		}
		name := strings.TrimSuffix(member.Name, "/")
		if err := layout.declare(name, member.Mode().IsDir()); err != nil {
			return nil, nil, 0, err
		}
		if member.UncompressedSize64 > uint64(limit-total) {
			return nil, nil, 0, fmt.Errorf("ZIP tree exceeds expanded size limit")
		}
		total += int64(member.UncompressedSize64)
	}
	return archive, layout, total, nil
}

type zipTreeNode struct {
	name      string
	directory bool
	explicit  bool
}

type zipTreeLayout struct {
	nodes     map[string]zipTreeNode
	nameBytes int
}

func newZIPTreeLayout() *zipTreeLayout {
	return &zipTreeLayout{nodes: make(map[string]zipTreeNode)}
}

func (tree *zipTreeLayout) declare(name string, directory bool) error {
	if !safeZIPTreePath(name) {
		return fmt.Errorf("unsafe ZIP tree path %q", name)
	}
	if err := tree.add(name, directory, true); err != nil {
		return err
	}
	for ancestor := path.Dir(name); ancestor != "."; ancestor = path.Dir(ancestor) {
		if err := tree.add(ancestor, true, false); err != nil {
			return err
		}
	}
	return nil
}

func (tree *zipTreeLayout) add(name string, directory, explicit bool) error {
	key := strings.Map(zipFoldRune, name)
	node, exists := tree.nodes[key]
	if exists {
		if node.name != name || node.directory != directory || explicit && node.explicit {
			return fmt.Errorf("colliding ZIP tree path %q", name)
		}
		node.explicit = node.explicit || explicit
		tree.nodes[key] = node
		return nil
	}
	const maxTreeNodes = 100000
	const maxTreeNameBytes = 32 << 20
	if len(tree.nodes) >= maxTreeNodes || len(name) > maxTreeNameBytes-tree.nameBytes {
		return fmt.Errorf("ZIP tree paths exceed supported metadata limits")
	}
	tree.nodes[key] = zipTreeNode{name, directory, explicit}
	tree.nameBytes += len(name)
	return nil
}

// Choose the same key for all runes equal under simple Unicode case folding.
func zipFoldRune(r rune) rune {
	key := r
	for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
		key = min(key, folded)
	}
	return key
}

func (tree *zipTreeLayout) directories() []string {
	var names []string
	for _, node := range tree.nodes {
		if node.directory {
			names = append(names, node.name)
		}
	}
	// A parent always sorts before any path beginning with parent+"/".
	sort.Strings(names)
	return names
}

func (tree *zipTreeLayout) matches(other *zipTreeLayout) bool {
	if len(tree.nodes) != len(other.nodes) {
		return false
	}
	for key, node := range tree.nodes {
		if other.nodes[key] != node {
			return false
		}
	}
	return true
}

func safeZIPTreePath(name string) bool {
	if len(name) == 0 || len(name) > 1024 || !utf8.ValidString(name) || strings.Count(name, "/") >= 64 || strings.ContainsAny(name, "\\:<>\"|?*") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if len(part) == 0 || len(part) > 255 || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || reservedZIPName(part) {
			return false
		}
	}
	return true
}

func reservedZIPName(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		switch base[3:] {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}
