// Package disc validates contained CUE and GDI references before engines follow them.
package disc

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// MaxDescriptorSize bounds a CUE or GDI descriptor, excluding referenced images.
const MaxDescriptorSize = 4 << 20

var cueFile = regexp.MustCompile(`(?i)^\s*FILE\s+(?:"([^"]+)"|([^\s]+))\s+\S+\s*$`)
var gdiTrack = regexp.MustCompile(`^\s*\d+\s+\d+\s+\d+\s+\d+\s+(?:"([^"]+)"|([^\s]+))\s+\d+\s*$`)

// BinaryCUEFile returns a FILE directive for a sibling BIN in a restored CUE.
func BinaryCUEFile(name string) (string, error) {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\"/\\:\r\n\x00") || len(name)+len("FILE \"\" BINARY\n") > 510 {
		return "", fmt.Errorf("BIN filename cannot be represented safely in a CUE sheet")
	}
	return fmt.Sprintf("FILE \"%s\" BINARY\n", name), nil
}

// ValidateReferences checks descriptor paths before an external engine follows them.
// Disc sets must be self-contained under the chosen descriptor's folder.
func ValidateReferences(path string) error { return VisitReferences(path, nil) }

// VisitReferences validates contained paths and visits each engine-visible track.
func VisitReferences(path string, visit func(string, os.FileInfo) error) error {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".cue" && ext != ".gdi" {
		return nil
	}
	f, st, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if st.Size() > MaxDescriptorSize {
		return fmt.Errorf("disc descriptor must be a regular file of at most 4 MiB")
	}
	directory := filepath.Dir(path)
	root, err := fileops.Resolve(directory)
	if err != nil {
		return err
	}
	reader := &io.LimitedReader{R: f, N: MaxDescriptorSize + 1}
	scanner := bufio.NewScanner(reader)
	// chdman reads at most 510 bytes per line, including line endings.
	// A longer line could make the engine parse a reference hidden in its tail.
	scanner.Buffer(make([]byte, 510), 510)
	count := 0
	expected := 0
	line := 0
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		line++
		if ext == ".gdi" && line == 1 {
			expected, err = strconv.Atoi(text)
			if err != nil || expected < 1 || expected > 1000 {
				return fmt.Errorf("invalid GDI track count")
			}
			continue
		}
		var match []string
		if ext == ".cue" {
			fields := strings.Fields(text)
			if len(fields) == 0 || !strings.EqualFold(fields[0], "FILE") {
				continue
			}
			match = cueFile.FindStringSubmatch(text)
		} else {
			if text == "" {
				continue
			}
			match = gdiTrack.FindStringSubmatch(text)
		}
		if match == nil {
			return fmt.Errorf("invalid disc file reference on line %d", line)
		}
		name := match[1]
		if name == "" {
			name = match[2]
		}
		name = strings.ReplaceAll(name, "\\", "/")
		if strings.HasPrefix(name, "/") || strings.ContainsAny(name, ":\x00") {
			return fmt.Errorf("disc references must be relative")
		}
		// The engine reads the original path. Removing a parent component
		// before resolution would hide traversal through a symbolic link.
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return fmt.Errorf("disc reference contains a parent directory")
			}
		}
		lexical := filepath.Join(directory, filepath.FromSlash(name))
		referenced, err := fileops.Resolve(lexical)
		if err != nil {
			return fmt.Errorf("disc file unavailable: %w", err)
		}
		rel, err := filepath.Rel(root, referenced)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("disc reference leaves its folder")
		}
		st, err := fileops.SnapshotStat(referenced, os.Stat)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("disc reference must be a regular file")
		}
		if visit != nil {
			// Snapshot the path the engine will use, including links in the
			// descriptor's directory and in the reference itself.
			if err := visit(lexical, st); err != nil {
				return err
			}
		}
		count++
		if count > 1000 {
			return fmt.Errorf("too many disc tracks")
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read disc descriptor (510-byte line limit): %w", err)
	}
	if reader.N == 0 {
		return fmt.Errorf("disc descriptor exceeds 4 MiB")
	}
	if count == 0 || ext == ".gdi" && count != expected {
		return fmt.Errorf("disc descriptor has missing tracks")
	}
	return nil
}

// MatchCUEFile returns the complete FILE directive and quoted/bare filename groups.
func MatchCUEFile(line string) []string { return cueFile.FindStringSubmatch(line) }

// MatchGDITrack returns the complete track directive and quoted/bare filename groups.
func MatchGDITrack(line string) []string { return gdiTrack.FindStringSubmatch(line) }
