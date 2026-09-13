package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeShutdownServer stands in for the *http.Server: Shutdown records
// the call and blocks until drain (graceful) or forceClosed (the grace
// period forcing connections shut).
type fakeShutdownServer struct {
	mu           sync.Mutex
	shutdowns    int
	closes       int
	shutdownSeen chan struct{}
	drain        chan struct{}
	forceClosed  chan struct{}
}

func newFakeServer() *fakeShutdownServer {
	return &fakeShutdownServer{
		shutdownSeen: make(chan struct{}, 1),
		drain:        make(chan struct{}),
		forceClosed:  make(chan struct{}),
	}
}

func (s *fakeShutdownServer) Shutdown(_ context.Context) error {
	s.mu.Lock()
	s.shutdowns++
	s.mu.Unlock()
	s.shutdownSeen <- struct{}{}
	select {
	case <-s.drain:
		return nil
	case <-s.forceClosed:
		return context.Canceled
	}
}

func (s *fakeShutdownServer) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	s.closeForce()
	return nil
}

func (s *fakeShutdownServer) closeForce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.forceClosed:
		// already closed
	default:
		close(s.forceClosed)
	}
}

func (s *fakeShutdownServer) counts() (shutdowns int, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdowns, s.closes
}

// fakeTimers emulate vitest's fake timers: scheduled callbacks fire
// exactly when the fake clock passes their deadline.
type fakeTimers struct {
	mu        sync.Mutex
	now       time.Duration
	scheduled []scheduledTimer
}

type scheduledTimer struct {
	deadline time.Duration
	f        func()
	stopped  bool
}

func (f *fakeTimers) after(d time.Duration, run func()) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	timer := scheduledTimer{deadline: f.now + d, f: run}
	f.scheduled = append(f.scheduled, timer)
	index := len(f.scheduled) - 1
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if index < len(f.scheduled) {
			f.scheduled[index].stopped = true
		}
	}
}

// awaitScheduled blocks until at least one live timer is scheduled, so
// tests never advance a clock whose timer has not been registered yet.
func (f *fakeTimers) awaitScheduled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		scheduled := len(f.scheduled)
		f.mu.Unlock()
		if scheduled > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no timer was ever scheduled")
}

func (f *fakeTimers) advance(by time.Duration) {
	f.mu.Lock()
	f.now += by
	var due []func()
	for i := range f.scheduled {
		if !f.scheduled[i].stopped && f.scheduled[i].deadline <= f.now {
			f.scheduled[i].stopped = true
			due = append(due, f.scheduled[i].f)
		}
	}
	f.mu.Unlock()
	for _, run := range due {
		run()
	}
}

// shutdownHarness wires every collaborator fake. A zero grace
// exercises the 10s default.
type shutdownHarness struct {
	server     *fakeShutdownServer
	timers     *fakeTimers
	registered []os.Signal
	signalCh   chan<- os.Signal
	exitCodes  chan int
	stdout     bytes.Buffer
	stderr     bytes.Buffer
}

func newShutdownHarness(grace time.Duration) *shutdownHarness {
	h := &shutdownHarness{
		server:    newFakeServer(),
		timers:    &fakeTimers{},
		exitCodes: make(chan int, 4),
	}
	shutdownOnSignals(shutdownOptions{
		server: h.server,
		grace:  grace,
		notify: func(ch chan<- os.Signal, signals ...os.Signal) {
			h.registered = append(h.registered, signals...)
			h.signalCh = ch
		},
		exit:   func(code int) { h.exitCodes <- code },
		after:  h.timers.after,
		stdout: &h.stdout,
		stderr: &h.stderr,
	})
	return h
}

// emit injects a signal the way the OS would deliver it to the
// registered channel.
func (h *shutdownHarness) emit(sig os.Signal) {
	h.signalCh <- sig
}

func TestShutdownOptionsResolveProductionDefaults(t *testing.T) {
	t.Parallel()
	// A pure resolution check: nothing here registers a real signal
	// handler against the test binary or wires the real os.Exit —
	// behaviors like the default grace period are pinned by the
	// harness tests below instead.
	resolved := shutdownOptions{}.withDefaults()

	if resolved.grace != defaultGracePeriod {
		t.Errorf("grace = %v, want the %v default", resolved.grace, defaultGracePeriod)
	}
	if resolved.notify == nil {
		t.Error("notify = nil, want the production signal.Notify")
	}
	if resolved.exit == nil {
		t.Error("exit = nil, want the production os.Exit")
	}
	if resolved.after == nil {
		t.Error("after = nil, want the production timer-backed after")
	}
	if resolved.stdout != os.Stdout || resolved.stderr != os.Stderr {
		t.Errorf("stdout/stderr = %v/%v, want the process streams", resolved.stdout, resolved.stderr)
	}
}

func TestShutdownOptionsKeepExplicitCollaborators(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	resolved := shutdownOptions{
		grace:  3 * time.Second,
		after:  func(time.Duration, func()) func() { return func() {} },
		stdout: &stdout,
		stderr: &stderr,
	}.withDefaults()

	if resolved.grace != 3*time.Second {
		t.Errorf("grace = %v, want the explicit 3s kept", resolved.grace)
	}
	if resolved.stdout != io.Writer(&stdout) || resolved.stderr != io.Writer(&stderr) {
		t.Error("stdout/stderr replaced, want the explicit writers kept")
	}
}

func TestShutdownRegistersSIGTERMAndSIGINT(t *testing.T) {
	t.Parallel()
	h := newShutdownHarness(time.Second)
	h.emit(syscall.SIGTERM) // start the loop observing signals

	want := []os.Signal{syscall.SIGTERM, syscall.SIGINT}
	if len(h.registered) != len(want) {
		t.Fatalf("registered = %v, want %v", h.registered, want)
	}
	for i := range want {
		if h.registered[i] != want[i] {
			t.Fatalf("registered = %v, want %v", h.registered, want)
		}
	}
}

func TestShutdownStopsAcceptingAndLetsRequestsDrain(t *testing.T) {
	t.Parallel()
	h := newShutdownHarness(time.Second)
	h.emit(syscall.SIGTERM)
	<-h.server.shutdownSeen

	if shutdowns, _ := h.server.counts(); shutdowns != 1 {
		t.Errorf("Shutdown called %d times, want 1", shutdowns)
	}
	select {
	case code := <-h.exitCodes:
		t.Fatalf("exited early with %d", code)
	default:
	}

	close(h.server.drain)
	if code := <-h.exitCodes; code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got, want := h.stdout.String(), "SIGTERM received, shutting down gracefully\nall connections closed, exiting\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestShutdownForceClosesOnlyAfterGracePeriod(t *testing.T) {
	t.Parallel()
	h := newShutdownHarness(time.Second)
	h.emit(syscall.SIGINT)
	<-h.server.shutdownSeen
	h.timers.awaitScheduled(t)

	h.timers.advance(999 * time.Millisecond)
	if _, closes := h.server.counts(); closes != 0 {
		t.Errorf("Close called at 999ms, want not yet")
	}

	h.timers.advance(time.Millisecond)
	if _, closes := h.server.counts(); closes != 1 {
		t.Errorf("Close calls = %d after grace, want 1", closes)
	}
	if got, want := h.stderr.String(), "grace period of 1000ms elapsed, closing remaining connections\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if code := <-h.exitCodes; code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestShutdownDefaultsGraceToFifteenSeconds(t *testing.T) {
	t.Parallel()
	h := newShutdownHarness(0) // zero grace → default
	h.emit(syscall.SIGTERM)
	<-h.server.shutdownSeen
	h.timers.awaitScheduled(t)

	h.timers.advance(14999 * time.Millisecond)
	if _, closes := h.server.counts(); closes != 0 {
		t.Errorf("Close called at 14999ms, want not yet")
	}
	h.timers.advance(time.Millisecond)
	if _, closes := h.server.counts(); closes != 1 {
		t.Errorf("Close calls = %d, want 1", closes)
	}
	if got, want := h.stderr.String(), "grace period of 15000ms elapsed, closing remaining connections\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestShutdownExitsImmediatelyOnSecondSignal(t *testing.T) {
	t.Parallel()
	h := newShutdownHarness(time.Second)
	h.emit(syscall.SIGTERM)
	<-h.server.shutdownSeen
	h.emit(syscall.SIGINT)

	if code := <-h.exitCodes; code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
