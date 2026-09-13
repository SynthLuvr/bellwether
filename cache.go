package main

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// cachedResult is one loader outcome: the value (servable when err is
// nil, or when stale is set), the loader error, and whether a value
// was served past its TTL because the reload failed.
type cachedResult[T any] struct {
	value T
	err   error
	stale bool
}

// cachedLoaderOptions configures a memoized loader.
type cachedLoaderOptions struct {
	ttl time.Duration
	// maxAge bounds the total life of an entry, fresh window plus
	// stale window: a reload failure may serve the previous value
	// (marked stale) only while loadedAt+maxAge is still in the
	// future. Zero means maxAge = ttl, disabling stale serving.
	maxAge time.Duration
	// cooldown is how long a failed load fast-fails before the loader
	// is retried: inside the window the previous error — or the
	// previous value, marked stale — is returned without calling the
	// loader, so request traffic cannot hammer a failing upstream once
	// per request. Zero disables fast-failing.
	cooldown time.Duration
	// now overrides the clock for deterministic TTL tests; nil uses
	// time.Now.
	now func() time.Time
}

// newCachedLoader memoizes loader in memory: the value is served until
// the TTL expires, concurrent calls share a single loader invocation
// (single-flight), and a failed load arms a cooldown — until it elapses,
// calls fast-fail with the previous error (or the previous value marked
// stale) instead of running the loader. A panicking loader counts as a
// failure: the panic is converted to an error rather than left to
// strand the in-flight callers. When the previous value is no older
// than maxAge, a failed reload serves it marked stale (with the error
// attached) instead of failing the caller. The TTL boundary is strict:
// a value is served fresh only while loadedAt+ttl is strictly after
// now.
func newCachedLoader[T any](loader func() (T, error), options cachedLoaderOptions) func() cachedResult[T] {
	now := options.now
	if now == nil {
		now = time.Now
	}
	maxAge := max(options.maxAge, options.ttl)
	state := &loaderState[T]{
		loader:   loader,
		ttl:      options.ttl,
		maxAge:   maxAge,
		cooldown: options.cooldown,
		now:      now,
	}
	return state.load
}

type loaderState[T any] struct {
	mu       sync.Mutex
	loader   func() (T, error)
	ttl      time.Duration
	maxAge   time.Duration
	cooldown time.Duration
	now      func() time.Time
	cached   *cacheEntry[T]
	inFlight *flight[T]
	// lastFailureAt is zero after a successful load; lastErr is the
	// error of the most recent failure, replayed while the cooldown
	// window stays open.
	lastFailureAt time.Time
	lastErr       error
}

// cacheEntry keeps the load time rather than an expiry, so the same
// stamp answers both "is it fresh?" (ttl) and "may it be served
// stale?" (maxAge).
type cacheEntry[T any] struct {
	loadedAt time.Time
	value    T
}

// flight is one in-progress loader invocation shared by concurrent
// callers; done closes when result is final.
type flight[T any] struct {
	done   chan struct{}
	result cachedResult[T]
}

func (s *loaderState[T]) load() cachedResult[T] {
	s.mu.Lock()
	now := s.now()
	if s.cached != nil && s.cached.loadedAt.Add(s.ttl).After(now) {
		value := s.cached.value
		s.mu.Unlock()
		return cachedResult[T]{value: value}
	}
	if s.inFlight != nil {
		current := s.inFlight
		s.mu.Unlock()
		<-current.done
		return current.result
	}
	// While the cooldown window is open the loader is not run at all,
	// so sequential requests cannot become one upstream attempt each
	// against a failing upstream.
	if !s.lastFailureAt.IsZero() && now.Sub(s.lastFailureAt) < s.cooldown {
		result := s.failedResultLocked(s.lastErr, now)
		s.mu.Unlock()
		return result
	}
	current := &flight[T]{done: make(chan struct{})}
	s.inFlight = current
	s.mu.Unlock()

	current.result = s.settle(s.runLoader())

	s.mu.Lock()
	if s.inFlight == current {
		s.inFlight = nil
	}
	s.mu.Unlock()
	close(current.done)
	return current.result
}

// failedResultLocked builds the outcome of a failed load at now: the
// previous value served stale while it is still within maxAge,
// otherwise just the error. Callers hold s.mu.
func (s *loaderState[T]) failedResultLocked(err error, now time.Time) cachedResult[T] {
	if s.cached != nil && s.cached.loadedAt.Add(s.maxAge).After(now) {
		return cachedResult[T]{value: s.cached.value, err: err, stale: true}
	}
	return cachedResult[T]{err: err}
}

// settle records a loader outcome: a success replaces the cached value
// and disarms the cooldown; a failure arms the cooldown so later loads
// fast-fail instead of re-running the loader once per request.
func (s *loaderState[T]) settle(value T, err error) cachedResult[T] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.cached = &cacheEntry[T]{loadedAt: s.now(), value: value}
		s.lastFailureAt, s.lastErr = time.Time{}, nil
		return cachedResult[T]{value: value}
	}
	now := s.now()
	s.lastFailureAt, s.lastErr = now, err
	return s.failedResultLocked(err, now)
}

// runLoader runs the loader, converting a panic into an error. The
// process often survives a panicking loader — net/http recovers
// panicking handlers — but an unconverted panic would strand the
// flight: done never closes, inFlight stays registered, and every
// later load blocks forever. As an error it is an ordinary failure —
// fast-failed inside the cooldown, retried once it elapses — and the
// recovered stack records where it came from.
func (s *loaderState[T]) runLoader() (value T, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("cached loader panicked: %v\n%s", recovered, debug.Stack())
		}
	}()
	return s.loader()
}
