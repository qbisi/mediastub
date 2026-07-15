package syncer

import (
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
	state.Tombstones["movie.jpg"] = Tombstone{}
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
	if _, ok := loaded.Tombstones["movie.jpg"]; !ok {
		t.Fatal("tombstone was not persisted")
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
