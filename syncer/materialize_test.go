package syncer

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/marker"
)

func TestMaterializePlanCreatesSparseReadOnlyFile(t *testing.T) {
	root := t.TempDir()
	plan, err := core.NewPlan(4<<20, []core.Extent{{Offset: 8, Data: []byte("head")}, {Offset: (4 << 20) - 4, Data: []byte("tail")}})
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(100, 0)
	result := &core.Result{Format: core.FormatMatroska, Plan: plan}
	if err := materializePlan(context.Background(), root, "dir/movie.mkv", result, `"etag"`, mtime); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "dir", "movie.mkv")
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= plan.Size() || info.Mode().Perm() != 0o444 {
		t.Fatalf("stub info = %+v", info)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[8:12]) != "head" || string(data[plan.Size()-4:plan.Size()]) != "tail" || data[1024] != 0 {
		t.Fatal("stub extents or hole are wrong")
	}
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	markerResult, err := marker.Inspect(f, info.Size())
	if err != nil || markerResult.Status != marker.ValidMarker || markerResult.Marker.RemoteSize != uint64(plan.Size()) || markerResult.Marker.PlanHash != plan.Hash() {
		t.Fatalf("marker = %+v, %v", markerResult, err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int64(stat.Blocks)*512 >= info.Size()/2 {
		t.Fatalf("file is not sparse enough: allocated=%d logical=%d", int64(stat.Blocks)*512, info.Size())
	}
}
