package syncer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/marker"
	"github.com/qbisi/mediastub/origin"
)

const (
	mediaRemoteStub      = "remote-stub"
	mediaPendingUpload   = "pending-upload"
	mediaUploading       = "uploading"
	mediaUploadFailed    = "upload-failed"
	mediaVerifyFailed    = "verify-failed"
	mediaProbeFailed     = "probe-failed"
	mediaStubFailed      = "stub-generation-failed"
	mediaInvalidMarker   = "invalid-marker"
	mediaRemoteMissing   = "remote-missing"
	mediaDuplicateRemote = "duplicate-remote-path"
)

type fileSource struct {
	file *os.File
	size int64
}

func (s *fileSource) Size() int64                             { return s.size }
func (s *fileSource) ReadAt(p []byte, off int64) (int, error) { return s.file.ReadAt(p, off) }

func inspectLocalMarker(root, rel string, file localFile) (marker.Result, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return marker.Result{}, err
	}
	result, inspectErr := marker.Inspect(f, file.Size)
	closeErr := f.Close()
	return result, errors.Join(inspectErr, closeErr)
}

func legacyTrackedStub(previous MediaState, file localFile, remote origin.Entry) bool {
	return previous.Managed && file.Mode.Perm() == 0o444 && file.Size == remote.Size &&
		previous.Size == remote.Size && time.Unix(0, file.ModTime).Equal(previous.LocalMTime)
}

func markerMatchesRemote(found marker.Marker, remote origin.Entry) bool {
	return found.RemoteSize == uint64(remote.Size) && found.RemoteETagHash == marker.ETagHash(remote.ETag)
}

func planHashString(hash [32]byte) string { return hex.EncodeToString(hash[:]) }

func (s *Service) recordRemoteStub(rel string, remote origin.Entry, local localFile, found marker.Marker, now time.Time) {
	s.state.Media[rel] = MediaState{
		Managed: true, Fingerprint: remote.Fingerprint(), ETag: remote.ETag, Size: remote.Size,
		RemoteMTime: remote.ModTime, LocalMTime: time.Unix(0, local.ModTime), LocalSize: local.Size,
		MarkerVersion: found.Version, PlanHash: planHashString(found.PlanHash), LastSeen: now,
		Status: mediaRemoteStub,
	}
}

func (s *Service) materializeRemote(ctx context.Context, rel string, remote origin.Entry, now time.Time) error {
	started := time.Now()
	result, err := probeEntry(ctx, s.origin, remote, s.config.Budget)
	if err != nil {
		previous := s.state.Media[rel]
		previous.Status, previous.LastError = mediaProbeFailed, err.Error()
		s.state.Media[rel] = previous
		return fmt.Errorf("probe remote media %q: %w", rel, err)
	}
	if err := materializePlan(ctx, s.config.LocalRoot, rel, result, remote.ETag, remote.ModTime); err != nil {
		previous := s.state.Media[rel]
		previous.Status, previous.LastError = mediaStubFailed, err.Error()
		s.state.Media[rel] = previous
		return fmt.Errorf("materialize %q: %w", rel, err)
	}
	local, err := statLocal(s.config.LocalRoot, rel)
	if err != nil {
		return err
	}
	found, err := inspectLocalMarker(s.config.LocalRoot, rel, local)
	if err != nil {
		return fmt.Errorf("verify materialized marker %q: %w", rel, err)
	}
	if found.Status != marker.ValidMarker {
		return fmt.Errorf("verify materialized marker %q: status=%d", rel, found.Status)
	}
	s.recordRemoteStub(rel, remote, local, found.Marker, now)
	s.logf("info", "stub synchronized path=%q size=%d stub_size=%d time=%s", rel, remote.Size, local.Size, time.Since(started).Round(time.Millisecond))
	return nil
}

func sameLocalFile(a, b localFile) bool {
	return a.Inode == b.Inode && a.Size == b.Size && a.ModTime == b.ModTime
}

func (s *Service) waitStableMedia(ctx context.Context, rel string, initial localFile) (localFile, error) {
	previous := initial
	deadline := time.Now().Add(time.Minute)
	for {
		timer := time.NewTimer(s.config.SettleTime)
		select {
		case <-ctx.Done():
			timer.Stop()
			return localFile{}, ctx.Err()
		case <-timer.C:
		}
		current, err := statLocal(s.config.LocalRoot, rel)
		if err != nil {
			return localFile{}, err
		}
		if sameLocalFile(previous, current) {
			return current, nil
		}
		if time.Now().After(deadline) {
			return current, errors.New("file did not settle within 60 seconds")
		}
		previous = current
	}
}

func mediaContentType(rel string) string {
	if value := mime.TypeByExtension(path.Ext(rel)); value != "" {
		return value
	}
	return "application/octet-stream"
}

func allocatedBytes(info os.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(stat.Blocks) * 512
	}
	return 0
}

func (s *Service) uploadLocalMedia(ctx context.Context, rel string, initial localFile, now time.Time) error {
	transaction := randomSuffix()
	previous := s.state.Media[rel]
	previous.Managed = false
	previous.Status = mediaPendingUpload
	previous.Transaction = transaction
	previous.LastError = ""
	s.state.Media[rel] = previous
	s.logf("info", "media upload pending transaction=%q path=%q size=%d mtime=%q", transaction, rel, initial.Size, time.Unix(0, initial.ModTime).Format(time.RFC3339Nano))
	if err := s.store.Save(s.state); err != nil {
		return err
	}
	stable, err := s.waitStableMedia(ctx, rel, initial)
	if err != nil {
		previous.Status, previous.LastError = mediaPendingUpload, err.Error()
		s.state.Media[rel] = previous
		return fmt.Errorf("settle media %q: %w", rel, err)
	}
	filename := filepath.Join(s.config.LocalRoot, filepath.FromSlash(rel))
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	probe, probeErr := core.Probe(&fileSource{file: f, size: stable.Size}, s.config.Budget)
	if probeErr != nil {
		_ = f.Close()
		previous.Status, previous.LastError = mediaProbeFailed, probeErr.Error()
		s.state.Media[rel] = previous
		s.logf("info", "media upload failed transaction=%q path=%q stage=probe error=%q local_preserved=true", transaction, rel, probeErr)
		return fmt.Errorf("probe local media %q: %w", rel, probeErr)
	}
	previous.Status = mediaUploading
	s.state.Media[rel] = previous
	if err := s.store.Save(s.state); err != nil {
		_ = f.Close()
		return err
	}
	started := time.Now()
	s.logf("info", "media upload started transaction=%q path=%q size=%d", transaction, rel, stable.Size)
	remote, putErr := s.mutable.Put(ctx, rel, f, stable.Size, mediaContentType(rel))
	closeErr := f.Close()
	if putErr == nil {
		putErr = closeErr
	}
	if putErr != nil {
		previous.Status, previous.LastError = mediaUploadFailed, putErr.Error()
		s.state.Media[rel] = previous
		s.logf("info", "media upload failed transaction=%q path=%q stage=put error=%q local_preserved=true", transaction, rel, putErr)
		return fmt.Errorf("upload media %q: %w", rel, putErr)
	}
	s.logf("info", "media upload completed transaction=%q path=%q elapsed=%s", transaction, rel, time.Since(started).Round(time.Millisecond))
	after, err := statLocal(s.config.LocalRoot, rel)
	if err != nil {
		return err
	}
	if !sameLocalFile(stable, after) {
		err := errors.New("local file changed during upload")
		previous.Status, previous.LastError = mediaPendingUpload, err.Error()
		s.state.Media[rel] = previous
		s.logf("info", "media upload failed transaction=%q path=%q stage=local-changed error=%q local_preserved=true", transaction, rel, err)
		return err
	}
	if remote.Size != stable.Size || remote.ETag == "" {
		err := fmt.Errorf("remote verification failed: size=%d etag_present=%t", remote.Size, remote.ETag != "")
		previous.Status, previous.LastError = mediaVerifyFailed, err.Error()
		s.state.Media[rel] = previous
		s.logf("info", "media upload failed transaction=%q path=%q stage=verify error=%q local_preserved=true", transaction, rel, err)
		return err
	}
	s.logf("info", "media upload verified transaction=%q path=%q local_size=%d remote_size=%d remote_etag=%q", transaction, rel, stable.Size, remote.Size, remote.ETag)
	if err := materializePlan(ctx, s.config.LocalRoot, rel, probe, remote.ETag, remote.ModTime); err != nil {
		previous.Status, previous.LastError = mediaStubFailed, err.Error()
		s.state.Media[rel] = previous
		return fmt.Errorf("replace uploaded media %q with stub: %w", rel, err)
	}
	local, err := statLocal(s.config.LocalRoot, rel)
	if err != nil {
		return err
	}
	found, err := inspectLocalMarker(s.config.LocalRoot, rel, local)
	if err != nil {
		return fmt.Errorf("verify uploaded media marker %q: %w", rel, err)
	}
	if found.Status != marker.ValidMarker {
		return fmt.Errorf("verify uploaded media marker %q: status=%d", rel, found.Status)
	}
	s.recordRemoteStub(rel, remote, local, found.Marker, now)
	s.snapshot.Entries[rel] = remote
	if info, err := os.Stat(filepath.Join(s.config.LocalRoot, filepath.FromSlash(rel))); err == nil {
		s.logf("info", "media replaced with stub transaction=%q path=%q remote_size=%d stub_size=%d allocated_bytes=%d", transaction, rel, remote.Size, info.Size(), allocatedBytes(info))
	}
	return nil
}

func (s *Service) confirmRemoteMissing(ctx context.Context, rel string) (bool, error) {
	_, err := s.origin.Stat(ctx, rel)
	if errors.Is(err, origin.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (s *Service) removeLocalMediaUnit(rel string) error {
	files, err := scanLocal(s.config.LocalRoot)
	if err != nil {
		return err
	}
	deletedSidecars := 0
	for candidate := range files {
		if candidate == rel {
			continue
		}
		match := ClassifySidecar(candidate, []string{rel})
		if match.MediaPath != rel || match.Ambiguous {
			continue
		}
		if err := os.Remove(filepath.Join(s.config.LocalRoot, filepath.FromSlash(candidate))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(s.state.Sidecars, candidate)
		deletedSidecars++
	}
	stubDeleted := false
	if err := os.Remove(filepath.Join(s.config.LocalRoot, filepath.FromSlash(rel))); err == nil {
		stubDeleted = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for candidate, sidecar := range s.state.Sidecars {
		if sidecar.MediaPath == rel || ClassifySidecar(candidate, []string{rel}).MediaPath == rel {
			delete(s.state.Sidecars, candidate)
		}
	}
	delete(s.state.Media, rel)
	s.logf("info", "local media unit removed path=%q stub_deleted=%t sidecars_deleted=%d", rel, stubDeleted, deletedSidecars)
	return syncDirectory(filepath.Dir(filepath.Join(s.config.LocalRoot, filepath.FromSlash(rel))))
}

func (s *Service) reconcileMediaFiles(ctx context.Context, local map[string]localFile, remoteScan bool) error {
	now := time.Now().UTC()
	all := make(map[string]bool)
	for rel, entry := range s.snapshot.Entries {
		if !entry.IsDir && entry.Size > 0 && s.matcher.Match(rel) {
			all[rel] = true
		}
	}
	for rel := range local {
		if s.matcher.Match(rel) {
			all[rel] = true
		}
	}
	for rel := range s.state.Media {
		all[rel] = true
	}
	paths := make([]string, 0, len(all))
	for rel := range all {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	var actionErrors []error
	for _, rel := range paths {
		if s.duplicateBlocked(rel) {
			previous := s.state.Media[rel]
			previous.Managed = false
			previous.Status = mediaDuplicateRemote
			s.state.Media[rel] = previous
			continue
		}
		remote, remoteExists := s.snapshot.Entries[rel]
		file, localExists := local[rel]
		previous, tracked := s.state.Media[rel]
		if !localExists {
			if remoteExists {
				if err := s.materializeRemote(ctx, rel, remote, now); err != nil {
					actionErrors = append(actionErrors, err)
				}
				continue
			}
			if tracked && remoteScan {
				missing, err := s.confirmRemoteMissing(ctx, rel)
				if err != nil {
					actionErrors = append(actionErrors, fmt.Errorf("confirm remote deletion %q: %w", rel, err))
					continue
				}
				if missing {
					s.logf("info", "remote media deletion confirmed path=%q local_kind=missing", rel)
					if err := s.removeLocalMediaUnit(rel); err != nil {
						actionErrors = append(actionErrors, err)
					}
				}
			}
			continue
		}
		found, err := inspectLocalMarker(s.config.LocalRoot, rel, file)
		if err != nil {
			actionErrors = append(actionErrors, err)
			continue
		}
		switch found.Status {
		case marker.InvalidMarker:
			previous.Managed = false
			previous.Status, previous.LastError = mediaInvalidMarker, "invalid mediastub marker"
			s.state.Media[rel] = previous
			s.logf("info", "invalid-marker path=%q", rel)
		case marker.NoMarker:
			if remoteExists && tracked && legacyTrackedStub(previous, file, remote) {
				if err := s.materializeRemote(ctx, rel, remote, now); err != nil {
					actionErrors = append(actionErrors, err)
				}
				continue
			}
			if !remoteExists && remoteScan && tracked {
				missing, statErr := s.confirmRemoteMissing(ctx, rel)
				if statErr != nil {
					actionErrors = append(actionErrors, statErr)
					continue
				}
				if missing {
					s.logf("info", "remote media deletion confirmed path=%q local_kind=no-marker", rel)
					s.logf("info", "remote deletion overridden by local media path=%q action=upload", rel)
				}
			}
			if err := s.uploadLocalMedia(ctx, rel, file, now); err != nil {
				actionErrors = append(actionErrors, err)
			}
		case marker.ValidMarker:
			if remoteExists {
				if markerMatchesRemote(found.Marker, remote) {
					s.recordRemoteStub(rel, remote, file, found.Marker, now)
				} else if err := s.materializeRemote(ctx, rel, remote, now); err != nil {
					actionErrors = append(actionErrors, err)
				}
				continue
			}
			if remoteScan {
				missing, statErr := s.confirmRemoteMissing(ctx, rel)
				if statErr != nil {
					actionErrors = append(actionErrors, statErr)
					continue
				}
				if missing {
					s.logf("info", "remote media deletion confirmed path=%q local_kind=valid-stub", rel)
					if err := s.removeLocalMediaUnit(rel); err != nil {
						actionErrors = append(actionErrors, err)
					}
				} else {
					previous.Status = mediaRemoteMissing
					s.state.Media[rel] = previous
				}
			}
		}
	}
	return errors.Join(actionErrors...)
}

func (s *Service) planMediaWork(local map[string]localFile) ([]string, int) {
	all := make(map[string]bool)
	for rel, entry := range s.snapshot.Entries {
		if !entry.IsDir && entry.Size > 0 && s.matcher.Match(rel) {
			all[rel] = true
		}
	}
	for rel := range local {
		if s.matcher.Match(rel) {
			all[rel] = true
		}
	}
	for rel := range s.state.Media {
		all[rel] = true
	}
	paths := make([]string, 0, len(all))
	for rel := range all {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	validMedia := make([]string, 0, len(paths))
	work := 0
	for _, rel := range paths {
		if s.duplicateBlocked(rel) {
			continue
		}
		remote, remoteExists := s.snapshot.Entries[rel]
		localFile, localExists := local[rel]
		needsWork := false
		willBeValid := false
		switch {
		case !localExists && remoteExists:
			needsWork, willBeValid = true, true
		case !localExists && !remoteExists:
			_, tracked := s.state.Media[rel]
			needsWork = tracked
		case localExists:
			found, err := inspectLocalMarker(s.config.LocalRoot, rel, localFile)
			if err != nil {
				needsWork = true
				break
			}
			switch found.Status {
			case marker.InvalidMarker:
				needsWork = false
			case marker.NoMarker:
				needsWork, willBeValid = true, true
			case marker.ValidMarker:
				needsWork = !remoteExists || !markerMatchesRemote(found.Marker, remote)
				willBeValid = remoteExists
			}
		}
		if willBeValid {
			validMedia = append(validMedia, rel)
		}
		if needsWork {
			work++
			s.logf("debug", "planned media path=%q", rel)
		}
	}
	return validMedia, work
}
