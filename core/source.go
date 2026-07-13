package core

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// Source is a sized random-access input.
type Source interface {
	io.ReaderAt
	Size() int64
}

// Budget defines hard probe limits.
type Budget struct {
	MaxBytes    int64
	MaxRequests int
	WindowSize  int
}

// DefaultBudget is deliberately much smaller than rclone's normal read chunk.
var DefaultBudget = Budget{
	MaxBytes:    16 << 20,
	MaxRequests: 128,
	WindowSize:  256 << 10,
}

// Stats describes actual source reads, excluding window hits.
type Stats struct {
	Bytes    int64
	Requests int
}

// ErrBudgetExceeded is returned before a read which would exceed a limit.
var ErrBudgetExceeded = errors.New("media probe read budget exceeded")

// BudgetedSource adds a single bounded read window and read accounting.
type BudgetedSource struct {
	source Source
	budget Budget
	mu     sync.Mutex
	stats  Stats
	window []byte
	start  int64
}

// NewBudgetedSource wraps source with the supplied limits.
func NewBudgetedSource(source Source, budget Budget) *BudgetedSource {
	if budget.MaxBytes <= 0 {
		budget.MaxBytes = DefaultBudget.MaxBytes
	}
	if budget.MaxRequests <= 0 {
		budget.MaxRequests = DefaultBudget.MaxRequests
	}
	if budget.WindowSize <= 0 {
		budget.WindowSize = DefaultBudget.WindowSize
	}
	return &BudgetedSource{source: source, budget: budget, start: -1}
}

// Size returns the logical source size.
func (s *BudgetedSource) Size() int64 { return s.source.Size() }

// Stats returns a snapshot of source read accounting.
func (s *BudgetedSource) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// ReadAt implements io.ReaderAt.
func (s *BudgetedSource) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, errors.New("negative read offset")
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	if off >= s.start && off+int64(len(p)) <= s.start+int64(len(s.window)) {
		copy(p, s.window[off-s.start:off-s.start+int64(len(p))])
		return len(p), nil
	}
	readSize := max(len(p), s.budget.WindowSize)
	if remain := s.Size() - off; int64(readSize) > remain {
		readSize = int(remain)
	}
	if s.stats.Requests+1 > s.budget.MaxRequests || s.stats.Bytes+int64(readSize) > s.budget.MaxBytes {
		return 0, fmt.Errorf("%w: bytes=%d/%d requests=%d/%d", ErrBudgetExceeded, s.stats.Bytes, s.budget.MaxBytes, s.stats.Requests, s.budget.MaxRequests)
	}
	buf := make([]byte, readSize)
	n, err := s.source.ReadAt(buf, off)
	if err == io.EOF && n == readSize {
		err = nil
	}
	buf = buf[:n]
	s.stats.Requests++
	s.stats.Bytes += int64(n)
	s.window = buf
	s.start = off
	available := min(len(p), n)
	copy(p[:available], buf[:available])
	if available != len(p) && err == nil {
		err = io.EOF
	}
	return available, err
}

type sourceReadSeeker struct {
	source Source
	offset int64
}

func newReadSeeker(source Source) io.ReadSeeker { return &sourceReadSeeker{source: source} }

func (r *sourceReadSeeker) Read(p []byte) (int, error) {
	n, err := r.source.ReadAt(p, r.offset)
	r.offset += int64(n)
	return n, err
}

func (r *sourceReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.offset + offset
	case io.SeekEnd:
		next = r.source.Size() + offset
	default:
		return 0, errors.New("invalid seek whence")
	}
	if next < 0 {
		return 0, errors.New("negative seek position")
	}
	r.offset = next
	return next, nil
}

// BytesSource adapts a byte slice for tests and small local probes.
type BytesSource []byte

func (s BytesSource) Size() int64 { return int64(len(s)) }
func (s BytesSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(s)) {
		return 0, io.EOF
	}
	n := copy(p, s[off:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}
