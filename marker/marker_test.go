package marker

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/qbisi/mediastub/core"
)

func TestContainerTrailersRoundTrip(t *testing.T) {
	planHash := [32]byte{1, 2, 3}
	for _, format := range []core.Format{core.FormatMatroska, core.FormatMP4} {
		trailer, err := Trailer(format, 1024, `"etag"`, planHash)
		if err != nil {
			t.Fatal(err)
		}
		file := append(make([]byte, 1024), trailer...)
		result, err := Inspect(bytes.NewReader(file), int64(len(file)))
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != ValidMarker || result.Format != format || result.Marker.RemoteSize != 1024 || result.Marker.PlanHash != planHash || result.Marker.RemoteETagHash != ETagHash(`"etag"`) {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestUnsupportedVersionIsInvalid(t *testing.T) {
	trailer, err := Trailer(core.FormatMP4, 16, "etag", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	payload := trailer[24:]
	binary.BigEndian.PutUint16(payload[8:10], Version+1)
	binary.BigEndian.PutUint32(payload[84:88], crc32.ChecksumIEEE(payload[:84]))
	file := append(make([]byte, 16), trailer...)
	result, err := Inspect(bytes.NewReader(file), int64(len(file)))
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("unsupported version result = %+v, %v", result, err)
	}
}

func TestNoInvalidAndTruncatedMarker(t *testing.T) {
	plain := []byte("ordinary media bytes")
	result, err := Inspect(bytes.NewReader(plain), int64(len(plain)))
	if err != nil || result.Status != NoMarker {
		t.Fatalf("plain result = %+v, %v", result, err)
	}
	trailer, err := Trailer(core.FormatMP4, int64(len(plain)), "etag", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append(append([]byte(nil), plain...), trailer...)
	corrupt[len(corrupt)-1] ^= 0xff
	result, err = Inspect(bytes.NewReader(corrupt), int64(len(corrupt)))
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("corrupt result = %+v, %v", result, err)
	}
	truncated := append(append([]byte(nil), plain...), trailer[:len(trailer)-3]...)
	result, err = Inspect(bytes.NewReader(truncated), int64(len(truncated)))
	if err != nil || result.Status != InvalidMarker {
		t.Fatalf("truncated result = %+v, %v", result, err)
	}
}
