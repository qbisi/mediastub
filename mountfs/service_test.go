package mountfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/origin"
)

func TestServiceCachesSuccessfulPlan(t *testing.T) {
	data := minimalMP4()
	upstream := newMemoryOrigin("movie.mp4", data)
	service, err := NewService(upstream, Config{Includes: []string{"*.mp4"}, Budget: core.DefaultBudget})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := upstream.Stat(context.Background(), "movie.mp4")
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		view, stubbed, err := service.OpenView(context.Background(), entry, true)
		if err != nil {
			t.Fatal(err)
		}
		if !stubbed {
			t.Fatal("eligible MP4 did not return a stub view")
		}
		buf := make([]byte, view.Size())
		if _, err := view.ReadAt(context.Background(), buf, 0); err != nil {
			t.Fatal(err)
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
		if string(buf[16:20]) != "mdat" {
			t.Fatalf("missing mdat header in projected view: %x", buf)
		}
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.opens != 1 {
		t.Fatalf("origin opens = %d, want one probe open", upstream.opens)
	}
	if upstream.closes != 1 {
		t.Fatalf("origin closes = %d, want one probe close", upstream.closes)
	}
}

func TestServiceLogsProbeCompletionTime(t *testing.T) {
	upstream := newMemoryOrigin("movie.mp4", minimalMP4())
	var output bytes.Buffer
	service, err := NewService(upstream, Config{
		Includes: []string{"*.mp4"}, Budget: core.DefaultBudget,
		Logger: log.New(&output, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := upstream.Stat(context.Background(), "movie.mp4")
	if err != nil {
		t.Fatal(err)
	}
	view, stubbed, err := service.OpenView(context.Background(), entry, true)
	if err != nil {
		t.Fatal(err)
	}
	if !stubbed {
		t.Fatal("eligible MP4 did not return a stub view")
	}
	defer view.Close()

	line := strings.TrimSpace(output.String())
	const marker = " probe_time="
	index := strings.LastIndex(line, marker)
	if index < 0 {
		t.Fatalf("stub completion log has no probe_time: %q", line)
	}
	duration := strings.TrimSpace(line[index+len(marker):])
	if _, err := time.ParseDuration(duration); err != nil {
		t.Fatalf("invalid probe_time %q in log %q: %v", duration, line, err)
	}
}

func TestServicePassthroughForNonMatch(t *testing.T) {
	data := []byte("original jpeg bytes")
	upstream := newMemoryOrigin("poster.jpg", data)
	service, err := NewService(upstream, Config{Includes: []string{"*.mkv"}})
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := upstream.Stat(context.Background(), "poster.jpg")
	view, stubbed, err := service.OpenView(context.Background(), entry, true)
	if err != nil {
		t.Fatal(err)
	}
	if stubbed {
		t.Fatal("non-matching file returned a stub view")
	}
	defer view.Close()
	buf := make([]byte, len(data))
	if _, err := view.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(data) {
		t.Fatalf("passthrough bytes = %q", buf)
	}
}

func TestServiceFailPolicy(t *testing.T) {
	upstream := newMemoryOrigin("broken.mp4", []byte("not an mp4"))
	service, err := NewService(upstream, Config{Includes: []string{"*.mp4"}, OnError: "fail"})
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := upstream.Stat(context.Background(), "broken.mp4")
	if _, _, err := service.OpenView(context.Background(), entry, true); !errors.Is(err, core.ErrNotMedia) {
		t.Fatalf("OpenView error = %v, want ErrNotMedia", err)
	}
}

func TestServiceProcessPassthroughSkipsProbe(t *testing.T) {
	data := minimalMP4()
	upstream := newMemoryOrigin("movie.mp4", data)
	service, err := NewService(upstream, Config{Includes: []string{"*.mp4"}, Budget: core.DefaultBudget})
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := upstream.Stat(context.Background(), "movie.mp4")
	view, stubbed, err := service.OpenView(context.Background(), entry, false)
	if err != nil {
		t.Fatal(err)
	}
	if stubbed {
		t.Fatal("passthrough-only open returned a stub")
	}
	buf := make([]byte, len(data))
	if _, err := view.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(data) {
		t.Fatal("passthrough-only open changed source bytes")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.opens != 1 {
		t.Fatalf("origin opens = %d, want one passthrough open and no probe", upstream.opens)
	}
}

func TestServiceRejectsInvalidIncludePattern(t *testing.T) {
	if _, err := NewService(newMemoryOrigin("movie.mp4", minimalMP4()), Config{Includes: []string{"["}}); err == nil {
		t.Fatal("invalid include pattern was accepted")
	}
}

func minimalMP4() []byte {
	box := func(typ string, payload []byte) []byte {
		out := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(out, uint32(len(out)))
		copy(out[4:8], typ)
		copy(out[8:], payload)
		return out
	}
	data := box("ftyp", []byte("isom"))
	data = append(data, box("mdat", []byte{1, 2, 3, 4})...)
	return append(data, box("moov", nil)...)
}

type memoryOrigin struct {
	mu     sync.Mutex
	entry  origin.Entry
	data   []byte
	opens  int
	closes int
}

func newMemoryOrigin(name string, data []byte) *memoryOrigin {
	return &memoryOrigin{
		entry: origin.Entry{Path: name, Name: name, Size: int64(len(data)), ModTime: time.Unix(1, 0), ETag: "v1"},
		data:  append([]byte(nil), data...),
	}
}

func (o *memoryOrigin) Stat(_ context.Context, name string) (origin.Entry, error) {
	if name != o.entry.Path {
		return origin.Entry{}, origin.ErrNotFound
	}
	return o.entry, nil
}

func (*memoryOrigin) ReadDir(context.Context, string) ([]origin.Entry, error) {
	return nil, origin.ErrNotDir
}

func (o *memoryOrigin) Open(_ context.Context, entry origin.Entry) (origin.Object, error) {
	if entry.Path != o.entry.Path {
		return nil, origin.ErrNotFound
	}
	o.mu.Lock()
	o.opens++
	o.mu.Unlock()
	return &memoryObject{origin: o}, nil
}

func (*memoryOrigin) Close() error { return nil }

type memoryObject struct{ origin *memoryOrigin }

func (o *memoryObject) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(o.origin.data)) {
		return 0, io.EOF
	}
	n := copy(p, o.origin.data[off:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (o *memoryObject) Close() error {
	o.origin.mu.Lock()
	o.origin.closes++
	o.origin.mu.Unlock()
	return nil
}
