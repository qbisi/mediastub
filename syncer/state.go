package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const stateVersion = 1

type State struct {
	Version    int                     `json:"version"`
	Remote     string                  `json:"remote"`
	LocalRoot  string                  `json:"localRoot"`
	Media      map[string]MediaState   `json:"media"`
	Sidecars   map[string]SidecarState `json:"sidecars"`
	Tombstones map[string]Tombstone    `json:"tombstones"`
}

type MediaState struct {
	Managed     bool      `json:"managed"`
	Fingerprint string    `json:"fingerprint"`
	ETag        string    `json:"etag,omitempty"`
	Size        int64     `json:"size"`
	RemoteMTime time.Time `json:"remoteMtime"`
	LocalMTime  time.Time `json:"localMtime"`
	LastSeen    time.Time `json:"lastSeen"`
	Status      string    `json:"status"`
}

type SidecarState struct {
	LocalSHA256        string    `json:"localSha256,omitempty"`
	LocalSize          int64     `json:"localSize"`
	LocalMTime         time.Time `json:"localMtime"`
	LastUploadedSHA256 string    `json:"lastUploadedSha256,omitempty"`
	RemoteETag         string    `json:"remoteEtag,omitempty"`
	RemoteSize         int64     `json:"remoteSize"`
	RemoteMTime        time.Time `json:"remoteMtime"`
	Status             string    `json:"status"`
}

type Tombstone struct {
	DeletedAt time.Time `json:"deletedAt"`
}

type stateStore struct {
	dir  string
	lock *os.File
}

func openStateStore(dir, remote, localRoot string) (*stateStore, *State, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil, errors.New("another mediastub sync process holds the state lock")
		}
		return nil, nil, fmt.Errorf("lock state directory: %w", err)
	}
	store := &stateStore{dir: dir, lock: lock}
	state := &State{Version: stateVersion, Remote: remote, LocalRoot: localRoot, Media: map[string]MediaState{}, Sidecars: map[string]SidecarState{}, Tombstones: map[string]Tombstone{}}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return store, state, nil
	}
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	if err := json.Unmarshal(data, state); err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("decode state.json: %w", err)
	}
	if state.Version != stateVersion {
		store.Close()
		return nil, nil, fmt.Errorf("unsupported state version %d", state.Version)
	}
	if state.Remote != remote || state.LocalRoot != localRoot {
		store.Close()
		return nil, nil, errors.New("state directory belongs to a different remote or local root")
	}
	if state.Media == nil {
		state.Media = map[string]MediaState{}
	}
	if state.Sidecars == nil {
		state.Sidecars = map[string]SidecarState{}
	}
	if state.Tombstones == nil {
		state.Tombstones = map[string]Tombstone{}
	}
	return store, state, nil
}

func (s *stateStore) Save(state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(s.dir, "state.json.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "state.json")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func (s *stateStore) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	lock := s.lock
	s.lock = nil
	_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return lock.Close()
}
