package romcompressor

import (
	"errors"
	"fmt"

	"github.com/olsonb97/romcompressor.go/internal/process"
)

// ErrNoRemovablePadding means a supported cartridge already has no removable tail.
// It is an ordinary no-change result; the source and destination are untouched.
var ErrNoRemovablePadding = errors.New("image has no removable padding")

// DependencyError identifies a missing or unavailable external executable.
// Use errors.As to offer engine setup without parsing a human error message.
type DependencyError struct {
	Engine     string
	Configured bool
	// Err preserves the original operation context and underlying lookup error.
	Err error
}

func (e *DependencyError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s executable is unavailable", e.Engine)
}

// Unwrap preserves standard errors.Is checks on the underlying failure.
func (e *DependencyError) Unwrap() error { return e.Err }

func dependencyError(err error) error {
	var public *DependencyError
	if errors.As(err, &public) {
		return err
	}
	var missing *process.ToolError
	if errors.As(err, &missing) {
		return &DependencyError{Engine: missing.Engine, Configured: missing.Configured, Err: err}
	}
	return err
}
