package glog

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

func TestDumpGoroutineStacksIncludesEveryGoroutine(t *testing.T) {
	const count = 256
	ready := make(chan struct{})
	release := make(chan struct{})
	var workers sync.WaitGroup
	t.Cleanup(func() {
		close(release)
		workers.Wait()
	})
	for range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			waitForStackDump(ready, release)
		}()
	}
	for range count {
		<-ready
	}
	var output bytes.Buffer
	if err := dumpRuntimeStacks(&output); err != nil {
		t.Fatal(err)
	}
	if output.Len() <= 16<<10 {
		t.Fatalf("fixture did not exercise a growing stack buffer: %d bytes", output.Len())
	}
	if got := bytes.Count(output.Bytes(), []byte(".waitForStackDump(")); got != count {
		t.Fatalf("dump has %d waiting goroutines, want %d", got, count)
	}
	if !bytes.Contains(output.Bytes(), []byte(".TestDumpGoroutineStacksIncludesEveryGoroutine(")) {
		t.Fatal("dump omitted the calling goroutine")
	}
}

func waitForStackDump(ready chan<- struct{}, release <-chan struct{}) {
	ready <- struct{}{}
	<-release
}

type failedStackWriter struct{ err error }

func (w failedStackWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDumpGoroutineStacksReturnsWriteError(t *testing.T) {
	want := errors.New("stack output failed")
	if got := dumpRuntimeStacks(failedStackWriter{want}); !errors.Is(got, want) {
		t.Fatalf("dump error = %v, want %v", got, want)
	}
}
