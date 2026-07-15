package syncer

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/marker"
	"github.com/qbisi/mediastub/origin"
)

type putOverrideOrigin struct {
	origin.Origin
	put func(context.Context, string, io.Reader, int64, string) (origin.Entry, error)
}

type scanFailOrigin struct {
	origin.MutableOrigin
	fail bool
}

type emptyScanOrigin struct{ origin.MutableOrigin }

func (o *emptyScanOrigin) ReadDir(context.Context, string) ([]origin.Entry, error) {
	return nil, nil
}

func (o *scanFailOrigin) ReadDir(ctx context.Context, rel string) ([]origin.Entry, error) {
	if o.fail {
		return nil, errors.New("remote scan failed")
	}
	return o.MutableOrigin.ReadDir(ctx, rel)
}

func (o *putOverrideOrigin) Put(ctx context.Context, rel string, source io.Reader, size int64, contentType string) (origin.Entry, error) {
	return o.put(ctx, rel, source, size, contentType)
}

func testMP4() []byte {
	box := func(kind string, payload []byte) []byte {
		out := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(out, uint32(len(out)))
		copy(out[4:8], kind)
		copy(out[8:], payload)
		return out
	}
	data := box("ftyp", []byte("isom"))
	data = append(data, box("mdat", []byte{1, 2, 3, 4})...)
	return append(data, box("moov", nil)...)
}

func runOnce(t *testing.T, upstream origin.Origin, remote, local, state string) {
	t.Helper()
	service, err := New(upstream, Config{Remote: remote, LocalRoot: local, StateDir: state, Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestReadyAfterPlanningBeforeApply(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.jpg"), []byte("poster"), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	service, err := New(upstream, Config{
		Remote: "file://" + remoteDir, LocalRoot: localDir, StateDir: stateDir,
		Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond,
		Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	readyCalled := false
	err = service.Run(context.Background(), func(status string) error {
		readyCalled = true
		if !strings.Contains(status, "media=1 sidecars=1") {
			t.Fatalf("ready status = %q", status)
		}
		if _, err := os.Stat(filepath.Join(localDir, "movie.mp4")); !os.IsNotExist(err) {
			t.Fatalf("stub existed before ready: %v", err)
		}
		if _, err := os.Stat(filepath.Join(localDir, "movie.jpg")); !os.IsNotExist(err) {
			t.Fatalf("sidecar existed before ready: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !readyCalled {
		t.Fatal("ready callback was not called")
	}
	for _, name := range []string{"movie.mp4", "movie.jpg"} {
		if _, err := os.Stat(filepath.Join(localDir, name)); err != nil {
			t.Fatalf("planned file %q was not synchronized: %v", name, err)
		}
	}
}

func TestDaemonStaysReadyAfterInitialApplyFailure(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	poster := filepath.Join(remoteDir, "movie.jpg")
	if err := os.WriteFile(poster, []byte("poster"), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	service, err := New(upstream, Config{
		Remote: "file://" + remoteDir, LocalRoot: localDir, StateDir: stateDir,
		Includes: []string{"*.mp4"}, PollInterval: time.Hour, SettleTime: time.Millisecond,
		Daemon: true, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = service.Run(ctx, func(string) error {
		if err := os.Remove(poster); err != nil {
			return err
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		return nil
	})
	if err != nil {
		t.Fatalf("daemon exited after post-readiness apply failure: %v", err)
	}
}

func TestInitialReconcileAndSidecarRestore(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.jpg"), []byte("remote poster"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.zh.srt"), []byte("remote subtitle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "movie.jpg"), []byte("local poster"), 0o664); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	remote := "file://" + remoteDir
	runOnce(t, upstream, remote, localDir, stateDir)
	stub, err := os.Stat(filepath.Join(localDir, "movie.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if stub.Size() <= int64(len(testMP4())) || stub.Mode().Perm() != 0o444 {
		t.Fatalf("stub = size %d mode %o", stub.Size(), stub.Mode().Perm())
	}
	f, err := os.Open(filepath.Join(localDir, "movie.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	markerResult, markerErr := marker.Inspect(f, stub.Size())
	_ = f.Close()
	if markerErr != nil || markerResult.Status != marker.ValidMarker {
		t.Fatalf("stub marker = %+v, %v", markerResult, markerErr)
	}
	if got, err := os.ReadFile(filepath.Join(remoteDir, "movie.jpg")); err != nil || string(got) != "local poster" {
		t.Fatalf("uploaded poster = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(localDir, "movie.zh.srt")); err != nil || string(got) != "remote subtitle" {
		t.Fatalf("downloaded subtitle = %q, %v", got, err)
	}
	posterEntry, err := upstream.Stat(context.Background(), "movie.jpg")
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	posterAgain, err := upstream.Stat(context.Background(), "movie.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if posterAgain.ETag != posterEntry.ETag {
		t.Fatal("unchanged sidecar was uploaded again")
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.jpg"), []byte("unexpected remote edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if got, err := os.ReadFile(filepath.Join(remoteDir, "movie.jpg")); err != nil || string(got) != "local poster" {
		t.Fatalf("remote edit was not overwritten by local authority: %q, %v", got, err)
	}

	if err := os.Remove(filepath.Join(remoteDir, "movie.zh.srt")); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if got, err := os.ReadFile(filepath.Join(remoteDir, "movie.zh.srt")); err != nil || string(got) != "remote subtitle" {
		t.Fatalf("missing remote subtitle was not restored: %q, %v", got, err)
	}

	if err := os.Remove(filepath.Join(localDir, "movie.jpg")); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if got, err := os.ReadFile(filepath.Join(localDir, "movie.jpg")); err != nil || string(got) != "local poster" {
		t.Fatalf("deleted poster was not restored: %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "movie.jpg"), []byte("replacement poster"), 0o664); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if got, err := os.ReadFile(filepath.Join(remoteDir, "movie.jpg")); err != nil || string(got) != "replacement poster" {
		t.Fatalf("recreated poster was not uploaded: %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(remoteDir, "movie.mp4")); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if _, err := os.Stat(filepath.Join(localDir, "movie.mp4")); !os.IsNotExist(err) {
		t.Fatalf("remote media deletion did not remove local stub: %v", err)
	}
	for _, name := range []string{"movie.jpg", "movie.zh.srt"} {
		if _, err := os.Stat(filepath.Join(localDir, name)); !os.IsNotExist(err) {
			t.Fatalf("remote media deletion did not remove %s: %v", name, err)
		}
	}
	store, state, err := openStateStore(stateDir, remote, localDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := state.Media["movie.mp4"]; exists {
		t.Fatalf("deleted media retained state: %+v", state.Media["movie.mp4"])
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if _, err := os.Stat(filepath.Join(localDir, "movie.jpg")); !os.IsNotExist(err) {
		t.Fatalf("orphan sidecar was restored without media: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	for _, name := range []string{"movie.mp4", "movie.jpg", "movie.zh.srt"} {
		if _, err := os.Stat(filepath.Join(localDir, name)); err != nil {
			t.Fatalf("remote media reappearance did not restore %s: %v", name, err)
		}
	}
}

func TestUntrackedLocalMediaUploadsAndBecomesStub(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	localMedia := append(append([]byte(nil), testMP4()...), []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}...)
	if err := os.WriteFile(filepath.Join(localDir, "movie.mp4"), localMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	runOnce(t, upstream, "file://"+remoteDir, localDir, stateDir)
	remoteMedia, err := os.ReadFile(filepath.Join(remoteDir, "movie.mp4"))
	if err != nil || string(remoteMedia) != string(localMedia) {
		t.Fatalf("local media was not uploaded: size=%d, %v", len(remoteMedia), err)
	}
	info, err := os.Stat(filepath.Join(localDir, "movie.mp4"))
	if err != nil || info.Mode().Perm() != 0o444 || info.Size() <= int64(len(localMedia)) {
		t.Fatalf("uploaded media did not become a stub: %+v, %v", info, err)
	}
}

func TestLocalMediaProbeFailurePreservesFile(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	remoteMedia := testMP4()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), remoteMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	localMedia := []byte("not media")
	localName := filepath.Join(localDir, "movie.mp4")
	if err := os.WriteFile(localName, localMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	service, err := New(upstream, Config{Remote: "file://" + remoteDir, LocalRoot: localDir, StateDir: stateDir, Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), nil); err == nil {
		t.Fatal("invalid local media unexpectedly synchronized")
	}
	if got, err := os.ReadFile(localName); err != nil || string(got) != string(localMedia) {
		t.Fatalf("failed upload changed local file: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(remoteDir, "movie.mp4")); err != nil || string(got) != string(remoteMedia) {
		t.Fatalf("failed upload changed remote file: size=%d, %v", len(got), err)
	}
}

func TestMediaPutFailurePreservesRealFile(t *testing.T) {
	remoteDir, localDir := t.TempDir(), t.TempDir()
	localMedia := testMP4()
	localName := filepath.Join(localDir, "movie.mp4")
	if err := os.WriteFile(localName, localMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	upstream := &putOverrideOrigin{Origin: base, put: func(context.Context, string, io.Reader, int64, string) (origin.Entry, error) {
		return origin.Entry{}, errors.New("PUT failed")
	}}
	service, err := New(upstream, Config{Remote: "file://" + remoteDir, LocalRoot: localDir, StateDir: t.TempDir(), Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), nil); err == nil {
		t.Fatal("PUT failure was ignored")
	}
	if got, err := os.ReadFile(localName); err != nil || string(got) != string(localMedia) {
		t.Fatalf("PUT failure changed local media: size=%d, %v", len(got), err)
	}
	if _, err := os.Stat(filepath.Join(remoteDir, "movie.mp4")); !os.IsNotExist(err) {
		t.Fatalf("PUT failure created remote media: %v", err)
	}
}

func TestMediaVerifyFailurePreservesRealFile(t *testing.T) {
	remoteDir, localDir := t.TempDir(), t.TempDir()
	localMedia := testMP4()
	localName := filepath.Join(localDir, "movie.mp4")
	if err := os.WriteFile(localName, localMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	upstream := &putOverrideOrigin{Origin: base, put: func(ctx context.Context, rel string, source io.Reader, size int64, contentType string) (origin.Entry, error) {
		entry, err := base.Put(ctx, rel, source, size, contentType)
		entry.ETag = ""
		return entry, err
	}}
	service, err := New(upstream, Config{Remote: "file://" + remoteDir, LocalRoot: localDir, StateDir: t.TempDir(), Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), nil); err == nil {
		t.Fatal("verification failure was ignored")
	}
	if got, err := os.ReadFile(localName); err != nil || string(got) != string(localMedia) {
		t.Fatalf("verification failure changed local media: size=%d, %v", len(got), err)
	}
}

func TestMediaChangeDuringUploadPreservesRealFile(t *testing.T) {
	remoteDir, localDir := t.TempDir(), t.TempDir()
	localMedia := testMP4()
	localName := filepath.Join(localDir, "movie.mp4")
	if err := os.WriteFile(localName, localMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	upstream := &putOverrideOrigin{Origin: base, put: func(ctx context.Context, rel string, source io.Reader, size int64, contentType string) (origin.Entry, error) {
		entry, err := base.Put(ctx, rel, source, size, contentType)
		if err == nil {
			f, openErr := os.OpenFile(localName, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				return origin.Entry{}, openErr
			}
			_, writeErr := f.Write([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'})
			closeErr := f.Close()
			if writeErr != nil {
				return origin.Entry{}, writeErr
			}
			if closeErr != nil {
				return origin.Entry{}, closeErr
			}
		}
		return entry, err
	}}
	service, err := New(upstream, Config{Remote: "file://" + remoteDir, LocalRoot: localDir, StateDir: t.TempDir(), Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), nil); err == nil {
		t.Fatal("local change during upload was ignored")
	}
	info, err := os.Stat(localName)
	if err != nil || info.Mode().Perm() == 0o444 || info.Size() <= int64(len(localMedia)) {
		t.Fatalf("changed local media was replaced: %+v, %v", info, err)
	}
}

func TestTrackedStubIsReplacedWhenRemoteChanges(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	remoteName := filepath.Join(remoteDir, "movie.mp4")
	if err := os.WriteFile(remoteName, testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	remote := "file://" + remoteDir
	runOnce(t, upstream, remote, localDir, stateDir)
	updated := append(append([]byte(nil), testMP4()...), []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}...)
	if err := os.WriteFile(remoteName, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(localDir, "movie.mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	info, err := os.Stat(filepath.Join(localDir, "movie.mp4"))
	if err != nil || info.Size() <= int64(len(updated)) || info.Mode().Perm() != 0o444 {
		t.Fatalf("updated stub = %+v, %v", info, err)
	}
}

func TestTrackedLegacyStubIsMigratedWithoutUpload(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	remoteMedia := testMP4()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), remoteMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	localName := filepath.Join(localDir, "movie.mp4")
	if err := os.WriteFile(localName, make([]byte, len(remoteMedia)), 0o444); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(localName)
	if err != nil {
		t.Fatal(err)
	}
	store, state, err := openStateStore(stateDir, "file://"+remoteDir, localDir)
	if err != nil {
		t.Fatal(err)
	}
	state.Media["movie.mp4"] = MediaState{Managed: true, Size: int64(len(remoteMedia)), LocalMTime: info.ModTime(), Status: "active"}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	base, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	upstream := &putOverrideOrigin{Origin: base, put: func(context.Context, string, io.Reader, int64, string) (origin.Entry, error) {
		return origin.Entry{}, errors.New("legacy stub must not be uploaded")
	}}
	runOnce(t, upstream, "file://"+remoteDir, localDir, stateDir)
	info, err = os.Stat(localName)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(localName)
	if err != nil {
		t.Fatal(err)
	}
	result, inspectErr := marker.Inspect(f, info.Size())
	_ = f.Close()
	if inspectErr != nil || result.Status != marker.ValidMarker {
		t.Fatalf("legacy stub was not migrated: %+v, %v", result, inspectErr)
	}
	if got, err := os.ReadFile(filepath.Join(remoteDir, "movie.mp4")); err != nil || string(got) != string(remoteMedia) {
		t.Fatalf("legacy migration changed remote media: size=%d, %v", len(got), err)
	}
}

func TestRemoteDeletionIsOverriddenByRealLocalMedia(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	remoteName := filepath.Join(remoteDir, "movie.mp4")
	if err := os.WriteFile(remoteName, testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	remote := "file://" + remoteDir
	runOnce(t, upstream, remote, localDir, stateDir)
	localMedia := append(append([]byte(nil), testMP4()...), []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}...)
	localName := filepath.Join(localDir, "movie.mp4")
	if err := os.Chmod(localName, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localName, localMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(remoteName); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if got, err := os.ReadFile(remoteName); err != nil || string(got) != string(localMedia) {
		t.Fatalf("real local media did not restore remote: size=%d, %v", len(got), err)
	}
	info, err := os.Stat(localName)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(localName)
	if err != nil {
		t.Fatal(err)
	}
	result, inspectErr := marker.Inspect(f, info.Size())
	_ = f.Close()
	if inspectErr != nil || result.Status != marker.ValidMarker {
		t.Fatalf("restored media did not become a valid stub: %+v, %v", result, inspectErr)
	}
}

func TestRemoteScanFailureDoesNotPropagateDeletion(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.jpg"), []byte("poster"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	upstream := &scanFailOrigin{MutableOrigin: base}
	remote := "file://" + remoteDir
	runOnce(t, upstream, remote, localDir, stateDir)
	upstream.fail = true
	service, err := New(upstream, Config{Remote: remote, LocalRoot: localDir, StateDir: stateDir, Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), nil); err == nil {
		t.Fatal("remote scan failure was ignored")
	}
	for _, name := range []string{"movie.mp4", "movie.jpg"} {
		if _, err := os.Stat(filepath.Join(localDir, name)); err != nil {
			t.Fatalf("scan failure removed %s: %v", name, err)
		}
	}
}

func TestDuplicateRemoteMediaPathFailsClosed(t *testing.T) {
	localDir := t.TempDir()
	localName := filepath.Join(localDir, "movie.mp4")
	localMedia := testMP4()
	if err := os.WriteFile(localName, localMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	base := &scanOrigin{dirs: map[string][]origin.Entry{
		".": {
			{Path: "movie.mp4", Size: int64(len(localMedia)), ETag: "one"},
			{Path: "movie.mp4", Size: int64(len(localMedia)), ETag: "two"},
		},
	}, fail: map[string]error{}}
	putCalled := false
	upstream := &putOverrideOrigin{Origin: base, put: func(context.Context, string, io.Reader, int64, string) (origin.Entry, error) {
		putCalled = true
		return origin.Entry{}, errors.New("unexpected PUT")
	}}
	service, err := New(upstream, Config{Remote: "https://example.invalid/dav", LocalRoot: localDir, StateDir: t.TempDir(), Includes: []string{"*.mp4"}, PollInterval: time.Second, SettleTime: time.Millisecond, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if putCalled {
		t.Fatal("duplicate remote path triggered PUT")
	}
	if got, err := os.ReadFile(localName); err != nil || string(got) != string(localMedia) {
		t.Fatalf("duplicate path changed local media: size=%d, %v", len(got), err)
	}
}

func TestRemoteDeletionRequiresExactNotFound(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	remote := "file://" + remoteDir
	runOnce(t, base, remote, localDir, stateDir)
	upstream := &emptyScanOrigin{MutableOrigin: base}
	runOnce(t, upstream, remote, localDir, stateDir)
	if _, err := os.Stat(filepath.Join(localDir, "movie.mp4")); err != nil {
		t.Fatalf("incomplete-looking snapshot deleted exact-existing media: %v", err)
	}
}

func TestInvalidMarkerIsPreserved(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	remoteName := filepath.Join(remoteDir, "movie.mp4")
	remoteMedia := testMP4()
	if err := os.WriteFile(remoteName, remoteMedia, 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	remote := "file://" + remoteDir
	runOnce(t, upstream, remote, localDir, stateDir)
	localName := filepath.Join(localDir, "movie.mp4")
	if err := os.Chmod(localName, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(localName)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(localName, data, 0o644); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if got, err := os.ReadFile(localName); err != nil || string(got) != string(data) {
		t.Fatalf("invalid marker file was replaced: size=%d, %v", len(got), err)
	}
	if got, err := os.ReadFile(remoteName); err != nil || string(got) != string(remoteMedia) {
		t.Fatalf("invalid marker file was uploaded: size=%d, %v", len(got), err)
	}
	store, state, err := openStateStore(stateDir, remote, localDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if state.Media["movie.mp4"].Status != mediaInvalidMarker {
		t.Fatalf("invalid marker state = %+v", state.Media["movie.mp4"])
	}
}

func TestWatcherUploadsNewSidecar(t *testing.T) {
	remoteDir, localDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	service, err := New(upstream, Config{
		Remote: "file://" + remoteDir, LocalRoot: localDir, StateDir: t.TempDir(),
		Includes: []string{"*.mp4"}, PollInterval: time.Hour, SettleTime: 10 * time.Millisecond,
		Daemon: true, Budget: core.DefaultBudget, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- service.Run(ctx, func(string) error { close(ready); return nil }) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("initial synchronization plan did not become ready")
	}
	if err := os.WriteFile(filepath.Join(localDir, "movie.nfo"), []byte("watch upload"), 0o664); err != nil {
		cancel()
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := os.ReadFile(filepath.Join(remoteDir, "movie.nfo"))
		if err == nil && string(got) == "watch upload" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("watch upload did not complete: %q, %v", got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
