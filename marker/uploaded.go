package marker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// UploadedXattrName marks a verified real-media upload that can be replaced
	// by a stub unless KeepXattrName is present.
	UploadedXattrName = "user.uploaded"
	// KeepXattrName prevents an uploaded real-media inode from being replaced
	// by a stub while the attribute is present.
	KeepXattrName = "user.keep"

	uploadedVersion   = uint16(1)
	uploadedValueSize = 2 + 2 + 8 + 8 + 32 + 4
)

type UploadedMarker struct {
	Version         uint16
	Flags           uint16
	LocalSize       uint64
	LocalMTimeNanos int64
	RemoteETagHash  [32]byte
	PayloadChecksum uint32
}

func uploadedValue(size int64, mtime time.Time, etag string) ([]byte, error) {
	if size < 0 {
		return nil, errors.New("negative uploaded media size")
	}
	out := make([]byte, uploadedValueSize)
	binary.BigEndian.PutUint16(out[0:2], uploadedVersion)
	binary.BigEndian.PutUint64(out[4:12], uint64(size))
	binary.BigEndian.PutUint64(out[12:20], uint64(mtime.UnixNano()))
	hash := ETagHash(etag)
	copy(out[20:52], hash[:])
	binary.BigEndian.PutUint32(out[52:56], crc32.ChecksumIEEE(out[:52]))
	return out, nil
}

// SetUploaded records that the current real-media inode was uploaded and
// verified. It does not change the file mtime.
func SetUploaded(path string, size int64, mtime time.Time, etag string) error {
	encoded, err := uploadedValue(size, mtime, etag)
	if err != nil {
		return err
	}
	if err := unix.Setxattr(path, UploadedXattrName, encoded, 0); err != nil {
		return fmt.Errorf("set %s: %w", UploadedXattrName, err)
	}
	return nil
}

func readUploaded(path string) ([]byte, bool, error) {
	size, err := unix.Getxattr(path, UploadedXattrName, nil)
	if errors.Is(err, unix.ENODATA) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s size: %w", UploadedXattrName, err)
	}
	if size == 0 {
		return nil, true, nil
	}
	buf := make([]byte, size)
	n, err := unix.Getxattr(path, UploadedXattrName, buf)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", UploadedXattrName, err)
	}
	return buf[:n], true, nil
}

// InspectUploaded validates the uploaded marker against the current file.
func InspectUploaded(path string, size int64, mtime time.Time) (Status, UploadedMarker, error) {
	var marker UploadedMarker
	if size < 0 {
		return InvalidMarker, marker, errors.New("negative uploaded media size")
	}
	encoded, present, err := readUploaded(path)
	if err != nil {
		return InvalidMarker, marker, err
	}
	if !present {
		return NoMarker, marker, nil
	}
	if len(encoded) != uploadedValueSize {
		return InvalidMarker, marker, nil
	}
	marker.Version = binary.BigEndian.Uint16(encoded[0:2])
	marker.Flags = binary.BigEndian.Uint16(encoded[2:4])
	marker.LocalSize = binary.BigEndian.Uint64(encoded[4:12])
	marker.LocalMTimeNanos = int64(binary.BigEndian.Uint64(encoded[12:20]))
	copy(marker.RemoteETagHash[:], encoded[20:52])
	marker.PayloadChecksum = binary.BigEndian.Uint32(encoded[52:56])
	if marker.Version != uploadedVersion ||
		marker.LocalSize != uint64(size) ||
		marker.LocalMTimeNanos != mtime.UnixNano() ||
		marker.PayloadChecksum != crc32.ChecksumIEEE(encoded[:52]) {
		return InvalidMarker, marker, nil
	}
	return ValidMarker, marker, nil
}

// RemoveUploaded clears the uploaded handoff marker.
func RemoveUploaded(path string) error {
	err := unix.Removexattr(path, UploadedXattrName)
	if errors.Is(err, unix.ENODATA) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove %s: %w", UploadedXattrName, err)
	}
	return nil
}

// BlockingUserXattrs returns KeepXattrName when it is present. Other user,
// system, ACL, and security xattrs do not block stub replacement.
func BlockingUserXattrs(path string) ([]string, error) {
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		return nil, fmt.Errorf("list xattrs: %w", err)
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := unix.Listxattr(path, buf)
	if err != nil {
		return nil, fmt.Errorf("list xattrs: %w", err)
	}
	var blockers []string
	for _, name := range strings.Split(string(buf[:n]), "\x00") {
		if name == KeepXattrName {
			blockers = append(blockers, name)
		}
	}
	return blockers, nil
}
