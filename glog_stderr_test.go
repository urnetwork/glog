// Console write failures are nonfatal; explicit termination and custom sink
// errors retain their contracts. Terminating paths run only in bounded children.

package glog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/glog/internal/logsink"
)

// Adapts controlled write outcomes to the existing console writer seam.
type stderrTestWriter func([]byte) (int, error)

// Executes exactly one controlled write.
func (self stderrTestWriter) Write(data []byte) (int, error) {
	return self(data)
}

// A failed entry is not retried, and the next entry still reaches the writer.
func checkStderrWriteRecovery(t *testing.T, failedBytes int, writeErr error) {
	t.Helper()
	failedEntry := []byte("failed console entry\n")
	nextEntry := []byte("next console entry\n")
	var recovered bytes.Buffer
	writeCalls := 0
	sink := &stderrSink{w: stderrTestWriter(func(data []byte) (int, error) {
		writeCalls++
		if writeCalls == 1 {
			if !bytes.Equal(data, failedEntry) {
				t.Errorf("first write received a different entry")
			}
			return failedBytes, writeErr
		}
		return recovered.Write(data)
	})}
	meta := &logsink.Meta{Severity: logsink.Info}
	if n, err := sink.Emit(meta, failedEntry); n != failedBytes || err != nil {
		t.Errorf("failed write Emit = (%d, %v), want (%d, nil)", n, err, failedBytes)
	}
	if writeCalls != 1 {
		t.Fatalf("failed entry caused %d write calls, want 1", writeCalls)
	}
	if n, err := sink.Emit(meta, nextEntry); n != len(nextEntry) || err != nil {
		t.Errorf("next write Emit = (%d, %v), want (%d, nil)", n, err, len(nextEntry))
	}
	if writeCalls != 2 || !bytes.Equal(recovered.Bytes(), nextEntry) {
		t.Errorf("next entry was not written exactly once without replaying the failed entry")
	}
}

// No bytes written is a dropped diagnostic entry, not a fatal sink error.
func TestStderrSinkZeroWriteErrorIsNonfatal(t *testing.T) {
	checkStderrWriteRecovery(t, 0, io.ErrClosedPipe)
}

// A partial write keeps its byte count without retrying or aborting.
func TestStderrSinkPartialWriteErrorIsNonfatal(t *testing.T) {
	checkStderrWriteRecovery(t, 4, io.ErrShortWrite)
}

// A real closed file must exercise the same contract without closing fd 1 or 2.
func TestStderrSinkClosedFileErrorIsNonfatal(t *testing.T) {
	closedFile, err := os.CreateTemp(t.TempDir(), "closed-console-")
	if err != nil {
		t.Fatal(err)
	}
	if err := closedFile.Close(); err != nil {
		t.Fatal(err)
	}
	entry := []byte("closed file console entry\n")
	if n, err := closedFile.Write(entry); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed-file control = (%d, %v), want (0, os.ErrClosed)", n, err)
	}
	sink := &stderrSink{w: closedFile}
	meta := &logsink.Meta{Severity: logsink.Info}
	if n, err := sink.Emit(meta, entry); n != 0 || err != nil {
		t.Errorf("closed-file Emit = (%d, %v), want (0, nil)", n, err)
	}
	var recovered bytes.Buffer
	sink.w = &recovered
	if n, err := sink.Emit(meta, entry); n != len(entry) || err != nil {
		t.Errorf("replacement writer Emit = (%d, %v), want (%d, nil)", n, err, len(entry))
	}
	if !bytes.Equal(recovered.Bytes(), entry) {
		t.Error("replacement console writer did not receive the entry")
	}
}

const (
	stderrChildEnv = "UR_GLOG_STDERR_TEST_CHILD"
	stderrDirEnv   = "UR_GLOG_STDERR_TEST_LOG_DIR"
)

// Only the custom-error child registers this sink, before any test logs.
func init() {
	if os.Getenv(stderrChildEnv) == "TestStderrCustomSinkErrorStillAborts" {
		logsink.TextSinks = append(logsink.TextSinks, stderrCustomErrorSink{})
	}
}

// Represents an unrelated sink that deliberately requests fatal handling.
type stderrCustomErrorSink struct{}

// Every log in the isolated custom-error child exercises its fatal contract.
func (self stderrCustomErrorSink) Enabled(*logsink.Meta) bool { return true }

// Console error handling must not suppress errors returned by another sink.
func (self stderrCustomErrorSink) Emit(*logsink.Meta, []byte) (int, error) {
	return 0, errors.New("synthetic custom sink failure")
}

// Real Info, Warning, and Error calls must return and persist in the file sink.
func TestStderrInfoPersistsWhenConsoleFails(t *testing.T) {
	checkStderrSubprocess(t, "info", 0, []string{
		"stderr-before-failure",
		"stderr-info-after-console-failure",
		"stderr-warning-after-console-failure",
		"stderr-error-after-console-failure",
		"stderr-console-recovered",
	})
}

// Explicit Fatal still flushes pending entries and terminates with broken stderr.
func TestStderrFatalStillTerminatesAndFlushes(t *testing.T) {
	checkStderrSubprocess(t, "fatal", 2, []string{
		"stderr-before-failure",
		"stderr-explicit-fatal",
	})
}

// Explicit Exit keeps its status-one contract instead of returning or aborting.
func TestStderrExitStillTerminatesAndFlushes(t *testing.T) {
	checkStderrSubprocess(t, "exit", 1, []string{
		"stderr-before-failure",
		"stderr-explicit-exit",
	})
}

// An unrelated fatal sink error must still abort an ordinary Info call.
func TestStderrCustomSinkErrorStillAborts(t *testing.T) {
	checkStderrSubprocess(t, "custom", 2, []string{
		"stderr-custom-info",
		"log: exiting because of error writing previous log to sinks:",
		"synthetic custom sink failure",
	})
}

// The child runs only this test; its exit status and durable files are the proof.
// The deadline bounds a broken child, not the ordering or outcome under test.
func checkStderrSubprocess(t *testing.T, scenario string, wantExit int, messages []string) {
	t.Helper()
	switch runtime.GOOS {
	case "android", "ios", "js", "wasip1":
		t.Skip("requires host subprocess execution; direct stderr sink tests remain portable")
	}
	if os.Getenv(stderrChildEnv) == t.Name() {
		runStderrChild(t, scenario)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logDirectory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.timeout=10s")
	command.Env = append(os.Environ(),
		stderrChildEnv+"="+t.Name(),
		stderrDirEnv+"="+logDirectory,
		"GOTRACEBACK=single",
	)
	command.WaitDelay = time.Second
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("child exceeded its deadline: %v", ctx.Err())
	}
	gotExit := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("cannot run child: %v", err)
		}
		gotExit = exitError.ExitCode()
	}
	if gotExit != wantExit {
		t.Fatalf("child exit = %d, want %d; output:\n%s", gotExit, wantExit, output)
	}
	if scenario == "info" && !bytes.Contains(output, []byte("stderr-info-returned")) {
		t.Fatal("child did not finish the ordinary logging path")
	}
	all := readDirContents(t, logDirectory)
	for _, message := range messages {
		if !strings.Contains(all, message) {
			t.Errorf("durable log missing %q", message)
		}
	}
	if scenario == "info" && strings.Contains(all, "log: exiting because of error writing previous log to sinks:") {
		t.Error("console failure reached fatal sink-error handling")
	}
}

// Uses existing writer and file-sink seams; package sink lists stay immutable.
func runStderrChild(t *testing.T, scenario string) {
	t.Helper()
	registered := false
	for _, sink := range logsink.TextSinks {
		if sink == &sinks.stderr {
			registered = true
		}
	}
	if !registered {
		t.Fatal("child has no registered stderr sink")
	}
	previousToStderr, previousAlsoToStderr, previousBufferLevel := toStderr, alsoToStderr, *logBufLevel
	t.Cleanup(func() {
		toStderr, alsoToStderr, *logBufLevel = previousToStderr, previousAlsoToStderr, previousBufferLevel
	})
	toStderr, alsoToStderr, *logBufLevel = false, true, int(logsink.Fatal)
	resetFileSink(t)
	if err := SetLogDir(os.Getenv(stderrDirEnv)); err != nil {
		t.Fatal(err)
	}
	var recovered bytes.Buffer
	previousWriter := func() io.Writer {
		sinks.stderr.mu.Lock()
		defer sinks.stderr.mu.Unlock()
		previous := sinks.stderr.w
		sinks.stderr.w = &recovered
		return previous
	}()
	t.Cleanup(func() {
		sinks.stderr.mu.Lock()
		defer sinks.stderr.mu.Unlock()
		sinks.stderr.w = previousWriter
	})
	if scenario == "custom" {
		Info("stderr-custom-info")
		os.Exit(99) // Returning from the fatal custom-sink path is a failure.
	}
	Info("stderr-before-failure")
	// The parent owns this directory even when Fatal bypasses child cleanups.
	closedFile, err := os.CreateTemp(os.Getenv(stderrDirEnv), "closed-console-")
	if err != nil {
		t.Fatal(err)
	}
	if err := closedFile.Close(); err != nil {
		t.Fatal(err)
	}
	func() {
		sinks.stderr.mu.Lock()
		defer sinks.stderr.mu.Unlock()
		sinks.stderr.w = closedFile
	}()
	switch scenario {
	case "info":
		Info("stderr-info-after-console-failure")
		Warning("stderr-warning-after-console-failure")
		Error("stderr-error-after-console-failure")
		func() {
			sinks.stderr.mu.Lock()
			defer sinks.stderr.mu.Unlock()
			sinks.stderr.w = &recovered
		}()
		Info("stderr-console-recovered")
		Flush()
		if !strings.Contains(recovered.String(), "stderr-console-recovered") {
			t.Fatal("next ordinary log did not reach the recovered console writer")
		}
		fmt.Fprintln(os.Stdout, "stderr-info-returned")
		return
	case "fatal":
		Fatal("stderr-explicit-fatal")
	case "exit":
		Exit("stderr-explicit-exit")
	default:
		t.Fatalf("unknown child scenario %q", scenario)
	}
	os.Exit(99) // Explicit Fatal and Exit must not return to their caller.
}
