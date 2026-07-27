// Package marker stores and recognizes mediastub metadata in a user xattr.
package marker

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/qbisi/mediastub/core"
	"golang.org/x/sys/unix"
)

const (
	// XattrName is the only marker recognized by mediastub.
	XattrName = "user.mediastub"
	Version   = uint16(1)

	valueSize = 2 + 2 + 2 + 2 + 8 + 32 + 32 + 4
)

type Status uint8

const (
	NoMarker Status = iota
	ValidMarker
	InvalidMarker
)

type Marker struct {
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

func formatID(format core.Format) (uint16, error) {
	switch format {
	case core.FormatMatroska:
		return 1, nil
	case core.FormatMP4:
		return 2, nil
	default:
		return 0, fmt.Errorf("unsupported marker format %q", format)
	}
}

func parseFormat(id uint16) (core.Format, bool) {
	switch id {
	case 1:
		return core.FormatMatroska, true
	case 2:
		return core.FormatMP4, true
	default:
		return "", false
	}
}

func value(format core.Format, remoteSize int64, etag string, planHash [32]byte) ([]byte, error) {
	if remoteSize < 0 {
		return nil, errors.New("negative marker remote size")
	}
	id, err := formatID(format)
	if err != nil {
		return nil, err
	}
	out := make([]byte, valueSize)
	binary.BigEndian.PutUint16(out[0:2], Version)
	binary.BigEndian.PutUint16(out[4:6], id)
	binary.BigEndian.PutUint64(out[8:16], uint64(remoteSize))
	hash := ETagHash(etag)
	copy(out[16:48], hash[:])
	copy(out[48:80], planHash[:])
	binary.BigEndian.PutUint32(out[80:84], crc32.ChecksumIEEE(out[:80]))
	return out, nil
}

// Set writes a complete marker to XattrName. The target must already have its
// final logical size; callers should set the xattr before publishing the file.
func Set(path string, format core.Format, remoteSize int64, etag string, planHash [32]byte) error {
	encoded, err := value(format, remoteSize, etag, planHash)
	if err != nil {
		return err
	}
	if err := unix.Setxattr(path, XattrName, encoded, 0); err != nil {
		return fmt.Errorf("set %s: %w", XattrName, err)
	}
	return nil
}

func readXattr(path string) ([]byte, bool, error) {
	size, err := unix.Getxattr(path, XattrName, nil)
	if errors.Is(err, unix.ENODATA) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s size: %w", XattrName, err)
	}
	if size == 0 {
		return nil, true, nil
	}
	buf := make([]byte, size)
	n, err := unix.Getxattr(path, XattrName, buf)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", XattrName, err)
	}
	return buf[:n], true, nil
}

func parseValue(encoded []byte, fileSize int64) Result {
	result := Result{Status: InvalidMarker}
	if len(encoded) != valueSize {
		return result
	}
	result.Marker.Version = binary.BigEndian.Uint16(encoded[0:2])
	result.Marker.Flags = binary.BigEndian.Uint16(encoded[2:4])
	format, formatOK := parseFormat(binary.BigEndian.Uint16(encoded[4:6]))
	result.Format = format
	result.Marker.RemoteSize = binary.BigEndian.Uint64(encoded[8:16])
	copy(result.Marker.RemoteETagHash[:], encoded[16:48])
	copy(result.Marker.PlanHash[:], encoded[48:80])
	result.Marker.PayloadChecksum = binary.BigEndian.Uint32(encoded[80:84])
	if result.Marker.Version != Version || !formatOK ||
		result.Marker.PayloadChecksum != crc32.ChecksumIEEE(encoded[:80]) ||
		result.Marker.RemoteSize != uint64(fileSize) {
		return result
	}
	result.Status = ValidMarker
	return result
}

// Inspect recognizes only XattrName. A file without this xattr is real media.
func Inspect(path string, fileSize int64) (Result, error) {
	if fileSize < 0 {
		return Result{}, errors.New("negative file size")
	}
	encoded, present, err := readXattr(path)
	if err != nil {
		return Result{}, err
	}
	if !present {
		return Result{Status: NoMarker}, nil
	}
	return parseValue(encoded, fileSize), nil
}
