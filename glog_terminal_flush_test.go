// Terminal sequencing uses real mobile log buffers without a flush daemon.
package glog

import (
	"bytes"
	"os"
	"testing"

	"github.com/urnetwork/glog/internal/logsink"
)

// A small info entry remains in the same userspace buffer that process exit loses.
func TestMobileInfoDiagnosticIsLostWhenTerminalStopPrecedesFlush(t *testing.T) {
	sink := newBufferPolicyTestSink(t, "android")
	buffer := sink.file[logsink.Info].(*syncBuffer)
	header := []byte("synthetic info file header\n")
	entry := []byte("[auth-discovery]callback category=network\n")
	if _, err := buffer.file.Write(header); err != nil {
		t.Fatal(err)
	}
	if n, err := sink.Emit(&logsink.Meta{Severity: logsink.Info}, entry); err != nil || n != len(entry) {
		t.Fatalf("info delivery = %d, %v", n, err)
	}
	if buffer.Buffered() != len(entry) {
		t.Fatalf("pending info bytes = %d, want %d", buffer.Buffered(), len(entry))
	}
	// Close only the underlying descriptor, as abrupt process termination does.
	if err := buffer.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(); err == nil {
		t.Fatal("flush after terminal close unexpectedly succeeded")
	}
	got, err := os.ReadFile(buffer.file.Name())
	if err != nil || !bytes.Equal(got, header) {
		t.Fatalf("terminal copy = %q, want only the direct header; error %v", got, err)
	}
}

// Explicit flush completes before the terminal close and the later evidence copy.
func TestMobileInfoDiagnosticSurvivesFlushBeforeTerminalStop(t *testing.T) {
	sink := newBufferPolicyTestSink(t, "android")
	buffer := sink.file[logsink.Info].(*syncBuffer)
	header := []byte("synthetic info file header\n")
	entry := []byte("[auth-discovery]callback category=network\n")
	if _, err := buffer.file.Write(header); err != nil {
		t.Fatal(err)
	}
	if n, err := sink.Emit(&logsink.Meta{Severity: logsink.Info}, entry); err != nil || n != len(entry) {
		t.Fatalf("info delivery = %d, %v", n, err)
	}
	before, err := os.ReadFile(buffer.file.Name())
	if err != nil || !bytes.Equal(before, header) || buffer.Buffered() != len(entry) {
		t.Fatalf("info was not held before explicit flush: bytes=%q pending=%d error=%v", before, buffer.Buffered(), err)
	}
	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := buffer.file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(buffer.file.Name())
	if err != nil || !bytes.Equal(got, append(header, entry...)) || buffer.Buffered() != 0 {
		t.Fatalf("flushed terminal copy = %q, pending=%d error=%v", got, buffer.Buffered(), err)
	}
}
