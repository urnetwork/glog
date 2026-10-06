//go:build !ios

package glog

import (
	"io"
	"runtime/pprof"
)

func dumpGoroutineStacks(w io.Writer) error {
	return pprof.Lookup("goroutine").WriteTo(w, 1)
}
