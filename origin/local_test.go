package origin

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalNamespaceAndRead(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "season"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("0123456789")
	if err := os.WriteFile(filepath.Join(root, "season", "episode.mkv"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })

	entries, err := local.ReadDir(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "season" || !entries[0].IsDir {
		t.Fatalf("unexpected root entries: %+v", entries)
	}
	entry, err := local.Stat(context.Background(), "season/episode.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != int64(len(want)) || entry.IsDir {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	object, err := local.Open(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	buf := make([]byte, 4)
	n, err := object.ReadAt(context.Background(), buf, 3)
	if err != nil || n != len(buf) || string(buf) != "3456" {
		t.Fatalf("ReadAt = %q, %d, %v", buf, n, err)
	}
}

func TestLocalRejectsTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })

	if _, err := local.Stat(context.Background(), "../outside"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := local.Stat(context.Background(), "escape"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("symlink Stat error = %v, want ErrUnsupported", err)
	}
	entries, err := local.ReadDir(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink leaked into directory: %+v", entries)
	}

	object := &shortObject{}
	buf := make([]byte, 2)
	if n, err := ReadFullAt(context.Background(), object, buf, 0); n != 1 || !errors.Is(err, io.EOF) {
		t.Fatalf("short ReadFullAt = %d, %v", n, err)
	}
}

type shortObject struct{ read bool }

func (o *shortObject) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	if o.read {
		return 0, io.EOF
	}
	o.read = true
	p[0] = 'x'
	return 1, nil
}

func (*shortObject) Close() error { return nil }
