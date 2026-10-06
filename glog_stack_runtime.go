package glog

import (
	"io"
	"runtime"
)

// Kept platform-independent so the iOS fatal fallback is exercised by the
// regular host test suite as well as compiled by the mobile build.
func dumpRuntimeStacks(w io.Writer) error {
	for size := 16 << 10; ; size *= 2 {
		buffer := make([]byte, size)
		n := runtime.Stack(buffer, true)
		if n < len(buffer) {
			_, err := w.Write(buffer[:n])
			return err
		}
	}
}
