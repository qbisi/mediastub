package mountfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestProcessMatcher(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(procRoot, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "42", "comm"), []byte("ffprobe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matcher, err := newProcessMatcher([]string{"ff*"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	matcher.procRoot = procRoot
	ctx := fuse.NewContext(context.Background(), &fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}, Pid: 42})
	matched, caller, err := matcher.match(ctx)
	if err != nil || !matched || caller.pid != 42 || caller.uid != 1000 || caller.gid != 100 || caller.comm != "ffprobe" {
		t.Fatalf("match = %v, %+v, %v", matched, caller, err)
	}
}

func TestProcessMatcherUIDGIDOrComm(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		uids     []uint32
		gids     []uint32
		caller   fuse.Caller
		want     bool
	}{
		{name: "uid", patterns: []string{"ffprobe"}, uids: []uint32{1000}, caller: fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}, Pid: 99}, want: true},
		{name: "gid", patterns: []string{"ffprobe"}, gids: []uint32{100}, caller: fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}, Pid: 99}, want: true},
		{name: "uid mismatch", uids: []uint32{1001}, caller: fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}}},
		{name: "gid mismatch", gids: []uint32{101}, caller: fuse.Caller{Owner: fuse.Owner{Uid: 1000, Gid: 100}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matcher, err := newProcessMatcher(test.patterns, test.uids, test.gids)
			if err != nil {
				t.Fatal(err)
			}
			ctx := fuse.NewContext(context.Background(), &test.caller)
			matched, caller, err := matcher.match(ctx)
			if err != nil || matched != test.want {
				t.Fatalf("match = %v, %+v, %v", matched, caller, err)
			}
		})
	}
}

func TestProcessMatcherNoMatchAndFailures(t *testing.T) {
	matcher, err := newProcessMatcher([]string{"ffprobe"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	matcher.procRoot = t.TempDir()
	ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 99})
	if matched, _, err := matcher.match(ctx); err == nil || matched {
		t.Fatalf("missing comm match = %v, %v; want passthrough error", matched, err)
	}
	if matched, _, err := matcher.match(context.Background()); err == nil || matched {
		t.Fatalf("missing caller match = %v, %v; want passthrough error", matched, err)
	}
	if _, err := newProcessMatcher([]string{"["}, nil, nil); err == nil {
		t.Fatal("invalid process pattern was accepted")
	}
}
