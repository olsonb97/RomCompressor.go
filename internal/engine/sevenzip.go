package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

func compress7z(ctx context.Context, source, output string, opts Config) error {
	if err := validate7zName(filepath.Base(source)); err != nil {
		return err
	}
	tool, err := resolveTool(opts, "7z")
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	expected, err := fileops.Digest(ctx, source)
	if err != nil {
		return err
	}
	if _, err = process.Run(ctx, tool, "a", "-t7z", "-bd", "-spd", "-y", "-mx=5", "-mmt=2", output, "--", source); err != nil {
		return err
	}
	if err = test7z(ctx, tool, output); err != nil {
		return err
	}
	hash := sha256.New()
	writer := &externalBoundWriter{writer: hash, remaining: info.Size()}
	if err = stream7zMember(ctx, writer, tool, output, filepath.Base(source)); err != nil {
		return err
	}
	if writer.remaining != 0 || !bytes.Equal(expected[:], hash.Sum(nil)) {
		return fmt.Errorf("7z verification failed: archived bytes differ from the source")
	}
	return nil
}

func extract7z(ctx context.Context, source, output string, opts Config) error {
	tool, err := resolveTool(opts, "7z")
	if err != nil {
		return err
	}
	entries, err := list7z(ctx, tool, source)
	if err != nil {
		return err
	}
	var selected *Entry
	requested := filepath.ToSlash(opts.Entry)
	for i := range entries {
		if requested == filepath.ToSlash(entries[i].Name) || (opts.Entry == "" && len(entries) == 1) {
			if selected != nil {
				return fmt.Errorf("7z archive contains duplicate entry names")
			}
			selected = &entries[i]
		}
	}
	if selected == nil {
		if opts.Entry != "" {
			return fmt.Errorf("7z entry was not found")
		}
		return fmt.Errorf("7z archive contains %d files; select one with entry", len(entries))
	}
	limit := opts.MaxOutput
	if limit == 0 {
		limit = 1 << 40
	}
	if err := check7zExpansion([]Entry{*selected}, limit); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writer := &externalBoundWriter{writer: file, remaining: int64(selected.Size)}
	extractErr := stream7zMember(ctx, writer, tool, source, selected.Name)
	closeErr := file.Close()
	if extractErr != nil {
		return extractErr
	}
	if closeErr != nil {
		return closeErr
	}
	if writer.remaining != 0 {
		return fmt.Errorf("7z output length does not match its entry")
	}
	return nil
}

func ArchiveEntries(ctx context.Context, source string, opts Config) ([]Entry, error) {
	tool, err := resolveTool(opts, "7z")
	if err != nil {
		return nil, err
	}
	return list7z(ctx, tool, source)
}

func list7z(ctx context.Context, tool, source string) ([]Entry, error) {
	listing, err := process.Run(ctx, tool, "l", "-t7z", "-slt", "-ba", "-sccUTF-8", "-spd", "--", source)
	if err != nil {
		return nil, err
	}
	return parse7zEntries(listing)
}

func verify7z(ctx context.Context, source string, opts Config) error {
	tool, err := resolveTool(opts, "7z")
	if err != nil {
		return err
	}
	entries, err := list7z(ctx, tool, source)
	if err != nil {
		return err
	}
	if err := check7zExpansion(entries, opts.MaxOutput); err != nil {
		return err
	}
	return test7z(ctx, tool, source)
}

func check7zExpansion(entries []Entry, remaining int64) error {
	if remaining < 0 {
		return fmt.Errorf("invalid 7z expansion limit")
	}
	for _, entry := range entries {
		if entry.Size > uint64(remaining) {
			return fmt.Errorf("7z expanded data exceeds the output limit")
		}
		remaining -= int64(entry.Size)
	}
	return nil
}

func test7z(ctx context.Context, tool, source string) error {
	log, err := process.Run(ctx, tool, "t", "-t7z", "-bd", "-spd", "-y", "--", source)
	if err != nil {
		return err
	}
	return check7zResult(log, "")
}

func stream7zMember(ctx context.Context, dst io.Writer, tool, source, name string) error {
	// Keep raw member bytes on stdout and request per-member status on stderr.
	log, err := process.RunTo(ctx, dst, tool, "x", "-t7z", "-so", "-bso2", "-bb1", "-bd", "-ssc", "-spd", "-sccUTF-8", "-y", "--", source, name)
	if err != nil {
		return err
	}
	return check7zResult(log, name)
}

func check7zResult(log, member string) error {
	var success, extracted bool
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch line {
		case "Everything is Ok":
			if success || member != "" && !extracted {
				return fmt.Errorf("7z returned an unexpected success record")
			}
			success = true
		case "No files to process":
			return fmt.Errorf("7z did not process any files")
		}
		if name, ok := strings.CutPrefix(line, "- "); ok && member != "" {
			if extracted || filepath.ToSlash(name) != filepath.ToSlash(member) {
				return fmt.Errorf("7z did not extract exactly the selected entry")
			}
			extracted = true
		}
	}
	if !success || member != "" && !extracted {
		return fmt.Errorf("7z did not confirm successful processing")
	}
	return nil
}

func parse7zEntries(listing string) ([]Entry, error) {
	var entries []Entry
	listing = strings.ReplaceAll(listing, "\r\n", "\n")
	for _, block := range strings.Split(listing, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var entry Entry
		var hasPath, hasSize, directory bool
		for _, line := range strings.Split(block, "\n") {
			if line == "" {
				continue
			}
			key, value, ok := strings.Cut(line, " = ")
			if !ok {
				return nil, fmt.Errorf("7z returned malformed entry metadata")
			}
			switch key {
			case "Path":
				if hasPath {
					return nil, fmt.Errorf("7z returned multiple paths for one entry")
				}
				hasPath = true
				entry.Name = value
			case "Size":
				if hasSize {
					return nil, fmt.Errorf("7z returned multiple sizes for one entry")
				}
				var err error
				entry.Size, err = strconv.ParseUint(value, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("7z returned an invalid entry size")
				}
				hasSize = true
			case "Folder":
				directory = directory || value == "+"
			case "Attributes", "Mode":
				isDirectory, special := classify7zAttributes(value)
				if special {
					return nil, fmt.Errorf("7z archive contains a link or special entry")
				}
				directory = directory || isDirectory
			case "Encrypted", "Anti", "Alternate Stream":
				if value == "+" {
					return nil, fmt.Errorf("7z archive contains an unsupported entry type")
				}
			case "Symbolic Link", "Hard Link":
				if value != "" {
					return nil, fmt.Errorf("7z archive contains a link")
				}
			}
		}
		if directory {
			continue
		}
		if !hasSize {
			return nil, fmt.Errorf("7z returned an entry without its size")
		}
		if err := validate7zName(entry.Name); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Both creation and extraction must be able to select a member through the
// engine's UTF-8, line-oriented listing and command-line filename syntax.
func validate7zName(name string) error {
	if strings.HasPrefix(name, "@") {
		return fmt.Errorf("7z filename cannot begin with @")
	}
	if name == "" || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("7z entry name cannot be selected safely")
	}
	return nil
}

// Technical listings may include Windows flags followed by a POSIX mode.
func classify7zAttributes(value string) (directory, special bool) {
	for i, field := range strings.Fields(value) {
		if len(field) == 10 && strings.Trim(field[1:], "-rwxstST") == "" {
			directory = directory || field[0] == 'd'
			special = special || !strings.ContainsRune("-d0", rune(field[0]))
		} else if i == 0 {
			directory = strings.ContainsRune(field, 'D')
			special = strings.ContainsAny(field, "Ld")
		}
	}
	return directory, special
}

type externalBoundWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *externalBoundWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("external output exceeds its declared size")
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}
