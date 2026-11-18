package romcompressor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/codec"
)

// FAT32 split WBFS replaces the final s in .wbfs by a single digit. Use
// the same bounded .wbf1–.wbf9 convention as Dolphin.
const maxWBFSParts = 10

type wbfsPart struct {
	path  string
	file  *os.File
	info  os.FileInfo
	start int64
}

type wbfsInput struct {
	parts []wbfsPart
	size  int64
}

func openWBFSInput(ctx context.Context, path string) (*wbfsInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	first, err := openWBFSPart(path, 0)
	if err != nil {
		return nil, err
	}
	input := &wbfsInput{parts: []wbfsPart{first}, size: first.info.Size()}
	complete := false
	defer func() {
		if !complete {
			input.close()
		}
	}()
	declared, err := codec.WBFSDeclaredSize(first.file)
	if err != nil {
		return nil, err
	}
	if input.size > declared {
		return nil, fmt.Errorf("WBFS file exceeds its declared volume size")
	}
	primaryName := strings.EqualFold(filepath.Ext(path), ".wbfs")
	if input.size < declared {
		if !primaryName {
			return nil, fmt.Errorf("split WBFS requires the primary .wbfs filename")
		}
		primary, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !primary.Mode().IsRegular() || !os.SameFile(primary, first.info) {
			return nil, fmt.Errorf("split WBFS primary must be a regular file")
		}
	}
	if primaryName {
		if err := input.openRemaining(ctx, path[:len(path)-1], declared); err != nil {
			return nil, err
		}
	}
	complete = true
	return input, nil
}

func openWBFSPart(path string, start int64) (wbfsPart, error) {
	before, err := os.Lstat(path)
	if start == 0 && err == nil && before.Mode()&os.ModeSymlink != 0 {
		before, err = os.Stat(path)
	}
	if err != nil {
		return wbfsPart{}, err
	}
	if !before.Mode().IsRegular() || before.Size() == 0 {
		return wbfsPart{}, fmt.Errorf("WBFS part %q must be a nonempty regular file", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return wbfsPart{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return wbfsPart{}, err
	}
	if !os.SameFile(before, info) || before.Size() != info.Size() || !before.ModTime().Equal(info.ModTime()) {
		file.Close()
		return wbfsPart{}, fmt.Errorf("WBFS part %q changed while opening", filepath.Base(path))
	}
	return wbfsPart{path: path, file: file, info: info, start: start}, nil
}

func (input *wbfsInput) openRemaining(ctx context.Context, prefix string, declared int64) error {
	for index := 1; index < maxWBFSParts; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := prefix + strconv.Itoa(index)
		if input.size == declared {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("unexpected WBFS part %q after the declared volume end", filepath.Base(path))
			} else if !os.IsNotExist(err) {
				return err
			}
			continue
		}
		part, err := openWBFSPart(path, input.size)
		if err != nil {
			return fmt.Errorf("open required WBFS part %q: %w", filepath.Base(path), err)
		}
		for _, prior := range input.parts {
			if os.SameFile(part.info, prior.info) {
				part.file.Close()
				return fmt.Errorf("WBFS part %q repeats an earlier file", filepath.Base(path))
			}
		}
		input.parts = append(input.parts, part)
		if part.info.Size() > declared-input.size {
			return fmt.Errorf("WBFS part %q extends beyond the declared volume size", filepath.Base(path))
		}
		input.size += part.info.Size()
	}
	if input.size != declared {
		return fmt.Errorf("split WBFS is incomplete after %d parts", maxWBFSParts)
	}
	return nil
}

func (input *wbfsInput) addToSnapshot(files map[string]os.FileInfo) {
	for _, part := range input.parts {
		files[part.path] = part.info
	}
}

func (input *wbfsInput) supportsOutput(format Format) bool {
	return len(input.parts) == 1 || format == Raw || dolphinFormat(format)
}

func (input *wbfsInput) close() {
	for _, part := range input.parts {
		part.file.Close()
	}
}

func (input *wbfsInput) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative WBFS read offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	read := 0
	for _, part := range input.parts {
		local := offset - part.start
		if local >= part.info.Size() {
			continue
		}
		count := min(int64(len(p)), part.info.Size()-local)
		n, err := part.file.ReadAt(p[:count], local)
		read += n
		if err != nil {
			return read, err
		}
		p = p[n:]
		offset += int64(n)
		if len(p) == 0 {
			return read, nil
		}
	}
	return read, io.EOF
}
