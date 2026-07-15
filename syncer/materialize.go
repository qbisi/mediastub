package syncer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/marker"
	"github.com/qbisi/mediastub/origin"
)

type originSource struct {
	ctx    context.Context
	object origin.Object
	size   int64
}

func (s *originSource) Size() int64 { return s.size }
func (s *originSource) ReadAt(p []byte, off int64) (int, error) {
	return origin.ReadFullAt(s.ctx, s.object, p, off)
}

func probeEntry(ctx context.Context, upstream origin.Origin, entry origin.Entry, budget core.Budget) (*core.Result, error) {
	object, err := upstream.Open(ctx, entry)
	if err != nil {
		return nil, err
	}
	result, probeErr := core.Probe(&originSource{ctx: ctx, object: object, size: entry.Size}, budget)
	closeErr := object.Close()
	if probeErr == nil {
		probeErr = closeErr
	}
	if probeErr != nil {
		return nil, probeErr
	}
	return result, nil
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func materializePlan(ctx context.Context, root, rel string, result *core.Result, remoteETag string, mtime time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o775); err != nil {
		return err
	}
	tmp := filepath.Join(parent, fmt.Sprintf(".%s.mediastub-new-%d-%s", filepath.Base(target), os.Getpid(), randomSuffix()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	trailer, err := marker.Trailer(result.Format, result.Plan.Size(), remoteETag, result.Plan.Hash())
	if err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err = f.Truncate(result.Plan.Size()); err == nil {
		for _, extent := range result.Plan.Extents() {
			if err = ctx.Err(); err != nil {
				break
			}
			var n int
			n, err = f.WriteAt(extent.Data, extent.Offset)
			if err == nil && n != len(extent.Data) {
				err = io.ErrShortWrite
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		var n int
		n, err = f.WriteAt(trailer, result.Plan.Size())
		if err == nil && n != len(trailer) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Chmod(0o444)
	}
	if err == nil && !mtime.IsZero() {
		err = os.Chtimes(tmp, mtime, mtime)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		cleanup()
		return err
	}
	return syncDirectory(parent)
}

func writeLocalAtomic(ctx context.Context, root, rel string, src io.Reader, size int64, mode os.FileMode, mtime time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o775); err != nil {
		return err
	}
	tmp := filepath.Join(parent, "."+filepath.Base(target)+".mediastub-tmp-"+randomSuffix())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	written, err := io.Copy(f, src)
	if err == nil && written != size {
		err = fmt.Errorf("downloaded %d bytes, want %d", written, size)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil && !mtime.IsZero() {
		err = os.Chtimes(tmp, mtime, mtime)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		cleanup()
		return err
	}
	return syncDirectory(parent)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
