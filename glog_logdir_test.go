// Tests for the urnetwork fork's log-directory handling: SetLogDir,
// SetMaxLogSize, and the file sink's behavior when the directory is missing.

package glog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/glog/internal/logsink"
)

// resetFileSink registers a cleanup that closes any real log files and
// restores the file sink and log-directory globals, so tests that create
// real files do not leak state into each other.
func resetFileSink(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		s := &sinks.file
		s.mu.Lock()
		defer s.mu.Unlock()
		for sev := logsink.Info; sev <= logsink.Fatal; sev++ {
			if sb, ok := s.file[sev].(*syncBuffer); ok && sb.file != nil {
				sb.Flush()
				sb.file.Close()
			}
			s.file[sev] = nil
		}
		logDirs = nil
		s.dirSet.Store(false)
		s.errReported = false
	})
}

// readDirContents concatenates the contents of all regular files in dir.
func readDirContents(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() { // skip the INFO/WARNING/... symlinks
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func TestSetLogDirWritesFiles(t *testing.T) {
	setFlags()
	dir := t.TempDir()
	resetFileSink(t)
	if err := SetLogDir(dir); err != nil {
		t.Fatal(err)
	}

	Info("hello-setlogdir")
	Flush()

	names, err := Names("INFO")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no INFO log file created")
	}
	for _, name := range names {
		if filepath.Dir(name) != dir {
			t.Errorf("log file %q not in configured directory %q", name, dir)
		}
	}
	if !strings.Contains(readDirContents(t, dir), "hello-setlogdir") {
		t.Errorf("log message not found in %q", dir)
	}
}

func TestSetLogDirEmpty(t *testing.T) {
	if err := SetLogDir(""); err == nil {
		t.Error("SetLogDir(\"\") succeeded, want error")
	}
}

// A second SetLogDir call must move the log chain, not register the file
// sink twice: each entry must appear exactly once.
func TestSetLogDirTwiceNoDuplicateLines(t *testing.T) {
	setFlags()
	d1, d2 := t.TempDir(), t.TempDir()
	resetFileSink(t)
	if err := SetLogDir(d1); err != nil {
		t.Fatal(err)
	}
	Info("before-move")
	if err := SetLogDir(d2); err != nil {
		t.Fatal(err)
	}
	Info("token-after-move")
	Flush()

	all := readDirContents(t, d1) + readDirContents(t, d2)
	if got := strings.Count(all, "token-after-move"); got != 1 {
		t.Errorf("entry written %d times, want exactly 1", got)
	}
	if got := strings.Count(all, "before-move"); got != 1 {
		t.Errorf("pre-move entry written %d times, want exactly 1", got)
	}
	if !strings.Contains(readDirContents(t, d2), "token-after-move") {
		t.Error("post-move entry did not land in the new directory")
	}
}

// SetLogDir must be safe to call while other goroutines are logging.
// This test is most meaningful under -race.
func TestSetLogDirConcurrentWithLogging(t *testing.T) {
	setFlags()
	dirs := make([]string, 8)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	resetFileSink(t)
	if err := SetLogDir(dirs[0]); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					Info("concurrent write")
				}
			}
		}()
	}
	for _, dir := range dirs[1:] {
		if err := SetLogDir(dir); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	wg.Wait()
	Flush()
}

// A log directory that becomes unwritable after SetLogDir must not abort
// the process; entries are dropped and file logging recovers when the
// directory is restored.
func TestFileWriteFailureDoesNotAbort(t *testing.T) {
	setFlags()
	dir := t.TempDir()
	resetFileSink(t)
	if err := SetLogDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	Info("dropped entry") // must not terminate the test process
	Flush()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	Info("recovered entry")
	Flush()

	all := readDirContents(t, dir)
	if !strings.Contains(all, "recovered entry") {
		t.Error("file logging did not recover after the directory was restored")
	}
	if strings.Contains(all, "dropped entry") {
		t.Error("entry written while the directory was missing; expected it to be dropped")
	}
}

func TestSetMaxLogSize(t *testing.T) {
	defer func(previous uint64) { maxLogSize.Store(previous) }(maxLogSize.Load())
	SetMaxLogSize(12345)
	if got := maxLogSize.Load(); got != 12345 {
		t.Errorf("maxLogSize = %d after SetMaxLogSize(12345)", got)
	}
	if err := maxLogSize.Set("777"); err != nil { // the --max_log_size flag path
		t.Fatal(err)
	}
	if got := maxLogSize.Load(); got != 777 {
		t.Errorf("maxLogSize = %d after flag Set(\"777\")", got)
	}
	if err := maxLogSize.Set("not-a-number"); err == nil {
		t.Error("flag Set accepted a non-numeric value")
	}
}
