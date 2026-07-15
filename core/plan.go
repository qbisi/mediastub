package core

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Format is a supported media container format.
type Format string

const (
	FormatMatroska Format = "matroska"
	FormatMP4      Format = "mp4"
)

// Extent contains real bytes in an otherwise zero-filled sparse file.
type Extent struct {
	Offset int64
	Data   []byte
}

// Plan is a complete immutable sparse-file read plan.
type Plan struct {
	logicalSize int64
	extents     []Extent
}

// NewPlan validates and normalizes a sparse plan.
func NewPlan(size int64, extents []Extent) (*Plan, error) {
	if size < 0 {
		return nil, fmt.Errorf("negative plan size %d", size)
	}
	for i := range extents {
		if extents[i].Offset < 0 || int64(len(extents[i].Data)) > size-extents[i].Offset {
			return nil, fmt.Errorf("extent outside file: offset=%d length=%d size=%d", extents[i].Offset, len(extents[i].Data), size)
		}
		extents[i].Data = append([]byte(nil), extents[i].Data...)
	}
	sort.Slice(extents, func(i, j int) bool { return extents[i].Offset < extents[j].Offset })
	for i := 1; i < len(extents); i++ {
		if extents[i].Offset < extents[i-1].Offset+int64(len(extents[i-1].Data)) {
			return nil, errors.New("overlapping sparse extents")
		}
	}
	return &Plan{logicalSize: size, extents: extents}, nil
}

// Size returns the logical sparse-file size.
func (p *Plan) Size() int64 { return p.logicalSize }

// Extents returns a deep copy of the real byte ranges in the sparse plan.
func (p *Plan) Extents() []Extent {
	extents := make([]Extent, len(p.extents))
	for i, extent := range p.extents {
		extents[i] = Extent{Offset: extent.Offset, Data: append([]byte(nil), extent.Data...)}
	}
	return extents
}

// Hash returns a stable digest of the logical size and ordered extent data.
func (p *Plan) Hash() [32]byte {
	h := sha256.New()
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(p.logicalSize))
	_, _ = h.Write(encoded[:])
	for _, extent := range p.extents {
		binary.BigEndian.PutUint64(encoded[:], uint64(extent.Offset))
		_, _ = h.Write(encoded[:])
		binary.BigEndian.PutUint64(encoded[:], uint64(len(extent.Data)))
		_, _ = h.Write(encoded[:])
		_, _ = h.Write(extent.Data)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// ReadAt reads metadata extents and fills every other byte with zero.
func (p *Plan) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= p.logicalSize {
		return 0, io.EOF
	}
	n := len(b)
	if remain := p.logicalSize - off; int64(n) > remain {
		n = int(remain)
	}
	clear(b[:n])
	end := off + int64(n)
	for _, extent := range p.extents {
		extentEnd := extent.Offset + int64(len(extent.Data))
		if extentEnd <= off {
			continue
		}
		if extent.Offset >= end {
			break
		}
		from := max(off, extent.Offset)
		to := min(end, extentEnd)
		copy(b[from-off:to-off], extent.Data[from-extent.Offset:to-extent.Offset])
	}
	if n != len(b) {
		return n, io.EOF
	}
	return n, nil
}
