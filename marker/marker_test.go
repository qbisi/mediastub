package marker

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
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
	err := unix.Setxattr(name, XattrName, []byte{1}, 0)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
		t.Skip("test filesystem does not support user xattrs")
	}
	if err != nil {
		t.Fatalf("probe user xattr support: %v", err)
	}
	if err := unix.Removexattr(name, XattrName); err != nil {
		t.Fatal(err)
	}
}

func TestValueEncodingRoundTrip(t *testing.T) {
	planHash := [32]byte{1, 2, 3}
	for _, format := range []core.Format{core.FormatMatroska, core.FormatMP4} {
		encoded, err := value(format, 1024, `"etag"`, planHash)
		if err != nil {
			t.Fatal(err)
		}
		result := parseValue(encoded, 1024)
		if result.Status != ValidMarker || result.Format != format ||
			result.Marker.RemoteETagHash != ETagHash(`"etag"`) ||
			result.Marker.PlanHash != planHash {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestXattrMarkersRoundTrip(t *testing.T) {
	requireMarkerXattrs(t)
	planHash := [32]byte{1, 2, 3}
	for _, format := range []core.Format{core.FormatMatroska, core.FormatMP4} {
		name := writeSizedFile(t, 1024)
		if err := Set(name, format, 1024, `"etag"`, planHash); err != nil {
			t.Fatal(err)
		}
		result, err := Inspect(name, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != ValidMarker || result.Format != format ||
			result.Marker.RemoteSize != 1024 || result.Marker.PlanHash != planHash ||
			result.Marker.RemoteETagHash != ETagHash(`"etag"`) {
			t.Fatalf("result = %+v", result)
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 1024 {
			t.Fatalf("xattr changed physical file size to %d", info.Size())
		}
	}
}

func TestUnsupportedXattrVersionIsInvalid(t *testing.T) {
	requireMarkerXattrs(t)
	name := writeSizedFile(t, 16)
	encoded, err := value(core.FormatMP4, 16, "etag", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(encoded[0:2], Version+1)
	binary.BigEndian.PutUint32(encoded[80:84], crc32.ChecksumIEEE(encoded[:80]))
	if err := unix.Setxattr(name, XattrName, encoded, 0); err != nil {
		t.Fatal(err)
	}
	result, err := Inspect(name, 16)
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("unsupported version result = %+v, %v", result, err)
	}
}

func TestNoAndInvalidXattrMarker(t *testing.T) {
	requireMarkerXattrs(t)
	name := writeSizedFile(t, 32)
	result, err := Inspect(name, 32)
	if err != nil || result.Status != NoMarker {
		t.Fatalf("plain result = %+v, %v", result, err)
	}
	if err := unix.Setxattr(name, XattrName, []byte("broken"), 0); err != nil {
		t.Fatal(err)
	}
	result, err = Inspect(name, 32)
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("corrupt result = %+v, %v", result, err)
	}
	if err := Set(name, core.FormatMP4, 32, "etag", [32]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(name, 31); err != nil {
		t.Fatal(err)
	}
	result, err = Inspect(name, 31)
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("size-mismatched result = %+v, %v", result, err)
	}
}

func TestFileContentIsNeverTreatedAsMarker(t *testing.T) {
	requireMarkerXattrs(t)
	name := filepath.Join(t.TempDir(), "legacy-looking.mp4")
	content := []byte("ordinary media MSTUB01\x00 uuid mediastubfs")
	if err := os.WriteFile(name, content, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Inspect(name, int64(len(content)))
	if err != nil || result.Status != NoMarker {
		t.Fatalf("file content affected marker detection: %+v, %v", result, err)
	}
}
