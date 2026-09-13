package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// defaultGracePeriod bounds how long in-flight requests may drain
// before remaining connections are closed forcibly. It outlasts the
// worst-case upstream exchange — two 5s attempts plus up to 1s of
// Retry-After-capped backoff (~11s) — so a request caught mid-retry
// still finishes.
const defaultGracePeriod = 15 * time.Second

// shutdownServer is the shutdown surface of *http.Server, narrowed so
// tests can substitute a controllable fake.
type shutdownServer interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// shutdownOptions wires the graceful-shutdown collaborators. Nil
// fields get production defaults (signal.Notify, os.Exit,
// time.AfterFunc, os.Stdout, os.Stderr); tests inject fakes.
type shutdownOptions struct {
	server shutdownServer
	// grace zero means defaultGracePeriod.
	grace time.Duration
	// notify installs the signal handlers.
	notify func(chan<- os.Signal, ...os.Signal)
	// exit terminates the process.
	exit func(code int)
	// after schedules f to run once d elapses; the returned func stops
	// the timer.
	after  func(d time.Duration, f func()) func()
	stdout io.Writer
	stderr io.Writer
}

// withDefaults returns a copy with every nil or zero field set to the
// production default documented on the struct. Separate from
// shutdownOnSignals so tests can pin the defaults without registering
// real signal handlers or wiring the real os.Exit.
func (o shutdownOptions) withDefaults() shutdownOptions {
	if o.grace <= 0 {
		o.grace = defaultGracePeriod
	}
	if o.notify == nil {
		o.notify = signal.Notify
	}
	if o.exit == nil {
		o.exit = os.Exit
	}
	if o.after == nil {
		o.after = defaultAfter
	}
	if o.stdout == nil {
		o.stdout = os.Stdout
	}
	if o.stderr == nil {
		o.stderr = os.Stderr
	}
	return o
}

// shutdownOnSignals stops accepting new connections on SIGTERM/SIGINT
// and exits once in-flight requests drain. Idle keep-alive sockets and
// slow requests may outlast the grace period, so both are force-closed
// when it ends. A second signal skips the wait and exits immediately.
func shutdownOnSignals(options shutdownOptions) {
	options = options.withDefaults()

	signals := make(chan os.Signal, 2)
	options.notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		// The drain runs on its own goroutine so a second signal is
		// observed while the first shutdown is still waiting.
		shuttingDown := false
		for received := range signals {
			if shuttingDown {
				options.exit(1)
				continue
			}
			shuttingDown = true
			fmt.Fprintf(options.stdout, "%s received, shutting down gracefully\n", signalName(received))

			drained := make(chan struct{})
			go func() {
				defer close(drained)
				_ = options.server.Shutdown(context.Background())
			}()
			go func() {
				stop := options.after(options.grace, func() {
					fmt.Fprintf(options.stderr, "grace period of %dms elapsed, closing remaining connections\n", options.grace.Milliseconds())
					_ = options.server.Close()
				})
				<-drained
				stop()
				fmt.Fprintln(options.stdout, "all connections closed, exiting")
				options.exit(0)
			}()
		}
	}()
}

// defaultAfter schedules f via time.AfterFunc and returns its Stop.
func defaultAfter(d time.Duration, f func()) func() {
	timer := time.AfterFunc(d, f)
	return func() { _ = timer.Stop() }
}

// signalName renders a signal as its upper-case name, not Go's
// localized description ("terminated").
func signalName(signal os.Signal) string {
	switch signal {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	default:
		return signal.String()
	}
}
