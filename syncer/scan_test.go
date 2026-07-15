package syncer

import (
	"context"
	"errors"
	"testing"

	"github.com/qbisi/mediastub/origin"
)

type scanOrigin struct {
	dirs map[string][]origin.Entry
	fail map[string]error
}

func (o *scanOrigin) Stat(context.Context, string) (origin.Entry, error) {
	return origin.Entry{}, origin.ErrNotFound
}
func (o *scanOrigin) ReadDir(_ context.Context, rel string) ([]origin.Entry, error) {
	if err := o.fail[rel]; err != nil {
		return nil, err
	}
	return append([]origin.Entry(nil), o.dirs[rel]...), nil
}
func (o *scanOrigin) Open(context.Context, origin.Entry) (origin.Object, error) {
	return nil, origin.ErrUnsupported
}
func (o *scanOrigin) Close() error { return nil }

func TestScanRemoteRejectsPartialTraversal(t *testing.T) {
	boom := errors.New("network failed")
	upstream := &scanOrigin{dirs: map[string][]origin.Entry{".": {{Path: "dir", IsDir: true}}}, fail: map[string]error{"dir": boom}}
	if snapshot, err := scanRemote(context.Background(), upstream); !errors.Is(err, boom) || snapshot != nil {
		t.Fatalf("scan = %+v, %v", snapshot, err)
	}
}

func TestScanRemoteDetectsDuplicatePath(t *testing.T) {
	upstream := &scanOrigin{dirs: map[string][]origin.Entry{
		".": {
			{Path: "movie.mkv", Size: 1, ETag: "one"},
			{Path: "movie.mkv", Size: 2, ETag: "two"},
			{Path: "unique.mkv", Size: 3, ETag: "three"},
		},
	}, fail: map[string]error{}}
	snapshot, err := scanRemote(context.Background(), upstream)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Entries["movie.mkv"]; ok {
		t.Fatal("duplicate path was selected")
	}
	if len(snapshot.Duplicates["movie.mkv"]) != 2 {
		t.Fatalf("duplicates = %+v", snapshot.Duplicates)
	}
	if snapshot.Entries["unique.mkv"].ETag != "three" {
		t.Fatalf("unique entry missing: %+v", snapshot.Entries)
	}
}
