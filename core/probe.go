package core

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maxMetadataElementSize = 64 << 20

// Result contains a sparse read plan and source I/O stats.
type Result struct {
	Format Format
	Plan   *Plan
	Stats  Stats
}

// ErrUnsupported is returned for recognizable but unsupported inputs.
var ErrUnsupported = errors.New("unsupported media container")

// ErrNotMedia is returned when the input has no recognized media signature.
var ErrNotMedia = errors.New("not a recognized media object")

// Probe detects a container and builds a metadata-only sparse plan.
func Probe(source Source, budget Budget) (*Result, error) {
	bounded := NewBudgetedSource(source, budget)
	headSize := min(source.Size(), int64(64))
	head := make([]byte, int(headSize))
	if len(head) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrNotMedia)
	}
	if _, err := bounded.ReadAt(head, 0); err != nil && err != io.EOF {
		return nil, err
	}
	var (
		format Format
		plan   *Plan
		err    error
	)
	switch {
	case len(head) >= 4 && binary.BigEndian.Uint32(head[:4]) == ebmlHeaderID:
		format = FormatMatroska
		plan, err = probeMatroska(bounded)
	case looksLikeMP4(head):
		format = FormatMP4
		plan, err = probeMP4(bounded)
	default:
		err = fmt.Errorf("%w: unknown magic", ErrNotMedia)
	}
	if err != nil {
		return nil, err
	}
	return &Result{Format: format, Plan: plan, Stats: bounded.Stats()}, nil
}

func looksLikeMP4(head []byte) bool {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return false
	}
	size := binary.BigEndian.Uint32(head[:4])
	return size == 1 || size >= 8
}
