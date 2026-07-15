// Package mountfs projects an Origin as a read-only FUSE filesystem.
package mountfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/origin"
	"github.com/qbisi/mediastub/pathfilter"
)

// Config controls media probing and Plan caching.
type Config struct {
	Includes     []string
	Budget       core.Budget
	OnError      string
	CacheEntries int
	Logger       *log.Logger
}

// View is the random-access file view returned to FUSE.
type View interface {
	Size() int64
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	Close() error
}

type probeCall struct {
	ready chan struct{}
	plan  *core.Plan
	err   error
}

// Service selects media objects, creates Plans and opens projected views.
type Service struct {
	origin  origin.Origin
	config  Config
	matcher *pathfilter.Matcher

	mu    sync.Mutex
	cache map[string]*probeCall
}

// NewService constructs a projection service.
func NewService(upstream origin.Origin, config Config) (*Service, error) {
	if upstream == nil {
		return nil, errors.New("origin is required")
	}
	if config.OnError == "" {
		config.OnError = "passthrough"
	}
	if config.OnError != "passthrough" && config.OnError != "fail" {
		return nil, fmt.Errorf("on-error must be passthrough or fail")
	}
	matcher, err := pathfilter.New(config.Includes)
	if err != nil {
		return nil, err
	}
	if config.CacheEntries <= 0 {
		config.CacheEntries = 1024
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	return &Service{origin: upstream, config: config, matcher: matcher, cache: make(map[string]*probeCall)}, nil
}

// Origin returns the read-only namespace backing this service.
func (s *Service) Origin() origin.Origin { return s.origin }

func (s *Service) matches(name string) bool { return s.matcher.Match(name) }

// StubCandidate reports whether an entry is eligible for media probing before
// caller-process policy is applied.
func (s *Service) StubCandidate(entry origin.Entry) bool {
	return entry.Size > 0 && s.matches(entry.Path)
}

type probeSource struct {
	ctx    context.Context
	object origin.Object
	size   int64
}

func (s *probeSource) Size() int64 { return s.size }

func (s *probeSource) ReadAt(p []byte, off int64) (int, error) {
	return origin.ReadFullAt(s.ctx, s.object, p, off)
}

func (s *Service) cachedCall(ctx context.Context, key string) (*probeCall, bool, error) {
	s.mu.Lock()
	call, found := s.cache[key]
	if !found {
		call = &probeCall{ready: make(chan struct{})}
		s.cache[key] = call
		s.mu.Unlock()
		return call, true, nil
	}
	s.mu.Unlock()
	select {
	case <-call.ready:
		return call, false, call.err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (s *Service) finishCall(key string, call *probeCall, cacheable bool) {
	s.mu.Lock()
	if !cacheable {
		delete(s.cache, key)
	}
	close(call.ready)
	if len(s.cache) > s.config.CacheEntries {
		for candidate, cached := range s.cache {
			if candidate == key {
				continue
			}
			select {
			case <-cached.ready:
				delete(s.cache, candidate)
			default:
			}
			if len(s.cache) <= s.config.CacheEntries {
				break
			}
		}
	}
	s.mu.Unlock()
}

func (s *Service) decide(ctx context.Context, entry origin.Entry) (*core.Plan, error) {
	if !s.StubCandidate(entry) {
		return nil, nil
	}
	probeStarted := time.Now()
	key := entry.Fingerprint()
	call, leader, err := s.cachedCall(ctx, key)
	if err != nil || !leader {
		if call == nil {
			return nil, err
		}
		return call.plan, call.err
	}

	cacheable := true
	defer func() { s.finishCall(key, call, cacheable) }()
	object, err := s.origin.Open(ctx, entry)
	if err != nil {
		call.err = err
		cacheable = !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
		return nil, err
	}
	result, probeErr := core.Probe(&probeSource{ctx: ctx, object: object, size: entry.Size}, s.config.Budget)
	closeErr := object.Close()
	if probeErr == nil && closeErr != nil {
		probeErr = closeErr
	}
	if probeErr != nil {
		if errors.Is(probeErr, context.Canceled) || errors.Is(probeErr, context.DeadlineExceeded) {
			call.err = probeErr
			cacheable = false
			return nil, probeErr
		}
		if s.config.OnError == "passthrough" {
			s.config.Logger.Printf("probe skipped path=%q probe_time=%s error=%v",
				entry.Path, time.Since(probeStarted).Round(time.Microsecond), probeErr)
			return nil, nil
		}
		call.err = fmt.Errorf("probe %q: %w", entry.Path, probeErr)
		return nil, call.err
	}
	call.plan = result.Plan
	s.config.Logger.Printf("stub ready path=%q format=%s probe_bytes=%d requests=%d probe_time=%s",
		entry.Path, result.Format, result.Stats.Bytes, result.Stats.Requests,
		time.Since(probeStarted).Round(time.Microsecond))
	return call.plan, nil
}

// OpenView opens a projected or passthrough view. stubAllowed controls whether
// this open is eligible for probing; stubbed reports whether the returned bytes
// come from a generated Plan.
func (s *Service) OpenView(ctx context.Context, entry origin.Entry, stubAllowed bool) (view View, stubbed bool, err error) {
	if stubAllowed {
		plan, err := s.decide(ctx, entry)
		if err != nil {
			return nil, false, err
		}
		if plan != nil {
			return &planView{plan: plan}, true, nil
		}
	}
	object, err := s.origin.Open(ctx, entry)
	if err != nil {
		return nil, false, err
	}
	return &originView{object: object, size: entry.Size}, false, nil
}

type planView struct {
	plan *core.Plan
}

func (v *planView) Size() int64 { return v.plan.Size() }
func (v *planView) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	return v.plan.ReadAt(p, off)
}
func (v *planView) Close() error { return nil }

type originView struct {
	object origin.Object
	size   int64
}

func (v *originView) Size() int64 { return v.size }
func (v *originView) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	return origin.ReadFullAt(ctx, v.object, p, off)
}
func (v *originView) Close() error { return v.object.Close() }

var _ io.ReaderAt = (*probeSource)(nil)
