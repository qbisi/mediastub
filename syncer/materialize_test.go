package syncer

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/qbisi/mediastub/core"
)

func TestMaterializePlanCreatesSparseReadOnlyFile(t *testing.T) {
	root := t.TempDir()
	plan, err := core.NewPlan(4<<20, []core.Extent{{Offset: 8, Data: []byte("head")}, {Offset: (4 << 20) - 4, Data: []byte("tail")}})
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(100, 0)
	if err := materializePlan(context.Background(), root, "dir/movie.mkv", plan, mtime); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "dir", "movie.mkv")
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != plan.Size() || info.Mode().Perm() != 0o444 {
		t.Fatalf("stub info = %+v", info)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[8:12]) != "head" || string(data[len(data)-4:]) != "tail" || data[1024] != 0 {
		t.Fatal("stub extents or hole are wrong")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int64(stat.Blocks)*512 >= info.Size()/2 {
		t.Fatalf("file is not sparse enough: allocated=%d logical=%d", int64(stat.Blocks)*512, info.Size())
	}
}
