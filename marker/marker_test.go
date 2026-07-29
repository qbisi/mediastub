package marker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qbisi/mediastub/core"
	"golang.org/x/sys/unix"
)

func writeSizedFile(t *testing.T, size int) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "media")
	if err := os.WriteFile(name, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func requireMarkerXattrs(t *testing.T) {
	t.Helper()
	name := writeSizedFile(t, 0)
	err := unix.Setxattr(name, FormatXattr, []byte("probe"), 0)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
		t.Skip("test filesystem does not support user xattrs")
	}
	if err != nil {
		t.Fatalf("probe user xattr support: %v", err)
	}
	if err := unix.Removexattr(name, FormatXattr); err != nil {
		t.Fatal(err)
	}
}

func TestReadableXattrMarkersRoundTrip(t *testing.T) {
	requireMarkerXattrs(t)
	planHash := [32]byte{1, 2, 3}
	for _, format := range []core.Format{core.FormatMatroska, core.FormatMP4} {
		name := writeSizedFile(t, 1024)
		webDAVURL := "https://example.test/dav/movie.mkv"
		if err := Set(name, format, 1024, `"etag"`, planHash, webDAVURL); err != nil {
			t.Fatal(err)
		}
		result, err := Inspect(name, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != ValidMarker || result.Format != format ||
			result.Marker.RemoteSize != 1024 || result.Marker.PlanHash != planHash ||
			result.Marker.RemoteETagHash != ETagHash(`"etag"`) ||
			result.Marker.WebDAVURL != webDAVURL {
			t.Fatalf("result = %+v", result)
		}
		for _, field := range []string{FormatXattr, RemoteSizeXattr, ETagHashXattr, PlanHashXattr, WebDAVURLXattr} {
			value, present, err := readXattr(name, field)
			if err != nil || !present || value == "" {
				t.Fatalf("%s = %q, present=%t, err=%v", field, value, present, err)
			}
		}
	}
}

func TestNoAndPartialXattrMarker(t *testing.T) {
	requireMarkerXattrs(t)
	name := writeSizedFile(t, 32)
	result, err := Inspect(name, 32)
	if err != nil || result.Status != NoMarker {
		t.Fatalf("plain result = %+v, %v", result, err)
	}
	if err := unix.Setxattr(name, FormatXattr, []byte(string(core.FormatMP4)), 0); err != nil {
		t.Fatal(err)
	}
	result, err = Inspect(name, 32)
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("partial result = %+v, %v", result, err)
	}
}

func TestSizeMismatchIsInvalid(t *testing.T) {
	requireMarkerXattrs(t)
	name := writeSizedFile(t, 32)
	if err := Set(name, core.FormatMP4, 32, "etag", [32]byte{}, "https://example.test/dav/movie.mp4"); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(name, 31); err != nil {
		t.Fatal(err)
	}
	result, err := Inspect(name, 31)
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("size-mismatched result = %+v, %v", result, err)
	}
}

func TestFileContentIsNeverTreatedAsMarker(t *testing.T) {
	requireMarkerXattrs(t)
	name := filepath.Join(t.TempDir(), "legacy-looking.mp4")
	content := []byte("ordinary media MSTUB01 uuid mediastubfs")
	if err := os.WriteFile(name, content, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Inspect(name, int64(len(content)))
	if err != nil || result.Status != NoMarker {
		t.Fatalf("file content affected marker detection: %+v, %v", result, err)
	}
}
