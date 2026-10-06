//go:build ios

package glog

import (
	"io"
)

// The iOS network extension has a compiled-size budget. Using pprof.Lookup
// just for the fatal fallback also retains the heap, block and other profile
// writers. runtime.Stack preserves the full all-goroutine crash diagnostic
// without linking those unrelated profilers. Grow until the dump is complete.
func dumpGoroutineStacks(w io.Writer) error {
	return dumpRuntimeStacks(w)
}
