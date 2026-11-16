package engine

import (
	"context"
	"fmt"

	"github.com/olsonb97/romcompressor.go/internal/fileops"
)

// withVerificationStage owns standalone verification scratch. Conversion
// staging stays beside its output so exclusive publication remains on one volume.
func withVerificationStage(ctx context.Context, opts Config, prefix string, limit int64, run func(context.Context, fileops.Stage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stage, err := fileops.NewStage(opts.TempDir, prefix)
	if err != nil {
		return fmt.Errorf("create verification scratch directory: %w", err)
	}
	defer stage.Cleanup()
	return fileops.RunMonitored(ctx, stage.Path(), limit, func(ctx context.Context) error {
		if err := stage.Check(); err != nil {
			return err
		}
		if err := run(ctx, stage); err != nil {
			return err
		}
		return stage.Check()
	})
}

// temporaryEnvironment redirects conventional child-process scratch locations.
// Engines may still use platform-specific locations; this is not an OS sandbox.
func temporaryEnvironment(directory string) []string {
	return []string{"TMPDIR=" + directory, "TMP=" + directory, "TEMP=" + directory}
}
