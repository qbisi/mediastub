package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Local exposes a directory through os.Root so paths cannot escape it.
type Local struct {
	root    *os.Root
	rootDir *os.File
}

// NewLocal opens root as a read-only origin.
func NewLocal(root string) (*Local, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	dir, err := os.Open(root)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return &Local{root: r, rootDir: dir}, nil
}

func localError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("%w: %v", ErrNotDir, err)
	default:
		return err
	}
}

func localEntry(rel string, info os.FileInfo) (Entry, error) {
	if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
		return Entry{}, fmt.Errorf("%w: %s", ErrUnsupported, rel)
	}
	etag := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		etag = fmt.Sprintf("%x-%x-%s", stat.Dev, stat.Ino, etag)
	}
	return Entry{
		Path: rel, Name: path.Base(rel), Size: info.Size(), ModTime: info.ModTime(),
		IsDir: info.IsDir(), ETag: etag,
	}, nil
}

// Stat returns metadata for rel.
func (l *Local) Stat(ctx context.Context, rel string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	clean, err := CleanPath(rel)
	if err != nil {
		return Entry{}, err
	}
	info, err := l.root.Lstat(clean)
	if err != nil {
		return Entry{}, localError(err)
	}
	entry, err := localEntry(clean, info)
	if clean == "." {
		entry.Name = ""
	}
	return entry, err
}

// ReadDir lists the direct children of rel.
func (l *Local) ReadDir(ctx context.Context, rel string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean, err := CleanPath(rel)
	if err != nil {
		return nil, err
	}
	dirEntries, err := iofs.ReadDir(l.root.FS(), clean)
	if err != nil {
		return nil, localError(err)
	}
	entries := make([]Entry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		info, err := dirEntry.Info()
		if err != nil {
			return nil, localError(err)
		}
		child := dirEntry.Name()
		if clean != "." {
			child = path.Join(clean, child)
		}
		entry, err := localEntry(child, info)
		if errors.Is(err, ErrUnsupported) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Open opens a regular local file.
func (l *Local) Open(ctx context.Context, entry Entry) (Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.IsDir {
		return nil, ErrIsDir
	}
	f, err := l.root.Open(entry.Path)
	if err != nil {
		return nil, localError(err)
	}
	return &localObject{file: f}, nil
}

// Put atomically publishes a local object. Its parent directory must exist.
func (l *Local) Put(ctx context.Context, rel string, src io.Reader, size int64, _ string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	clean, err := CleanPath(rel)
	if err != nil {
		return Entry{}, err
	}
	if clean == "." || size < 0 {
		return Entry{}, errors.New("invalid local PUT target or size")
	}
	parent := path.Dir(clean)
	if info, err := l.root.Stat(parent); err != nil || !info.IsDir() {
		if err == nil {
			err = ErrNotDir
		}
		return Entry{}, fmt.Errorf("local PUT parent %q: %w", parent, localError(err))
	}
	tmp := path.Join(parent, "."+path.Base(clean)+".mediastub-tmp-"+strings.ReplaceAll(fmt.Sprintf("%d", time.Now().UnixNano()), "-", ""))
	f, err := l.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Entry{}, localError(err)
	}
	cleanup := func() { _ = l.root.Remove(tmp) }
	written, copyErr := io.Copy(f, io.LimitReader(src, size+1))
	if copyErr == nil && written != size {
		copyErr = fmt.Errorf("local PUT copied %d bytes, want %d", written, size)
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		cleanup()
		return Entry{}, copyErr
	}
	// os.Root.Rename was added after the project's Go 1.24 baseline. renameat
	// preserves atomic same-origin publication even if the root itself is moved.
	if err := unix.Renameat(int(l.rootDir.Fd()), tmp, int(l.rootDir.Fd()), clean); err != nil {
		cleanup()
		return Entry{}, localError(err)
	}
	if dir, err := l.root.Open(parent); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return l.Stat(ctx, clean)
}

// Close closes the protected root.
func (l *Local) Close() error { return errors.Join(l.root.Close(), l.rootDir.Close()) }

type localObject struct {
	file *os.File
}

func (o *localObject) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return o.file.ReadAt(p, off)
}

func (o *localObject) Close() error { return o.file.Close() }

var _ MutableOrigin = (*Local)(nil)
