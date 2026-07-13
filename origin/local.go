package origin

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path"
	"syscall"
)

// Local exposes a directory through os.Root so paths cannot escape it.
type Local struct {
	root *os.Root
}

// NewLocal opens root as a read-only origin.
func NewLocal(root string) (*Local, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return &Local{root: r}, nil
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

// Close closes the protected root.
func (l *Local) Close() error { return l.root.Close() }

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
