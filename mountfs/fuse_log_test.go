package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/origin"
)

func TestParseLogLevel(t *testing.T) {
	for _, test := range []struct {
		name  string
		level LogLevel
		all   bool
		debug bool
	}{
		{name: "info", level: LogLevelInfo},
		{name: "verbose", level: LogLevelVerbose, all: true},
		{name: "debug", level: LogLevelDebug, all: true, debug: true},
	} {
		level, err := ParseLogLevel(test.name)
		if err != nil {
			t.Fatalf("ParseLogLevel(%q): %v", test.name, err)
		}
		if level != test.level || level.logAllAccesses() != test.all || level.debugFUSE() != test.debug {
			t.Errorf("ParseLogLevel(%q) = %v (all=%v debug=%v)", test.name, level, level.logAllAccesses(), level.debugFUSE())
		}
	}
	if _, err := ParseLogLevel("trace"); err == nil {
		t.Fatal("invalid log level was accepted")
	}
}

func TestLogErrorSuppressesNegativeLookup(t *testing.T) {
	var output bytes.Buffer
	n := &node{path: ".", logger: log.New(&output, "", 0)}
	n.logError("lookup .git", fmt.Errorf("%w: 404 Not Found", origin.ErrNotFound))
	if output.Len() != 0 {
		t.Fatalf("negative lookup was logged: %q", output.String())
	}
	n.logError("lookup movie.mkv", errors.New("backend unavailable"))
	if got := output.String(); !strings.Contains(got, `lookup movie.mkv path="." error=backend unavailable`) {
		t.Fatalf("real backend error was not logged: %q", got)
	}
}

func TestAccessLoggingLevels(t *testing.T) {
	tests := []struct {
		name     string
		level    LogLevel
		filename string
		data     []byte
		wantLog  bool
		want     string
	}{
		{
			name: "info included", level: LogLevelInfo, filename: "movie.mp4", data: minimalMP4(), wantLog: true,
			want: `access pid=42 uid=1000 gid=100 process="ffprobe" path="movie.mp4" include=true route=stub`,
		},
		{name: "info excluded", level: LogLevelInfo, filename: "poster.jpg", data: []byte("jpeg")},
		{
			name: "verbose excluded", level: LogLevelVerbose, filename: "poster.jpg", data: []byte("jpeg"), wantLog: true,
			want: `access pid=42 uid=1000 gid=100 process="ffprobe" path="poster.jpg" include=false route=passthrough`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newMemoryOrigin(test.filename, test.data)
			service, err := NewService(upstream, Config{Includes: []string{"*.mp4"}, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
			if err != nil {
				t.Fatal(err)
			}
			entry, err := upstream.Stat(context.Background(), test.filename)
			if err != nil {
				t.Fatal(err)
			}
			matcher := testProcessMatcher(t, 42, "ffprobe")
			var output bytes.Buffer
			n := &node{
				service: service, path: test.filename, attrTTL: time.Second,
				logger: log.New(&output, "", 0), processes: matcher, logLevel: test.level,
			}
			n.cacheStat(entry, time.Now().Add(time.Minute))
			ctx := fuse.NewContext(context.Background(), &fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}, Pid: 42})
			handle, _, errno := n.Open(ctx, syscall.O_RDONLY)
			if errno != 0 {
				t.Fatalf("Open errno = %v", errno)
			}
			if errno := handle.(*fileHandle).Release(context.Background()); errno != 0 {
				t.Fatalf("Release errno = %v", errno)
			}
			got := strings.TrimSpace(output.String())
			if test.wantLog && got != test.want {
				t.Fatalf("access log = %q, want %q", got, test.want)
			}
			if !test.wantLog && got != "" {
				t.Fatalf("unexpected access log: %q", got)
			}
		})
	}
}

func TestAccessLoggingUIDMatchShortCircuitsComm(t *testing.T) {
	upstream := newMemoryOrigin("movie.mp4", minimalMP4())
	service, err := NewService(upstream, Config{
		Includes: []string{"*.mp4"}, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := upstream.Stat(context.Background(), "movie.mp4")
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := newProcessMatcher([]string{"other-process"}, []uint32{1000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	matcher.procRoot = t.TempDir()
	var output bytes.Buffer
	n := &node{
		service: service, path: "movie.mp4", attrTTL: time.Second,
		logger: log.New(&output, "", 0), processes: matcher, logLevel: LogLevelInfo,
	}
	n.cacheStat(entry, time.Now().Add(time.Minute))
	ctx := fuse.NewContext(context.Background(), &fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}, Pid: 99})
	handle, _, errno := n.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	if errno := handle.(*fileHandle).Release(context.Background()); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
	want := `access pid=99 uid=1000 gid=100 process="" path="movie.mp4" include=true route=stub`
	if got := strings.TrimSpace(output.String()); got != want {
		t.Fatalf("access log = %q, want %q", got, want)
	}
}

func testProcessMatcher(t *testing.T, pid uint32, comm string) *processMatcher {
	t.Helper()
	procRoot := t.TempDir()
	dir := filepath.Join(procRoot, fmt.Sprint(pid))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matcher, err := newProcessMatcher([]string{"ffprobe"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	matcher.procRoot = procRoot
	return matcher
}
