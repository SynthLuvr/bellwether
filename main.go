// Package main implements bellwether, a web service that looks up a
// fixed number of closing prices of a specific stock.
package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

func main() {
	// Optional .env in the working directory; existing environment
	// variables always win (godotenv.Load never overrides).
	_ = godotenv.Load()
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.LookupEnv))
	}
	os.Exit(serve(os.LookupEnv, os.Stdout, os.Stderr, nil, net.Listen))
}

// serve loads configuration, binds the configured port, and runs the
// service until shutdown, returning the process exit code. The
// collaborators are injected so tests can drive the full lifecycle;
// main wires the real ones.
func serve(
	getenv func(string) (string, bool),
	stdout, stderr io.Writer,
	notify func(chan<- os.Signal, ...os.Signal),
	listen func(network, address string) (net.Listener, error),
) int {
	cfg, err := loadConfig(getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	log := newDefaultLogger(stdout)
	listener, err := listen("tcp", net.JoinHostPort("", strconv.Itoa(cfg.port)))
	if err != nil {
		log.Log(slog.LevelError, "listen failed",
			logField{Key: "error", Value: err.Error()})
		return 1
	}
	return run(cfg, listener, log, notify, nil)
}

// serveFunc serves HTTP on a listener until the server shuts down; the
// production value is (*http.Server).Serve. Tests substitute fakes to
// drive unexpected serve failures deterministically.
type serveFunc func(server *http.Server, listener net.Listener) error

// run serves HTTP traffic on the already-bound listener, announces
// readiness, and blocks until a signal-driven shutdown completes or
// the listener dies unexpectedly. Receiving the listener rather than
// binding here lets callers hand over a port they already hold — the
// standard way for a test to pick an ephemeral port without a
// find-then-release race.
func run(
	cfg config,
	listener net.Listener,
	log logger,
	notify func(chan<- os.Signal, ...os.Signal),
	serveHTTP serveFunc,
) int {
	if serveHTTP == nil {
		serveHTTP = (*http.Server).Serve
	}
	server := &http.Server{
		Handler: newApp(appOptions{
			symbol:   cfg.symbol,
			ndays:    cfg.ndays,
			apiKey:   cfg.apiKey,
			cacheTTL: cfg.cacheTTL,
		}, log),
		// Bounded header reads (60s), an overall request cap (5
		// minutes), and short idle keep-alives (5s).
		ReadHeaderTimeout: 60 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       5 * time.Second,
	}

	exitCode := make(chan int, 1)
	exit := forwardExitCode(exitCode)
	// A listener that dies outside shutdown must end the process — it
	// would otherwise hang, alive but unable to accept connections — so
	// the supervisor (Kubernetes, Docker, systemd) restarts it.
	// ErrServerClosed is the expected outcome of Shutdown and Close,
	// not a failure.
	go func() {
		if err := serveHTTP(server, listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Log(slog.LevelError, "serve failed", logField{Key: "error", Value: err.Error()})
			exit(1)
		}
	}()
	shutdownOnSignals(shutdownOptions{
		server: server,
		notify: notify,
		exit:   exit,
	})

	port := listener.Addr().(*net.TCPAddr).Port
	log.Log(slog.LevelInfo, "listening",
		logField{Key: "port", Value: port},
		logField{Key: "symbol", Value: cfg.symbol},
		logField{Key: "ndays", Value: cfg.ndays},
		logField{Key: "cacheTtlSeconds", Value: int(cfg.cacheTTL.Seconds())},
	)
	return <-exitCode
}

// forwardExitCode returns a shutdown exit callback that delivers at
// most one code to dst and drops the rest. run returns after the
// first delivery, so a second report — the graceful drain's 0 racing
// a second signal's 1 — must not park on the full buffered channel;
// the first code is the only one anyone will ever read.
func forwardExitCode(dst chan int) func(code int) {
	return func(code int) {
		select {
		case dst <- code:
		default:
		}
	}
}
