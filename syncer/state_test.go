package syncer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStateStoreSaveLoadAndLock(t *testing.T) {
	dir := t.TempDir()
	store, state, err := openStateStore(dir, "file:///remote", "/local")
	if err != nil {
		t.Fatal(err)
	}
	state.Media["movie.mkv"] = MediaState{Managed: true, Status: "active"}
	state.Sidecars["movie.jpg"] = SidecarState{MediaPath: "movie.mkv", Status: "synchronized"}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openStateStore(dir, "file:///remote", "/local"); err == nil {
		t.Fatal("second state lock succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, loaded, err := openStateStore(dir, "file:///remote", "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !loaded.Media["movie.mkv"].Managed || loaded.Media["movie.mkv"].Status != "active" {
		t.Fatalf("loaded state = %+v", loaded)
	}
	if loaded.Sidecars["movie.jpg"].MediaPath != "movie.mkv" {
		t.Fatal("sidecar association was not persisted")
	}
	other, _, err := openStateStore(t.TempDir(), "other", "/local")
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	if err := os.WriteFile(filepath.Join(dir, "state.json.tmp"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVersionOneStateDropsTombstones(t *testing.T) {
	dir := t.TempDir()
	legacy := []byte(`{"version":1,"remote":"file:///remote","localRoot":"/local","media":{},"sidecars":{},"tombstones":{"movie.jpg":{"deletedAt":"2026-01-01T00:00:00Z"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	store, state, err := openStateStore(dir, "file:///remote", "/local")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if state.Version != stateVersion {
		t.Fatalf("migrated version = %d", state.Version)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("tombstone")) {
		t.Fatalf("migrated state retained tombstones: %s", data)
	}
}

func TestStateStoreRejectsIdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	store, state, err := openStateStore(dir, "file:///remote", "/local")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if _, _, err := openStateStore(dir, "file:///other", "/local"); err == nil {
		t.Fatal("remote mismatch accepted")
	}
	if _, _, err := openStateStore(dir, "file:///remote", "/other"); err == nil {
		t.Fatal("local root mismatch accepted")
	}
}
