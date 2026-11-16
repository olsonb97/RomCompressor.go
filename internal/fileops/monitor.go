package fileops

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// RunMonitored runs an engine while periodically checking its staging budget.
// The final scan also catches output produced just before engine exit.
func RunMonitored(ctx context.Context, stage string, limit int64, run func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done, stopped := make(chan struct{}), make(chan struct{})
	go Monitor(ctx, cancel, stage, limit, 10000, done, stopped)
	err := run(ctx)
	close(done)
	<-stopped
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// Monitor bounds staging bytes and entries until done closes or ctx is cancelled.
// It always closes stopped when monitoring has finished.
func Monitor(ctx context.Context, cancel context.CancelCauseFunc, stage string, limit int64, maxEntries int, done <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		finished := false
		select {
		case <-done:
			finished = true
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := Scan(ctx, stage, limit, maxEntries); err != nil {
			cancel(err)
			return
		}
		if finished {
			return
		}
	}
}

// Scan checks a staging tree in bounded batches with at most one directory open.
// Engines may remove scratch entries while a scan is in progress.
func Scan(ctx context.Context, stage string, limit int64, maxEntries int) error {
	count := 1 // Include the staging root.
	var total int64
	pending := []string{stage}
	scanDirectory := func(path string) error {
		before, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if before.Mode().Type() != os.ModeDir {
			return fmt.Errorf("staging directory became a link or other entry")
		}
		dir, err := OpenDirectory(path)
		if err != nil {
			return err
		}
		defer dir.Close()
		opened, err := dir.Stat()
		if err != nil {
			return err
		}
		if opened.Mode().Type() != os.ModeDir || !os.SameFile(before, opened) {
			return fmt.Errorf("staging directory changed while opening")
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, readErr := dir.ReadDir(128)
			for _, entry := range entries {
				count++
				if count > maxEntries {
					return fmt.Errorf("staging exceeds %d entries", maxEntries)
				}
				info, err := entry.Info()
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return err
				}
				switch info.Mode().Type() {
				case os.ModeDir:
					pending = append(pending, filepath.Join(path, entry.Name()))
				case 0:
					if info.Size() > limit-total {
						return fmt.Errorf("staging exceeds output limit")
					}
					total += info.Size()
				default:
					return fmt.Errorf("engine created a link or special file")
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
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		last := len(pending) - 1
		path := pending[last]
		pending[last] = ""
		pending = pending[:last]
		if err := scanDirectory(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return ctx.Err()
}
