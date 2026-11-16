// Package progress provides synchronous phase-aware progress throttling.
package progress

import (
	"time"
)

// Reporter reports phase changes immediately and throttles updates within one
// phase to at most one per 100 ms. A nil callback produces a no-op reporter.
func Reporter(fn func(string, int64, int64)) func(string, int64, int64) {
	var last time.Time
	var phase string
	return func(p string, n, total int64) {
		if fn != nil && (p != phase || time.Since(last) >= 100*time.Millisecond) {
			fn(p, n, total)
			phase = p
			last = time.Now()
		}
	}
}
