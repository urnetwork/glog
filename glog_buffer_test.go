package glog

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/glog/internal/logsink"
)

func TestLogFileBufferSizeMatchesPlatformBudget(t *testing.T) {
	for _, platform := range []string{"android", "ios", "darwin", "linux", "windows", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			want := 256 * 1024
			if platform == "android" || platform == "ios" {
				want = 32 * 1024
			}
			if got := logFileBufferSize(platform); got != want {
				t.Fatalf("log buffer = %d bytes, want %d", got, want)
			}
			// A single ERROR opens INFO, WARNING, and ERROR files. Their
			// buffers remain live even after the error and traffic end.
			if got := 3 * logFileBufferSize(platform); got != 3*want {
				t.Fatalf("three retained severity buffers = %d, want %d", got, 3*want)
			}
		})
	}
}

// A local sink exercises routing, buffering, and explicit flushes without
// racing the process-global flush daemon. Only its buffer policy varies.
func newBufferPolicyTestSink(t testing.TB, platform string) *fileSink {
	t.Helper()
	s := &fileSink{flushChan: make(chan logsink.Severity, 1)}
	dir := t.TempDir()
	for sev := logsink.Info; sev <= logsink.Fatal; sev++ {
		file, err := os.Create(filepath.Join(dir, sev.String()))
		if err != nil {
			t.Fatal(err)
		}
		s.file[sev] = &syncBuffer{
			sink: s, Writer: bufio.NewWriterSize(file, logFileBufferSize(platform)),
			file: file, names: []string{file.Name()}, sev: sev, madeAt: time.Now(),
		}
		t.Cleanup(func() { file.Close() })
	}
	return s
}

func TestMobileFileSinkPreservesSeverityAndFlush(t *testing.T) {
	previous := *logBufLevel
	*logBufLevel = int(logsink.Info)
	t.Cleanup(func() { *logBufLevel = previous })
	for _, platform := range []string{"android", "ios", "linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			s := newBufferPolicyTestSink(t, platform)
			var expected [4][]byte
			for sev := logsink.Info; sev <= logsink.Fatal; sev++ {
				data := []byte("retained-severity-" + sev.String() + "\n")
				if n, err := s.Emit(&logsink.Meta{Severity: sev}, data); err != nil || n != len(data) {
					t.Fatalf("Emit(%s) = (%d, %v)", sev, n, err)
				}
				for dst := logsink.Info; dst <= sev; dst++ {
					expected[dst] = append(expected[dst], data...)
				}
				select {
				case got := <-s.flushChan:
					if sev == logsink.Info || got != sev {
						t.Fatalf("Emit(%s) requested flush %s", sev, got)
					}
				default:
					if sev != logsink.Info {
						t.Fatalf("Emit(%s) did not request prompt flush", sev)
					}
				}
			}
			// Small entries remain buffered until the same explicit/daemon
			// flush, irrespective of the mobile retention limit.
			for sev := logsink.Info; sev <= logsink.Fatal; sev++ {
				sb := s.file[sev].(*syncBuffer)
				if sb.Size() != logFileBufferSize(platform) {
					t.Fatalf("%s has unexpected buffer capacity", sev)
				}
				if info, err := sb.file.Stat(); err != nil || info.Size() != 0 {
					t.Fatalf("%s written before flush: %v, %v", sev, info, err)
				}
			}
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			for sev := logsink.Info; sev <= logsink.Fatal; sev++ {
				sb := s.file[sev].(*syncBuffer)
				got, err := os.ReadFile(sb.file.Name())
				if err != nil || !bytes.Equal(got, expected[sev]) {
					t.Fatalf("%s severity delivery changed: got %q, want %q, error %v", sev, got, expected[sev], err)
				}
				if sb.Buffered() != 0 {
					t.Fatalf("%s retained bytes after flush", sev)
				}
			}
		})
	}
}

func TestMobileFileSinkWritesDoNotAllocate(t *testing.T) {
	for _, platform := range []string{"android", "ios", "linux"} {
		t.Run(platform, func(t *testing.T) {
			s := newBufferPolicyTestSink(t, platform)
			data := bytes.Repeat([]byte("test error line\n"), 16)
			meta := &logsink.Meta{Severity: logsink.Error}
			var writeErr error
			allocs := testing.AllocsPerRun(1000, func() {
				_, writeErr = s.Emit(meta, data)
			})
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			if allocs != 0 {
				t.Fatalf("steady ERROR writes allocated %.2f objects, want zero", allocs)
			}
		})
	}
}

func TestMobileFileSinkPreservesLargeErrorEntries(t *testing.T) {
	for _, platform := range []string{"android", "ios", "linux"} {
		t.Run(platform, func(t *testing.T) {
			s := newBufferPolicyTestSink(t, platform)
			var expected []byte
			for _, size := range []int{32*1024 - 1, 32*1024 + 1, 256*1024 + 17} {
				data := bytes.Repeat([]byte("L"), size)
				data[len(data)-1] = '\n'
				if n, err := s.Emit(&logsink.Meta{Severity: logsink.Error}, data); err != nil || n != len(data) {
					t.Fatalf("large ERROR write = (%d, %v), want %d bytes", n, err, len(data))
				}
				expected = append(expected, data...)
			}
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			for sev := logsink.Info; sev <= logsink.Fatal; sev++ {
				sb := s.file[sev].(*syncBuffer)
				want := expected
				if sev == logsink.Fatal {
					want = nil
				}
				got, err := os.ReadFile(sb.file.Name())
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s large-entry contents changed: %v", sev, err)
				}
				if sb.Size() != logFileBufferSize(platform) {
					t.Fatalf("%s large entry grew retained buffer", sev)
				}
			}
		})
	}
}

func TestLogFileRotationKeepsPlatformPolicyAndPendingBytes(t *testing.T) {
	setFlags()
	resetFileSink(t)
	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s := &sinks.file
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.createMissingFiles(logsink.Error); err != nil {
		t.Fatal(err)
	}
	for sev := logsink.Info; sev <= logsink.Error; sev++ {
		sb := s.file[sev].(*syncBuffer)
		if sb.Size() != logFileBufferSize(runtime.GOOS) {
			t.Fatalf("%s initial buffer has wrong platform capacity", sev)
		}
		oldFile := sb.file
		const before, after = "buffered-before-rotation\n", "buffered-after-rotation\n"
		if _, err := sb.Write([]byte(before)); err != nil {
			t.Fatal(err)
		}
		if err := sb.rotateFile(sb.madeAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if sb.Size() != logFileBufferSize(runtime.GOOS) {
			t.Fatalf("%s rotated buffer has wrong platform capacity", sev)
		}
		if _, err := oldFile.Write([]byte("must-be-closed")); err == nil {
			t.Fatal("rotation retained an open old file")
		}
		if _, err := sb.Write([]byte(after)); err != nil {
			t.Fatal(err)
		}
		if err := sb.Flush(); err != nil {
			t.Fatal(err)
		}
		first, err := os.ReadFile(oldFile.Name())
		if err != nil || bytes.Count(first, []byte(before)) != 1 || !bytes.Contains(first, []byte(footer)) {
			t.Fatalf("%s old pending entry/footer lost: %v", sev, err)
		}
		second, err := os.ReadFile(sb.file.Name())
		if err != nil || bytes.Count(second, []byte(after)) != 1 || !bytes.Contains(second, []byte(oldFile.Name())) {
			t.Fatalf("%s new entry/continuation header lost: %v", sev, err)
		}
	}
}

// Compare the changed buffering layer against the unchanged 256 KiB server
// default with real files. Both cases retain all bytes and flush at the same
// workload boundaries; the continuous case exposes extra mobile write calls.
func BenchmarkLogFileBufferPolicy(b *testing.B) {
	for _, size := range []int{32 * 1024, 256 * 1024} {
		for _, flushEvery := range []int{32, 0} {
			b.Run(fmt.Sprintf("%dKiB/flush-every-%d", size/1024, flushEvery), func(b *testing.B) {
				file, err := os.CreateTemp(b.TempDir(), "buffer-")
				if err != nil {
					b.Fatal(err)
				}
				defer file.Close()
				writer := bufio.NewWriterSize(file, size)
				line := bytes.Repeat([]byte("x"), 256)
				b.ReportAllocs()
				b.SetBytes(int64(len(line)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := writer.Write(line); err != nil {
						b.Fatal(err)
					}
					if flushEvery != 0 && (i+1)%flushEvery == 0 {
						if err := writer.Flush(); err != nil {
							b.Fatal(err)
						}
					}
					// Bound temporary disk usage at 4 MiB per fixture. This
					// boundary is a multiple of both buffer capacities.
					if (i+1)%(16*1024) == 0 {
						if err := writer.Flush(); err != nil {
							b.Fatal(err)
						}
						if _, err := file.Seek(0, 0); err != nil {
							b.Fatal(err)
						}
					}
				}
				if err := writer.Flush(); err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}

// Include the unchanged sink lock, severity routing, rotation check, and
// existing-file check to put the buffer-only cost in its logging context.
func BenchmarkFileSinkBufferPolicy(b *testing.B) {
	for _, platform := range []string{"android", "linux"} {
		for _, flushEvery := range []int{32, 0} {
			b.Run(fmt.Sprintf("%s/flush-every-%d", platform, flushEvery), func(b *testing.B) {
				s := newBufferPolicyTestSink(b, platform)
				sb := s.file[logsink.Info].(*syncBuffer)
				meta := &logsink.Meta{Severity: logsink.Info}
				line := bytes.Repeat([]byte("x"), 256)
				b.ReportAllocs()
				b.SetBytes(int64(len(line)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := s.Emit(meta, line); err != nil {
						b.Fatal(err)
					}
					if flushEvery != 0 && (i+1)%flushEvery == 0 {
						if err := sb.Flush(); err != nil {
							b.Fatal(err)
						}
					}
					if (i+1)%(16*1024) == 0 {
						if err := sb.Flush(); err != nil {
							b.Fatal(err)
						}
						if _, err := sb.file.Seek(0, 0); err != nil {
							b.Fatal(err)
						}
						sb.nbytes = 0
					}
				}
				if err := sb.Flush(); err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}
