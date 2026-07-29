// Package marker stores and recognizes mediastub metadata in readable user xattrs.
package marker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/qbisi/mediastub/core"
	"golang.org/x/sys/unix"
)

const (
	XattrPrefix     = "user.mediastub."
	FormatXattr     = XattrPrefix + "format"
	RemoteSizeXattr = XattrPrefix + "remote_size"
	ETagHashXattr   = XattrPrefix + "etag_hash"
	PlanHashXattr   = XattrPrefix + "plan_hash"
	WebDAVURLXattr  = XattrPrefix + "webdav_url"
)

type Status uint8

const (
	NoMarker Status = iota
	ValidMarker
	InvalidMarker
)

type Marker struct {
	RemoteSize     uint64
	RemoteETagHash [32]byte
	PlanHash       [32]byte
	WebDAVURL      string
}

type Result struct {
	Status Status
	Format core.Format
	Marker Marker
}

func ETagHash(etag string) [32]byte { return sha256.Sum256([]byte(etag)) }

func validFormat(format core.Format) bool {
	return format == core.FormatMatroska || format == core.FormatMP4
}

// Set writes all marker fields to a temporary stub before it is published.
func Set(path string, format core.Format, remoteSize int64, etag string, planHash [32]byte, webDAVURL string) error {
	if remoteSize < 0 {
		return errors.New("negative marker remote size")
	}
	if !validFormat(format) {
		return fmt.Errorf("unsupported marker format %q", format)
	}
	if webDAVURL == "" {
		return errors.New("empty marker WebDAV URL")
	}
	etagHash := ETagHash(etag)
	values := []struct {
		name  string
		value string
	}{
		{FormatXattr, string(format)},
		{RemoteSizeXattr, strconv.FormatInt(remoteSize, 10)},
		{ETagHashXattr, hex.EncodeToString(etagHash[:])},
		{PlanHashXattr, hex.EncodeToString(planHash[:])},
		{WebDAVURLXattr, webDAVURL},
	}
	for _, field := range values {
		if err := unix.Setxattr(path, field.name, []byte(field.value), 0); err != nil {
			return fmt.Errorf("set %s: %w", field.name, err)
		}
	}
	return nil
}

func missingXattr(err error) bool {
	return err != nil && (errors.Is(err, unix.ENODATA) || strings.Contains(err.Error(), "attribute not found"))
}

func readXattr(path, name string) (string, bool, error) {
	size, err := unix.Getxattr(path, name, nil)
	if missingXattr(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s size: %w", name, err)
	}
	buf := make([]byte, size)
	n, err := unix.Getxattr(path, name, buf)
	if err != nil {
		return "", true, fmt.Errorf("read %s: %w", name, err)
	}
	return string(buf[:n]), true, nil
}

// Inspect requires the complete field set. A partial set is an invalid marker.
func Inspect(path string, fileSize int64) (Result, error) {
	if fileSize < 0 {
		return Result{}, errors.New("negative file size")
	}
	names := []string{FormatXattr, RemoteSizeXattr, ETagHashXattr, PlanHashXattr, WebDAVURLXattr}
	values := make(map[string]string, len(names))
	present := 0
	for _, name := range names {
		value, found, err := readXattr(path, name)
		if err != nil {
			return Result{}, err
		}
		if found {
			present++
			values[name] = value
		}
	}
	if present == 0 {
		return Result{Status: NoMarker}, nil
	}
	result := Result{Status: InvalidMarker}
	if present != len(names) {
		return result, nil
	}
	format := core.Format(values[FormatXattr])
	if !validFormat(format) {
		return result, nil
	}
	size, err := strconv.ParseUint(values[RemoteSizeXattr], 10, 64)
	if err != nil || size != uint64(fileSize) {
		return result, nil
	}
	etagHash, err := hex.DecodeString(values[ETagHashXattr])
	if err != nil || len(etagHash) != sha256.Size {
		return result, nil
	}
	planHash, err := hex.DecodeString(values[PlanHashXattr])
	if err != nil || len(planHash) != sha256.Size {
		return result, nil
	}
	result.Format = format
	result.Marker.RemoteSize = size
	copy(result.Marker.RemoteETagHash[:], etagHash)
	copy(result.Marker.PlanHash[:], planHash)
	result.Marker.WebDAVURL = values[WebDAVURLXattr]
	if result.Marker.WebDAVURL == "" {
		return result, nil
	}
	result.Status = ValidMarker
	return result, nil
}
