// Package marker encodes and recognizes container-valid mediastub trailers.
package marker

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"

	"github.com/qbisi/mediastub/core"
)

const (
	Version     = uint16(1)
	payloadSize = 8 + 2 + 2 + 8 + 32 + 32 + 4
)

var (
	Magic = [8]byte{'M', 'S', 'T', 'U', 'B', '0', '1', 0}
	UUID  = [16]byte{0x6d, 0x65, 0x64, 0x69, 0x61, 0x73, 0x74, 0x75, 0x62, 0x66, 0x73, 0x00, 0x00, 0x00, 0x00, 0x01}
)

type Status uint8

const (
	NoMarker Status = iota
	ValidMarker
	InvalidMarker
)

type Marker struct {
	Magic           [8]byte
	Version         uint16
	Flags           uint16
	RemoteSize      uint64
	RemoteETagHash  [32]byte
	PlanHash        [32]byte
	PayloadChecksum uint32
}

type Result struct {
	Status Status
	Format core.Format
	Marker Marker
}

func ETagHash(etag string) [32]byte { return sha256.Sum256([]byte(etag)) }

func payload(remoteSize int64, etag string, planHash [32]byte) []byte {
	out := make([]byte, payloadSize)
	copy(out[:8], Magic[:])
	binary.BigEndian.PutUint16(out[8:10], Version)
	binary.BigEndian.PutUint64(out[12:20], uint64(remoteSize))
	hash := ETagHash(etag)
	copy(out[20:52], hash[:])
	copy(out[52:84], planHash[:])
	binary.BigEndian.PutUint32(out[84:88], crc32.ChecksumIEEE(out[:84]))
	return out
}

func Trailer(format core.Format, remoteSize int64, etag string, planHash [32]byte) ([]byte, error) {
	if remoteSize < 0 {
		return nil, errors.New("negative marker remote size")
	}
	p := payload(remoteSize, etag, planHash)
	switch format {
	case core.FormatMatroska:
		// EBML Void ID followed by a one-byte VINT payload size.
		return append([]byte{0xec, 0x80 | byte(len(p))}, p...), nil
	case core.FormatMP4:
		out := make([]byte, 4+4+len(UUID)+len(p))
		binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
		copy(out[4:8], "uuid")
		copy(out[8:24], UUID[:])
		copy(out[24:], p)
		return out, nil
	default:
		return nil, errors.New("unsupported marker container")
	}
}

func parsePayload(p []byte) (Marker, bool) {
	var m Marker
	if len(p) != payloadSize || !bytes.Equal(p[:8], Magic[:]) {
		return m, false
	}
	copy(m.Magic[:], p[:8])
	m.Version = binary.BigEndian.Uint16(p[8:10])
	m.Flags = binary.BigEndian.Uint16(p[10:12])
	m.RemoteSize = binary.BigEndian.Uint64(p[12:20])
	copy(m.RemoteETagHash[:], p[20:52])
	copy(m.PlanHash[:], p[52:84])
	m.PayloadChecksum = binary.BigEndian.Uint32(p[84:88])
	return m, m.Version == Version && m.PayloadChecksum == crc32.ChecksumIEEE(p[:84])
}

// Inspect recognizes a marker at the physical end of a file.
func Inspect(r io.ReaderAt, size int64) (Result, error) {
	if size < 0 {
		return Result{}, errors.New("negative file size")
	}
	readTail := func(n int) ([]byte, error) {
		if size < int64(n) {
			return nil, io.EOF
		}
		buf := make([]byte, n)
		_, err := r.ReadAt(buf, size-int64(n))
		return buf, err
	}
	if box, err := readTail(4 + 4 + len(UUID) + payloadSize); err == nil {
		if string(box[4:8]) == "uuid" && bytes.Equal(box[8:24], UUID[:]) {
			m, valid := parsePayload(box[24:])
			if binary.BigEndian.Uint32(box[:4]) != uint32(len(box)) || !valid || m.RemoteSize != uint64(size-int64(len(box))) {
				return Result{Status: InvalidMarker, Format: core.FormatMP4, Marker: m}, nil
			}
			return Result{Status: ValidMarker, Format: core.FormatMP4, Marker: m}, nil
		}
	}
	if element, err := readTail(2 + payloadSize); err == nil {
		if element[0] == 0xec && element[1] == 0x80|byte(payloadSize) {
			m, valid := parsePayload(element[2:])
			if !valid || m.RemoteSize != uint64(size-int64(len(element))) {
				return Result{Status: InvalidMarker, Format: core.FormatMatroska, Marker: m}, nil
			}
			return Result{Status: ValidMarker, Format: core.FormatMatroska, Marker: m}, nil
		}
	}
	window := int64(512)
	if size < window {
		window = size
	}
	if window > 0 {
		buf := make([]byte, window)
		if _, err := r.ReadAt(buf, size-window); err != nil && !errors.Is(err, io.EOF) {
			return Result{}, err
		}
		if bytes.Contains(buf, Magic[:]) || bytes.Contains(buf, UUID[:]) {
			return Result{Status: InvalidMarker}, nil
		}
	}
	return Result{Status: NoMarker}, nil
}
