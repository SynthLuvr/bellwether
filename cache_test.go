package main

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// manualClock makes TTL expiry (including the exact boundary)
// deterministic without fake timers.
type manualClock struct {
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Current() time.Time       { return c.now }
func (c *manualClock) Advance(by time.Duration) { c.now = c.now.Add(by) }

func TestCachedLoaderServesValueInsideTTL(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loads := 0
	load := newCachedLoader(func() (string, error) {
		loads++
		return "first", nil
	}, cachedLoaderOptions{ttl: time.Second, now: clock.Current})

	if result := load(); result.err != nil || result.value != "first" || result.stale {
		t.Fatalf("load() = %+v; want fresh first", result)
	}
	clock.Advance(999 * time.Millisecond)
	if result := load(); result.err != nil || result.value != "first" || result.stale {
		t.Fatalf("load() = %+v; want fresh first", result)
	}
	if loads != 1 {
		t.Errorf("loader ran %d times, want 1", loads)
	}
}

func TestCachedLoaderReloadsOnceTTLFullyElapsed(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loads := 0
	load := newCachedLoader(func() (int, error) {
		loads++
		return loads, nil
	}, cachedLoaderOptions{ttl: time.Second, now: clock.Current})

	if result := load(); result.err != nil || result.value != 1 || result.stale {
		t.Fatalf("load() = %+v; want fresh 1", result)
	}
	// At exactly the TTL the value is expired: the loader requires
	// loadedAt+ttl to be strictly after now.
	clock.Advance(time.Second)
	if result := load(); result.err != nil || result.value != 2 || result.stale {
		t.Fatalf("load() = %+v; want fresh 2", result)
	}
	if loads != 2 {
		t.Errorf("loader ran %d times, want 2", loads)
	}
}

// gatedLoader stays pending until released, standing in for a slow
// upstream so concurrent single-flight behavior is observable. A
// non-empty panicValue makes the release path panic instead of
// returning, standing in for a crashing upstream.
type gatedLoader struct {
	mu         sync.Mutex
	loads      int
	entered    chan struct{}
	release    chan struct{}
	err        error
	panicValue string
}

func newGatedLoader() *gatedLoader {
	return &gatedLoader{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (g *gatedLoader) Load() (string, error) {
	g.mu.Lock()
	g.loads++
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.release
	if g.panicValue != "" {
		panic(g.panicValue)
	}
	if g.err != nil {
		return "", g.err
	}
	return "value", nil
}

func (g *gatedLoader) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.loads
}

func TestCachedLoaderSharesSingleFlightBetweenConcurrentCallers(t *testing.T) {
	t.Parallel()
	gate := newGatedLoader()
	load := newCachedLoader(gate.Load, cachedLoaderOptions{ttl: time.Second})

	results := make(chan cachedResult[string], 2)
	start := func() {
		go func() { results <- load() }()
	}
	// First caller parks inside the loader (the flight is registered
	// before the loader runs), the second joins that pending flight.
	start()
	<-gate.entered
	start()
	close(gate.release)

	for range 2 {
		if result := <-results; result.err != nil || result.value != "value" || result.stale {
			t.Errorf("load() = %+v, want fresh value", result)
		}
	}
	if loads := gate.count(); loads != 1 {
		t.Errorf("loader ran %d times, want 1", loads)
	}
}

func TestCachedLoaderPropagatesColdFailureToEveryConcurrentCaller(t *testing.T) {
	t.Parallel()
	gate := newGatedLoader()
	gate.err = errors.New("upstream down")
	// No maxAge beyond the TTL and no prior success: nothing to serve
	// stale, so every caller must see the error.
	load := newCachedLoader(gate.Load, cachedLoaderOptions{ttl: time.Second})

	errs := make(chan error, 2)
	start := func() { go func() { errs <- load().err }() }
	// The first caller parks inside the failing loader; the second
	// either joins the same pending flight (one load, the common case)
	// or, having arrived after it settled, retries immediately —
	// failures are never cached, so both outcomes propagate the error.
	start()
	<-gate.entered
	start()
	close(gate.release)

	for range 2 {
		if err := <-errs; !errors.Is(err, gate.err) {
			t.Errorf("load() error = %v, want %v", err, gate.err)
		}
	}
	// While the flight is pending the loader runs at most once; a
	// second run can only be the never-cache retry of a late caller.
	if loads := gate.count(); loads < 1 || loads > 2 {
		t.Errorf("loader ran %d times, want 1 or 2", loads)
	}
}

// awaitLoad runs call on its own goroutine and fails the test if it
// does not return within the deadline — the failure mode of a cache
// whose flight was stranded by a panic.
func awaitLoad(t *testing.T, call func() cachedResult[string]) cachedResult[string] {
	t.Helper()
	done := make(chan cachedResult[string], 1)
	go func() { done <- call() }()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("load() still blocked after 2s — the flight was stranded")
		return cachedResult[string]{}
	}
}

func TestCachedLoaderConvertsLoaderPanicToError(t *testing.T) {
	t.Parallel()
	load := newCachedLoader(func() (string, error) {
		panic("boom inside loader")
	}, cachedLoaderOptions{ttl: time.Second})

	result := awaitLoad(t, load)
	if result.err == nil {
		t.Fatal("load() succeeded, want the panic converted to an error")
	}
	if !strings.Contains(result.err.Error(), "boom inside loader") {
		t.Errorf("error = %q, want it to name the panic value", result.err)
	}
	if !strings.Contains(result.err.Error(), "runLoader") {
		t.Errorf("error = %q, want the recovered stack for the panic origin", result.err)
	}
	if result.value != "" || result.stale {
		t.Errorf("result = %+v, want the zero value with no stale serve", result)
	}
}

func TestCachedLoaderRetriesAfterLoaderPanic(t *testing.T) {
	t.Parallel()
	loads := 0
	load := newCachedLoader(func() (string, error) {
		loads++
		if loads == 1 {
			panic("boom inside loader")
		}
		return "recovered", nil
	}, cachedLoaderOptions{ttl: time.Second})

	if result := awaitLoad(t, load); result.err == nil {
		t.Fatal("first load succeeded, want the panic converted to an error")
	}
	// The wedge regression: a panicking loader must leave no stranded
	// flight, so the next call runs the loader again instead of
	// blocking on a done channel that never closed.
	result := awaitLoad(t, load)
	if result.err != nil {
		t.Fatalf("second load error = %v", result.err)
	}
	if result.value != "recovered" || result.stale {
		t.Errorf("second load = %+v, want fresh recovered", result)
	}
	if loads != 2 {
		t.Errorf("loader ran %d times, want 2", loads)
	}
}

func TestCachedLoaderUnblocksConcurrentWaitersOfPanickingFlight(t *testing.T) {
	t.Parallel()
	gate := newGatedLoader()
	gate.panicValue = "boom inside loader"
	load := newCachedLoader(gate.Load, cachedLoaderOptions{ttl: time.Second})

	errs := make(chan error, 2)
	start := func() { go func() { errs <- load().err }() }
	// The first caller panics inside the loader; the second joined its
	// pending flight and must receive the converted error, not block
	// forever on the flight's done channel.
	start()
	<-gate.entered
	start()
	close(gate.release)

	for range 2 {
		select {
		case err := <-errs:
			if err == nil || !strings.Contains(err.Error(), "boom inside loader") {
				t.Errorf("load() error = %v, want the converted panic", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a flight waiter still blocked after the loader panicked")
		}
	}
}

func TestCachedLoaderNeverCachesFailures(t *testing.T) {
	t.Parallel()
	loads := 0
	load := newCachedLoader(func() (string, error) {
		loads++
		if loads == 1 {
			return "", errors.New("upstream down")
		}
		return "recovered", nil
	}, cachedLoaderOptions{ttl: time.Second})

	if result := load(); result.err == nil {
		t.Fatal("first load succeeded, want failure")
	}
	result := load()
	if result.err != nil {
		t.Fatalf("second load error = %v", result.err)
	}
	if result.value != "recovered" || result.stale {
		t.Errorf("second load = %+v, want fresh recovered", result)
	}
	if loads != 2 {
		t.Errorf("loader ran %d times, want 2", loads)
	}
}

func TestCachedLoaderServesStaleValueWhenReloadFails(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loads := 0
	fail := false
	load := newCachedLoader(func() (string, error) {
		loads++
		if fail {
			return "", errors.New("upstream down")
		}
		return "good", nil
	}, cachedLoaderOptions{ttl: time.Second, maxAge: time.Minute, now: clock.Current})

	if result := load(); result.value != "good" || result.stale {
		t.Fatalf("first load = %+v, want fresh good", result)
	}
	// Past the TTL the reload fails; the previous value is still
	// within maxAge, so it is served stale with the error attached.
	fail = true
	clock.Advance(2 * time.Second)
	result := load()
	if result.err == nil || result.value != "good" || !result.stale {
		t.Fatalf("second load = %+v, want stale good with error", result)
	}
	if loads != 2 {
		t.Errorf("loader ran %d times, want 2", loads)
	}
	// The failure is not cached: the next call retries the reload
	// rather than serving the stale entry without another attempt.
	clock.Advance(2 * time.Second)
	result = load()
	if result.err == nil || !result.stale {
		t.Fatalf("third load = %+v, want another stale serve after a retried failure", result)
	}
	if loads != 3 {
		t.Errorf("loader ran %d times, want 3", loads)
	}
}

func TestCachedLoaderServesFreshValueAgainAfterStale(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	calls := 0
	load := newCachedLoader(func() (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("upstream down")
		}
		return "value-" + strconv.Itoa(calls), nil
	}, cachedLoaderOptions{ttl: time.Second, maxAge: time.Minute, now: clock.Current})

	if result := load(); result.err != nil || result.value != "value-1" || result.stale {
		t.Fatalf("first load = %+v, want fresh value-1", result)
	}
	clock.Advance(2 * time.Second)
	if result := load(); result.err == nil || !result.stale {
		t.Fatalf("second load = %+v, want a stale serve after the failed reload", result)
	}
	// A later successful reload replaces the entry and clears the
	// marker.
	clock.Advance(2 * time.Second)
	result := load()
	if result.err != nil || result.value != "value-3" || result.stale {
		t.Fatalf("third load = %+v, want fresh value-3", result)
	}
}

func TestCachedLoaderRefusesStaleValueBeyondMaxAge(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loads := 0
	load := newCachedLoader(func() (string, error) {
		loads++
		if loads == 1 {
			return "good", nil
		}
		return "", errors.New("upstream down")
	}, cachedLoaderOptions{ttl: time.Second, maxAge: time.Minute, now: clock.Current})

	if result := load(); result.err != nil || result.stale {
		t.Fatalf("first load = %+v, want fresh", result)
	}
	// Beyond maxAge the previous value is no longer servable: the
	// caller sees the raw failure instead of unboundedly stale data.
	clock.Advance(time.Minute)
	result := load()
	if result.err == nil || result.value != "" || result.stale {
		t.Fatalf("second load = %+v, want a non-stale failure", result)
	}
	if loads != 2 {
		t.Errorf("loader ran %d times, want 2", loads)
	}
}

// cooldownLoader counts loads and fails while fail is set, standing
// in for a quota-exhausted or hard-down upstream. The failure is a
// shared sentinel so errors.Is can identify it across loads.
type cooldownLoader struct {
	mu    sync.Mutex
	loads int
	fail  bool
	err   error
}

func newCooldownLoader() *cooldownLoader {
	return &cooldownLoader{err: errors.New("upstream down")}
}

func (c *cooldownLoader) Load() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loads++
	if c.fail {
		return "", c.err
	}
	return "value", nil
}

func (c *cooldownLoader) setFailing(fail bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = fail
}

func (c *cooldownLoader) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

func TestCachedLoaderFastFailsColdFailuresWithinCooldown(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loader := newCooldownLoader()
	loader.setFailing(true)
	load := newCachedLoader(loader.Load, cachedLoaderOptions{
		ttl:      time.Second,
		cooldown: 5 * time.Second,
		now:      clock.Current,
	})

	first := load()
	if first.err == nil || first.value != "" || first.stale {
		t.Fatalf("first load = %+v, want a cold failure", first)
	}
	// Inside the cooldown the recorded error is returned without
	// another loader run.
	clock.Advance(time.Second)
	second := load()
	if !errors.Is(second.err, first.err) {
		t.Fatalf("second load error = %v, want the recorded %v", second.err, first.err)
	}
	if loads := loader.count(); loads != 1 {
		t.Errorf("loader ran %d times inside the cooldown, want 1", loads)
	}
	// Past the cooldown the loader runs again.
	clock.Advance(5 * time.Second)
	third := load()
	if !errors.Is(third.err, first.err) {
		t.Fatalf("third load error = %v, want a retried failure", third.err)
	}
	if loads := loader.count(); loads != 2 {
		t.Errorf("loader ran %d times after the cooldown, want 2", loads)
	}
}

func TestCachedLoaderServesStaleInsideCooldownWithoutLoaderRun(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loader := newCooldownLoader()
	load := newCachedLoader(loader.Load, cachedLoaderOptions{
		ttl:      time.Second,
		maxAge:   time.Minute,
		cooldown: 5 * time.Second,
		now:      clock.Current,
	})

	if result := load(); result.err != nil || result.value != "value" {
		t.Fatalf("first load = %+v, want fresh value", result)
	}
	// Past the TTL the reload fails; the value is within maxAge, so it
	// is served stale and the failure arms the cooldown.
	loader.setFailing(true)
	clock.Advance(2 * time.Second)
	if result := load(); result.err == nil || !result.stale || result.value != "value" {
		t.Fatalf("second load = %+v, want stale value with error", result)
	}
	// The next request inside the cooldown serves the stale value
	// without another upstream attempt.
	clock.Advance(time.Second)
	result := load()
	if result.err == nil || !result.stale || result.value != "value" {
		t.Fatalf("third load = %+v, want stale value without a loader run", result)
	}
	if loads := loader.count(); loads != 2 {
		t.Errorf("loader ran %d times, want 2 (no run inside the cooldown)", loads)
	}
	// Past the cooldown the reload is retried.
	clock.Advance(5 * time.Second)
	if result := load(); result.err == nil || !result.stale {
		t.Fatalf("fourth load = %+v, want a retried stale serve", result)
	}
	if loads := loader.count(); loads != 3 {
		t.Errorf("loader ran %d times after the cooldown, want 3", loads)
	}
}

func TestCachedLoaderClearsCooldownAfterSuccess(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loader := newCooldownLoader()
	loader.setFailing(true)
	load := newCachedLoader(loader.Load, cachedLoaderOptions{
		ttl:      time.Second,
		cooldown: 5 * time.Second,
		now:      clock.Current,
	})

	if result := load(); result.err == nil {
		t.Fatal("first load succeeded, want failure")
	}
	// Past the cooldown the retry succeeds and must disarm it…
	loader.setFailing(false)
	clock.Advance(6 * time.Second)
	if result := load(); result.err != nil || result.value != "value" {
		t.Fatalf("second load = %+v, want fresh value", result)
	}
	// …so the next TTL lapse reloads immediately, not one cooldown
	// later.
	clock.Advance(2 * time.Second)
	if result := load(); result.err != nil || result.value != "value" || result.stale {
		t.Fatalf("third load = %+v, want a fresh reload", result)
	}
	if loads := loader.count(); loads != 3 {
		t.Errorf("loader ran %d times, want 3", loads)
	}
}

func TestCachedLoaderFastFailBeyondMaxAgeReturnsErrorNotStale(t *testing.T) {
	t.Parallel()
	clock := newManualClock()
	loader := newCooldownLoader()
	load := newCachedLoader(loader.Load, cachedLoaderOptions{
		ttl:      time.Second,
		maxAge:   time.Minute,
		cooldown: 10 * time.Minute,
		now:      clock.Current,
	})

	if result := load(); result.err != nil || result.value != "value" {
		t.Fatalf("first load = %+v, want fresh value", result)
	}
	loader.setFailing(true)
	clock.Advance(2 * time.Second)
	if result := load(); result.err == nil || !result.stale {
		t.Fatalf("second load = %+v, want stale serve with error", result)
	}
	// The stale window lapses while the cooldown is still open: the
	// fast-fail must surface the error, not serve ever-older data.
	clock.Advance(time.Minute)
	result := load()
	if result.err == nil || result.stale || result.value != "" {
		t.Fatalf("third load = %+v, want the error with no stale serve", result)
	}
	if loads := loader.count(); loads != 2 {
		t.Errorf("loader ran %d times, want 2", loads)
	}
}
