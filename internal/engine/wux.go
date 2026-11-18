package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/olsonb97/romcompressor.go/internal/codec"
	"github.com/olsonb97/romcompressor.go/internal/fileops"
	kind "github.com/olsonb97/romcompressor.go/internal/format"
	"github.com/olsonb97/romcompressor.go/internal/process"
)

func convertWUX(ctx context.Context, source, output string, input string, opts Config) error {
	if (input != kind.Raw || opts.Format != kind.WUX || strings.ToLower(filepath.Ext(source)) != ".wud") && (input != kind.WUX || opts.Format != kind.Raw) {
		return fmt.Errorf("WUX conversion requires WUD input or raw output")
	}
	if err := ValidateWUXSource(ctx, source, input, opts); err != nil {
		return err
	}
	tool, err := resolveTool(opts, "java")
	if err != nil {
		return err
	}
	tool, err = filepath.Abs(tool)
	if err != nil {
		return err
	}
	jar := opts.Tools["jwudtool"]
	if jar == "" {
		return fmt.Errorf("configure jwudtool with the path to JWUDTool.jar")
	}
	jar, err = filepath.Abs(jar)
	if err != nil {
		return err
	}
	if info, err := os.Stat(jar); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("JWUDTool JAR is unavailable")
	}
	mode := "-compress"
	generatedName := "game.wux"
	if input == kind.WUX {
		mode = "-decompress"
		generatedName = "game.wud"
	}
	engineDir, err := os.MkdirTemp(filepath.Dir(output), "wux-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(engineDir)
	// JWUDTool accepts an output directory and uses a fixed filename. Isolate
	// its default key-file searches as well; compression does not need keys.
	var stdout wuxProgressLog
	if _, err := process.Execute(ctx, &stdout, nil, engineDir, tool, "-Duser.home="+engineDir, "-jar", jar, "-in", source, "-out", engineDir, mode); err != nil {
		return err
	}
	result, err := stdout.finish()
	if err != nil {
		return err
	}
	// JWUDTool 0.4 reports verification failure in text while exiting zero.
	if !wuxConfirmed(result) {
		return fmt.Errorf("JWUDTool did not confirm that the converted image matches its source")
	}
	generated := filepath.Join(engineDir, generatedName)
	if err := ValidateWUXSource(ctx, generated, opts.Format, opts); err != nil {
		return fmt.Errorf("validate JWUDTool output: %w", err)
	}
	return fileops.Publish(generated, output)
}

const wuxProgressPrefix = "\rCompressing into .wux | Progress "

// wuxProgressLog removes JWUDTool's carriage-return compression updates, which
// exceed the log limit for a full WUD. Keep diagnostics and verification output.
type wuxProgressLog struct {
	log     process.Log
	matched int
}

func (l *wuxProgressLog) Write(p []byte) (int, error) {
	size := len(p)
	for len(p) > 0 {
		switch l.matched {
		case len(wuxProgressPrefix):
			end := bytes.IndexAny(p, "\r\n")
			if end < 0 {
				return size, nil
			}
			p = p[end:]
			l.matched = 0
		case 0:
			before, after, found := bytes.Cut(p, []byte{'\r'})
			if _, err := l.log.Write(before); err != nil {
				return size, err
			}
			if !found {
				return size, nil
			}
			p = after
			l.matched = 1
		default:
			length := min(len(p), len(wuxProgressPrefix)-l.matched)
			if string(p[:length]) == wuxProgressPrefix[l.matched:l.matched+length] {
				l.matched += length
				p = p[length:]
			} else {
				if _, err := l.log.Write([]byte(wuxProgressPrefix[:l.matched])); err != nil {
					return size, err
				}
				l.matched = 0
			}
		}
	}
	return size, nil
}

func (l *wuxProgressLog) finish() (string, error) {
	if l.matched > 0 && l.matched < len(wuxProgressPrefix) {
		if _, err := l.log.Write([]byte(wuxProgressPrefix[:l.matched])); err != nil {
			return "", err
		}
	}
	return l.log.String(), nil
}

func wuxConfirmed(result string) bool {
	confirmed := false
	for _, line := range strings.Split(result, "\n") {
		switch strings.TrimSpace(line) {
		case "Compressed files is valid.":
			confirmed = true
		case "Warning! (De)Compressed file is INVALID!":
			return false
		}
	}
	return confirmed
}

func ValidateWUXSource(ctx context.Context, source string, input string, opts Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, info, err := fileops.OpenRegular(source)
	if err != nil {
		return err
	}
	defer file.Close()
	if opts.MaxOutput > 0 && opts.MaxOutput < codec.WUDImageBytes {
		return fmt.Errorf("WUX image exceeds the expanded size limit")
	}
	if input == kind.Raw {
		var magic [4]byte
		if _, err := file.ReadAt(magic[:], 0); err != nil && err != io.EOF {
			return err
		}
		// JWUDTool would reconstruct this archive before comparing images.
		if string(magic[:]) == "Wii " {
			return fmt.Errorf("WUMAD archives are unsupported for WUD compression")
		}
		if info.Size() != codec.WUDImageBytes {
			return fmt.Errorf("JWUDTool requires a complete 25,025,314,816-byte WUD image")
		}
		return nil
	}
	return codec.ValidateWUX(ctx, file, info.Size())
}
