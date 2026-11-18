package engine

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

func convertDolphin(ctx context.Context, source, output string, input string, opts Config) error {
	if opts.Inspect == nil {
		return fmt.Errorf("Dolphin conversion requires output inspection")
	}
	if !kind.Dolphin(opts.Format) && opts.Format != kind.Raw {
		return fmt.Errorf("Dolphin output must be rvz, wia, gcz, or raw")
	}
	if !kind.Dolphin(input) && input != kind.Raw {
		return fmt.Errorf("Dolphin input must be a GameCube or Wii disc image")
	}
	if input == kind.Raw {
		if err := checkDolphinISO(source); err != nil {
			return err
		}
	}
	tool, err := resolveTool(opts, "dolphin-tool")
	if err != nil {
		return err
	}
	userDir, err := os.MkdirTemp(filepath.Dir(output), "dolphin-user-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(userDir)
	before, err := dolphinDiscHash(ctx, tool, source, userDir, input, opts.MaxOutput)
	if err != nil {
		return err
	}
	target := string(opts.Format)
	if opts.Format == kind.Raw {
		target = "iso"
	}
	args := []string{"convert", "-u", userDir, "-i", source, "-o", output, "-f", target}
	switch opts.Format {
	case kind.RVZ:
		args = append(args, "-b", "131072", "-c", "zstd", "-l", "5")
	case kind.WIA:
		// WIA blocks must align to Wii groups (2 MiB), unlike RVZ blocks.
		args = append(args, "-b", "2097152", "-c", "lzma2", "-l", "5")
	case kind.GCZ:
		args = append(args, "-b", "16384")
	}
	if _, err = process.Run(ctx, tool, args...); err != nil {
		return err
	}
	format, size, err := opts.Inspect(ctx, output)
	if err != nil {
		return fmt.Errorf("inspect Dolphin output: %w", err)
	}
	if format != opts.Format {
		return fmt.Errorf("Dolphin produced %s output instead of %s", format, opts.Format)
	}
	if opts.MaxOutput > 0 && size > opts.MaxOutput {
		return fmt.Errorf("Dolphin output exceeds the size limit")
	}
	if opts.Format == kind.Raw {
		if err := checkDolphinISO(output); err != nil {
			return err
		}
	}
	after, err := dolphinDiscHash(ctx, tool, output, userDir, opts.Format, opts.MaxOutput)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("Dolphin verification failed: decompressed disc hashes differ")
	}
	return nil
}

func checkDolphinISO(path string) error {
	file, _, err := fileops.OpenRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var header [32]byte
	if _, err := io.ReadFull(file, header[:]); err != nil ||
		(binary.BigEndian.Uint32(header[24:28]) != 0x5d1c9ea3 && binary.BigEndian.Uint32(header[28:32]) != 0xc2339f3d) {
		return fmt.Errorf("RVZ, WIA, and GCZ require a GameCube or Wii disc image")
	}
	return nil
}

// DolphinDiscSize reads the logical ISO size from Dolphin's GCZ blob header
// or the shared first WIA/RVZ header before an engine starts reading the disc.
func DolphinDiscSize(path, format string, limit int64) (int64, error) {
	file, _, err := fileops.OpenRegular(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var order binary.ByteOrder = binary.BigEndian
	var offset int64 = 36
	if format == kind.GCZ {
		order, offset = binary.LittleEndian, 16
	}
	var field [8]byte
	if _, err := file.ReadAt(field[:], offset); err != nil {
		return 0, fmt.Errorf("read %s disc size: %w", format, err)
	}
	size := order.Uint64(field[:])
	if limit == 0 {
		limit = kind.DefaultMaxOutput
	}
	if size == 0 || size > uint64(limit) {
		return 0, fmt.Errorf("%s disc has invalid size or exceeds the expanded size limit", format)
	}
	return int64(size), nil
}

func dolphinDiscHash(ctx context.Context, tool, path, userDir string, format string, limit int64) ([sha1.Size]byte, error) {
	if format != kind.Raw {
		if _, err := DolphinDiscSize(path, format, limit); err != nil {
			return [sha1.Size]byte{}, err
		}
		return dolphinHash(ctx, tool, path, userDir)
	}
	var result [sha1.Size]byte
	digest := sha1.New()
	if err := fileops.HashFile(ctx, path, digest); err != nil {
		return result, err
	}
	digest.Sum(result[:0])
	return result, nil
}

// Dolphin's hash-only CLI prints a single digest to stdout on success.
func dolphinHash(ctx context.Context, tool, path, userDir string) ([sha1.Size]byte, error) {
	return dolphinHashWithEnvironment(ctx, tool, path, userDir, nil)
}

func dolphinHashWithEnvironment(ctx context.Context, tool, path, userDir string, env []string) ([sha1.Size]byte, error) {
	var stdout process.Log
	if _, err := process.Execute(ctx, &stdout, env, "", tool, "verify", "-u", userDir, "-i", path, "-a", "sha1"); err != nil {
		return [sha1.Size]byte{}, err
	}
	return parseDolphinHash(stdout.String())
}

func parseDolphinHash(stdout string) ([sha1.Size]byte, error) {
	var result [sha1.Size]byte
	value := strings.TrimSpace(stdout)
	if len(value) != hex.EncodedLen(len(result)) {
		return result, fmt.Errorf("Dolphin did not return a single SHA-1 checksum")
	}
	if _, err := hex.Decode(result[:], []byte(value)); err != nil {
		return result, fmt.Errorf("Dolphin returned an invalid SHA-1 checksum")
	}
	return result, nil
}
