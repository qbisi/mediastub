package marker

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUploadedMarkerAndBlockers(t *testing.T) {
	requireMarkerXattrs(t)
	name := writeSizedFile(t, 32)
	mtime := time.Unix(123, 456)
	if err := os.Chtimes(name, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := SetUploaded(name, 32, mtime, `"etag"`); err != nil {
		t.Fatal(err)
	}
	status, uploaded, err := InspectUploaded(name, 32, mtime)
	if err != nil || status != ValidMarker || uploaded.RemoteETagHash != ETagHash(`"etag"`) {
		t.Fatalf("uploaded marker = %+v status=%d, %v", uploaded, status, err)
	}
	blockers, err := BlockingUserXattrs(name)
	if err != nil || len(blockers) != 0 {
		t.Fatalf("initial blockers = %v, %v", blockers, err)
	}
	if err := unix.Setxattr(name, "user.subrip", []byte("working"), 0); err != nil {
		t.Fatal(err)
	}
	blockers, err = BlockingUserXattrs(name)
	if err != nil || len(blockers) != 0 {
		t.Fatalf("unrelated xattr blockers = %v, %v", blockers, err)
	}
	if err := unix.Setxattr(name, KeepXattrName, []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	blockers, err = BlockingUserXattrs(name)
	if err != nil || len(blockers) != 1 || blockers[0] != KeepXattrName {
		t.Fatalf("blockers = %v, %v", blockers, err)
	}
	if err := unix.Removexattr(name, "user.subrip"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Removexattr(name, KeepXattrName); err != nil {
		t.Fatal(err)
	}
	if err := RemoveUploaded(name); err != nil {
		t.Fatal(err)
	}
	status, _, err = InspectUploaded(name, 32, mtime)
	if err != nil || status != NoMarker {
		t.Fatalf("removed status = %d, %v", status, err)
	}
}

func TestUploadedMarkerRejectsChangedFile(t *testing.T) {
	requireMarkerXattrs(t)
	name := writeSizedFile(t, 32)
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetUploaded(name, info.Size(), info.ModTime(), "etag"); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(name, 31); err != nil {
		t.Fatal(err)
	}
	status, _, err := InspectUploaded(name, 31, info.ModTime())
	if err != nil || status != InvalidMarker {
		t.Fatalf("changed status = %d, %v", status, err)
	}
}
