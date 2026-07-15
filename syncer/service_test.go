package syncer

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/origin"
)

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

func TestInitialReconcileAndTombstone(t *testing.T) {
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
	if stub.Size() != int64(len(testMP4())) || stub.Mode().Perm() != 0o444 {
		t.Fatalf("stub = size %d mode %o", stub.Size(), stub.Mode().Perm())
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
	if _, err := os.Stat(filepath.Join(localDir, "movie.jpg")); !os.IsNotExist(err) {
		t.Fatalf("deleted poster was restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteDir, "movie.jpg")); err != nil {
		t.Fatalf("remote poster was deleted: %v", err)
	}
	store, state, err := openStateStore(stateDir, remote, localDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Tombstones["movie.jpg"]; !ok {
		t.Fatal("deleted poster has no tombstone")
	}
	_ = store.Close()
	if err := os.WriteFile(filepath.Join(localDir, "movie.jpg"), []byte("replacement poster"), 0o664); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if got, err := os.ReadFile(filepath.Join(remoteDir, "movie.jpg")); err != nil || string(got) != "replacement poster" {
		t.Fatalf("recreated poster was not uploaded: %q, %v", got, err)
	}
	store, state, err = openStateStore(stateDir, remote, localDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Tombstones["movie.jpg"]; ok {
		t.Fatal("recreated poster retained tombstone")
	}
	_ = store.Close()

	if err := os.Remove(filepath.Join(remoteDir, "movie.mp4")); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, remote, localDir, stateDir)
	if _, err := os.Stat(filepath.Join(localDir, "movie.mp4")); err != nil {
		t.Fatalf("remote media deletion removed local stub: %v", err)
	}
	store, state, err = openStateStore(stateDir, remote, localDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if state.Media["movie.mp4"].Status != "remote-missing" {
		t.Fatalf("media status = %+v", state.Media["movie.mp4"])
	}
}

func TestUntrackedLocalMediaIsNotOverwritten(t *testing.T) {
	remoteDir, localDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteDir, "movie.mp4"), testMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "movie.mp4"), []byte("real local media"), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream, err := origin.NewLocal(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	runOnce(t, upstream, "file://"+remoteDir, localDir, stateDir)
	got, err := os.ReadFile(filepath.Join(localDir, "movie.mp4"))
	if err != nil || string(got) != "real local media" {
		t.Fatalf("collision was overwritten: %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(localDir, "movie.mp4")); err != nil {
		t.Fatal(err)
	}
	runOnce(t, upstream, "file://"+remoteDir, localDir, stateDir)
	info, err := os.Stat(filepath.Join(localDir, "movie.mp4"))
	if err != nil || info.Mode().Perm() != 0o444 || info.Size() != int64(len(testMP4())) {
		t.Fatalf("removed collision did not become a stub: %+v, %v", info, err)
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
	if err != nil || info.Size() != int64(len(updated)) || info.Mode().Perm() != 0o444 {
		t.Fatalf("updated stub = %+v, %v", info, err)
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
