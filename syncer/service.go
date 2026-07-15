package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qbisi/mediastub/origin"
	"github.com/qbisi/mediastub/pathfilter"
)

type Service struct {
	origin   origin.Origin
	mutable  origin.MutableOrigin
	config   Config
	matcher  *pathfilter.Matcher
	store    *stateStore
	state    *State
	snapshot *remoteSnapshot
}

func (s *Service) logf(level, format string, args ...any) {
	priority := map[string]int{"info": 0, "verbose": 1, "debug": 2}
	if priority[level] <= priority[s.config.LogLevel] {
		s.config.Logger.Printf(format, args...)
	}
}

// Run performs one reconcile. In daemon mode it then watches and polls until
// ctx ends. ready is called after the initial inputs have been scanned and the
// work set has been planned, but before media and sidecar I/O begins.
func (s *Service) Run(ctx context.Context, ready func(string) error) error {
	if info, err := os.Stat(s.config.LocalRoot); err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return fmt.Errorf("local directory %q: %w", s.config.LocalRoot, err)
	}
	store, state, err := openStateStore(s.config.StateDir, s.config.Remote, s.config.LocalRoot)
	if err != nil {
		return err
	}
	s.store, s.state = store, state
	defer store.Close()

	var events <-chan struct{}
	var closeWatcher func() error
	if s.config.Daemon {
		events, closeWatcher, err = watchTree(ctx, s.config.LocalRoot, s.config.Logger, s.config.LogLevel)
		if err != nil {
			return err
		}
		defer closeWatcher()
	}
	local, err := s.scanInputs(ctx, true)
	if err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}
	if ready != nil {
		media, sidecars := s.planWork(local)
		s.logf("info", "initial synchronization planned media=%d sidecars=%d", media, sidecars)
		if err := ready(fmt.Sprintf("Initial synchronization planned: media=%d sidecars=%d", media, sidecars)); err != nil {
			return err
		}
	}
	applyErr := s.applyReconcile(ctx, local, true)
	if !s.config.Daemon {
		if applyErr != nil {
			return fmt.Errorf("initial reconcile: %w", applyErr)
		}
		return nil
	}
	if applyErr != nil {
		s.logf("info", "initial reconcile failed after readiness: %v", applyErr)
	}

	poll := time.NewTicker(s.config.PollInterval)
	defer poll.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return s.store.Save(s.state)
		case <-events:
			debounce = time.After(s.config.SettleTime)
		case <-debounce:
			debounce = nil
			if err := s.reconcile(ctx, false); err != nil {
				s.logf("info", "local reconcile failed: %v", err)
			}
		case <-poll.C:
			if err := s.reconcile(ctx, true); err != nil {
				s.logf("info", "remote reconcile failed: %v", err)
			}
		}
	}
}

func (s *Service) reconcile(ctx context.Context, refreshRemote bool) error {
	local, err := s.scanInputs(ctx, refreshRemote)
	if err != nil {
		return err
	}
	return s.applyReconcile(ctx, local, refreshRemote)
}

func (s *Service) scanInputs(ctx context.Context, refreshRemote bool) (map[string]localFile, error) {
	if refreshRemote || s.snapshot == nil {
		snapshot, err := scanRemote(ctx, s.origin)
		if err != nil {
			return nil, err
		}
		s.snapshot = snapshot
		s.logf("verbose", "remote scan complete entries=%d duplicate_paths=%d", len(snapshot.Entries), len(snapshot.Duplicates))
		for rel, objects := range snapshot.Duplicates {
			for _, entry := range objects {
				s.logf("info", "duplicate-remote-path path=%q etag=%q size=%d mtime=%s", rel, entry.ETag, entry.Size, entry.ModTime.Format(time.RFC3339Nano))
			}
		}
	}
	local, err := scanLocal(s.config.LocalRoot)
	if err != nil {
		return nil, err
	}
	s.logf("verbose", "local scan complete files=%d", len(local))
	return local, nil
}

func (s *Service) applyReconcile(ctx context.Context, local map[string]localFile, remoteScan bool) error {
	mediaErr := s.reconcileMediaFiles(ctx, local, remoteScan)
	// Persist ownership of newly published stubs before sidecar I/O. Otherwise
	// a later failure could make a valid stub look like an untracked collision.
	if err := s.store.Save(s.state); err != nil {
		return errors.Join(mediaErr, err)
	}
	local, err := scanLocal(s.config.LocalRoot)
	if err != nil {
		return errors.Join(mediaErr, err)
	}
	sidecarErr := s.reconcileSidecars(ctx, local)
	return errors.Join(mediaErr, sidecarErr, s.store.Save(s.state))
}

func (s *Service) planWork(local map[string]localFile) (mediaCount, sidecarCount int) {
	mediaPaths, mediaCount := s.planMediaWork(local)
	remoteSidecars := make(map[string]origin.Entry)
	localSidecars := make(map[string]localFile)
	candidates := make(map[string]bool)
	for rel, entry := range s.snapshot.Entries {
		if entry.IsDir || s.matcher.Match(rel) {
			continue
		}
		match := ClassifySidecar(rel, mediaPaths)
		if !match.Ambiguous && match.MediaPath != "" {
			remoteSidecars[rel] = entry
			candidates[rel] = true
		}
	}
	for rel := range local {
		if s.matcher.Match(rel) {
			continue
		}
		match := ClassifySidecar(rel, mediaPaths)
		if !match.Ambiguous && match.MediaPath != "" {
			localSidecars[rel] = local[rel]
			candidates[rel] = true
		}
	}
	for rel := range s.state.Sidecars {
		candidates[rel] = true
	}
	for rel := range candidates {
		if s.duplicateBlocked(rel) {
			continue
		}
		lf, localExists := localSidecars[rel]
		remoteEntry, remoteExists := remoteSidecars[rel]
		previous, known := s.state.Sidecars[rel]
		needsWork := false
		switch {
		case localExists:
			localChanged := !known || previous.LocalSize != lf.Size || !previous.LocalMTime.Equal(time.Unix(0, lf.ModTime)) || previous.Status == "local-dirty" || previous.Status == "upload-failed"
			remoteChanged := !remoteExists || !known || previous.RemoteETag != remoteEntry.ETag || previous.RemoteSize != remoteEntry.Size || !previous.RemoteMTime.Equal(remoteEntry.ModTime)
			needsWork = localChanged || remoteChanged || (remoteExists && remoteEntry.ETag == "")
		case remoteExists || known:
			needsWork = true
		}
		if needsWork {
			sidecarCount++
			s.logf("debug", "planned sidecar path=%q", rel)
		}
	}
	return mediaCount, sidecarCount
}

func (s *Service) duplicateBlocked(rel string) bool {
	for duplicate, entries := range s.snapshot.Duplicates {
		if rel == duplicate {
			return true
		}
		if !strings.HasPrefix(rel, duplicate+"/") {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir {
				return true
			}
		}
	}
	return false
}

func (s *Service) trackedMediaPaths() []string {
	var paths []string
	for rel, media := range s.state.Media {
		if media.Managed {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	return paths
}

func (s *Service) reconcileSidecars(ctx context.Context, local map[string]localFile) error {
	var actionErrors []error
	mediaPaths := s.trackedMediaPaths()
	remoteSidecars := make(map[string]origin.Entry)
	localSidecars := make(map[string]localFile)
	mediaFor := make(map[string]string)
	for rel, entry := range s.snapshot.Entries {
		if entry.IsDir || s.matcher.Match(rel) {
			continue
		}
		match := ClassifySidecar(rel, mediaPaths)
		if match.Ambiguous {
			s.logf("info", "ambiguous-sidecar path=%q", rel)
			continue
		}
		if match.MediaPath != "" {
			remoteSidecars[rel] = entry
			mediaFor[rel] = match.MediaPath
		}
	}
	for rel, file := range local {
		if s.matcher.Match(rel) {
			continue
		}
		match := ClassifySidecar(rel, mediaPaths)
		if match.Ambiguous {
			s.logf("info", "ambiguous-sidecar path=%q", rel)
			continue
		}
		if match.MediaPath != "" {
			localSidecars[rel] = file
			mediaFor[rel] = match.MediaPath
		}
	}
	all := make(map[string]bool)
	for rel := range remoteSidecars {
		all[rel] = true
	}
	for rel := range localSidecars {
		all[rel] = true
	}
	for rel := range s.state.Sidecars {
		all[rel] = true
	}
	paths := make([]string, 0, len(all))
	for rel := range all {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		if s.duplicateBlocked(rel) {
			continue
		}
		lf, localExists := localSidecars[rel]
		remoteEntry, remoteExists := remoteSidecars[rel]
		previous, known := s.state.Sidecars[rel]
		if localExists {
			localHash, stableFile, err := s.stableLocalHash(ctx, rel, lf)
			if err != nil {
				s.logf("info", "sidecar remains dirty path=%q error=%v", rel, err)
				actionErrors = append(actionErrors, fmt.Errorf("settle sidecar %q: %w", rel, err))
				continue
			}
			needUpload := !remoteExists
			if !known && remoteExists {
				remoteHash, err := hashRemote(ctx, s.origin, remoteEntry)
				if err != nil {
					return err
				}
				needUpload = remoteHash != localHash
			}
			if known {
				remoteChanged := !remoteExists || previous.RemoteETag != remoteEntry.ETag || previous.RemoteSize != remoteEntry.Size || !previous.RemoteMTime.Equal(remoteEntry.ModTime)
				if remoteExists && !remoteChanged && remoteEntry.ETag == "" {
					remoteHash, err := hashRemote(ctx, s.origin, remoteEntry)
					if err != nil {
						return err
					}
					remoteChanged = remoteHash != previous.LastUploadedSHA256
				}
				needUpload = needUpload || previous.LastUploadedSHA256 != localHash || remoteChanged || previous.Status == "local-dirty" || previous.Status == "upload-failed"
			}
			if needUpload {
				entry, err := s.uploadAndVerify(ctx, rel, stableFile, localHash)
				if err != nil {
					s.state.Sidecars[rel] = SidecarState{LocalSHA256: localHash, LocalSize: stableFile.Size, LocalMTime: time.Unix(0, stableFile.ModTime), LastUploadedSHA256: previous.LastUploadedSHA256, RemoteETag: previous.RemoteETag, RemoteSize: previous.RemoteSize, RemoteMTime: previous.RemoteMTime, MediaPath: mediaFor[rel], Status: "upload-failed", LastError: err.Error()}
					s.logf("info", "sidecar upload failed path=%q error=%v", rel, err)
					actionErrors = append(actionErrors, fmt.Errorf("upload sidecar %q: %w", rel, err))
					continue
				}
				remoteEntry, remoteExists = entry, true
				s.snapshot.Entries[rel] = entry
				s.logf("info", "sidecar uploaded path=%q size=%d", rel, stableFile.Size)
			}
			lastUploaded := previous.LastUploadedSHA256
			if needUpload || !known {
				lastUploaded = localHash
			}
			s.state.Sidecars[rel] = SidecarState{LocalSHA256: localHash, LocalSize: stableFile.Size, LocalMTime: time.Unix(0, stableFile.ModTime), LastUploadedSHA256: lastUploaded, RemoteETag: remoteEntry.ETag, RemoteSize: remoteEntry.Size, RemoteMTime: remoteEntry.ModTime, MediaPath: mediaFor[rel], Status: "synchronized"}
			continue
		}
		if remoteExists {
			state, err := s.downloadSidecar(ctx, rel, remoteEntry)
			if err != nil {
				previous.Status, previous.LastError = "download-failed", err.Error()
				s.state.Sidecars[rel] = previous
				actionErrors = append(actionErrors, err)
				continue
			}
			state.MediaPath = mediaFor[rel]
			s.state.Sidecars[rel] = state
			s.logf("info", "sidecar restored from remote path=%q media=%q size=%d remote_etag=%q", rel, mediaFor[rel], remoteEntry.Size, remoteEntry.ETag)
		} else if known {
			delete(s.state.Sidecars, rel)
		}
	}
	return errors.Join(actionErrors...)
}

func hashLocalFile(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashRemote(ctx context.Context, upstream origin.Origin, entry origin.Entry) (string, error) {
	object, err := upstream.Open(ctx, entry)
	if err != nil {
		return "", err
	}
	defer object.Close()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for off := int64(0); off < entry.Size; {
		n := int64(len(buf))
		if n > entry.Size-off {
			n = entry.Size - off
		}
		read, err := origin.ReadFullAt(ctx, object, buf[:n], off)
		if read > 0 {
			_, _ = h.Write(buf[:read])
			off += int64(read)
		}
		if err != nil && !(errors.Is(err, io.EOF) && off == entry.Size) {
			return "", err
		}
		if read == 0 && off < entry.Size {
			return "", io.ErrNoProgress
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) stableLocalHash(ctx context.Context, rel string, initial localFile) (string, localFile, error) {
	filename := filepath.Join(s.config.LocalRoot, filepath.FromSlash(rel))
	previous := initial
	deadline := time.Now().Add(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return "", localFile{}, ctx.Err()
		case <-time.After(s.config.SettleTime):
		}
		current, err := statLocal(s.config.LocalRoot, rel)
		if err != nil {
			return "", localFile{}, err
		}
		if sameLocalFile(current, previous) {
			hash, err := hashLocalFile(filename)
			return hash, current, err
		}
		if time.Now().After(deadline) {
			return "", current, errors.New("file did not settle within 60 seconds")
		}
		previous = current
	}
}

func (s *Service) uploadAndVerify(ctx context.Context, rel string, file localFile, _ string) (origin.Entry, error) {
	filename := filepath.Join(s.config.LocalRoot, filepath.FromSlash(rel))
	f, err := os.Open(filename)
	if err != nil {
		return origin.Entry{}, err
	}
	entry, putErr := s.mutable.Put(ctx, rel, f, file.Size, sidecarContentType(rel))
	closeErr := f.Close()
	if putErr != nil {
		return origin.Entry{}, putErr
	}
	if closeErr != nil {
		return origin.Entry{}, closeErr
	}
	if entry.Size != file.Size || entry.ETag == "" {
		return origin.Entry{}, fmt.Errorf("verify sidecar upload: size=%d want=%d etag_present=%t", entry.Size, file.Size, entry.ETag != "")
	}
	return entry, nil
}

func sidecarContentType(rel string) string {
	ext := strings.ToLower(path.Ext(rel))
	if contentType := mime.TypeByExtension(ext); contentType != "" {
		return contentType
	}
	switch ext {
	case ".nfo":
		return "application/xml"
	case ".srt":
		return "application/x-subrip"
	case ".ass", ".ssa":
		return "text/x-ssa"
	case ".vtt":
		return "text/vtt"
	case ".sub":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

func (s *Service) downloadSidecar(ctx context.Context, rel string, entry origin.Entry) (SidecarState, error) {
	object, err := s.origin.Open(ctx, entry)
	if err != nil {
		return SidecarState{}, err
	}
	source := &originSource{ctx: ctx, object: object, size: entry.Size}
	h := sha256.New()
	reader := io.TeeReader(io.NewSectionReader(source, 0, entry.Size), h)
	err = writeLocalAtomic(ctx, s.config.LocalRoot, rel, reader, entry.Size, 0o664, entry.ModTime)
	closeErr := object.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return SidecarState{}, err
	}
	info, err := os.Stat(filepath.Join(s.config.LocalRoot, filepath.FromSlash(rel)))
	if err != nil {
		return SidecarState{}, err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	return SidecarState{LocalSHA256: hash, LocalSize: info.Size(), LocalMTime: info.ModTime(), LastUploadedSHA256: hash, RemoteETag: entry.ETag, RemoteSize: entry.Size, RemoteMTime: entry.ModTime, Status: "synchronized"}, nil
}
