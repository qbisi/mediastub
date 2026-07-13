// Package origin defines the namespace and random-read boundary used by mediastub.
package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	ErrNotFound    = errors.New("origin entry not found")
	ErrNotDir      = errors.New("origin entry is not a directory")
	ErrIsDir       = errors.New("origin entry is a directory")
	ErrUnsupported = errors.New("unsupported origin entry")
)

// Entry describes one object or directory in an Origin.
type Entry struct {
	Path        string
	Name        string
	Size        int64
	ModTime     time.Time
	IsDir       bool
	ETag        string
	ContentType string
}

// Fingerprint identifies the exact object version used by a cached Plan.
func (e Entry) Fingerprint() string {
	return fmt.Sprintf("%s\x00%d\x00%d\x00%s", e.Path, e.Size, e.ModTime.UnixNano(), e.ETag)
}

// Object is a context-aware random-access object.
type Object interface {
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	Close() error
}

// Origin provides a read-only namespace and random object access.
type Origin interface {
	Stat(ctx context.Context, path string) (Entry, error)
	ReadDir(ctx context.Context, path string) ([]Entry, error)
	Open(ctx context.Context, entry Entry) (Object, error)
	Close() error
}

// ReadFullAt normalizes short ranged reads to io.ReaderAt semantics.
func ReadFullAt(ctx context.Context, object Object, p []byte, off int64) (int, error) {
	read := 0
	for read < len(p) {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		n, err := object.ReadAt(ctx, p[read:], off+int64(read))
		read += n
		if err != nil {
			if err == io.EOF && read == len(p) {
				return read, nil
			}
			return read, err
		}
		if n == 0 {
			return read, io.ErrNoProgress
		}
	}
	return read, nil
}

// CleanPath converts a user path into the slash-separated relative namespace.
func CleanPath(name string) (string, error) {
	name = strings.Trim(name, "/")
	if name == "" || name == "." {
		return ".", nil
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid relative path %q", name)
		}
	}
	return strings.Join(parts, "/"), nil
}
