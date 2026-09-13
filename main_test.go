package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServeFailsFastOnInvalidConfiguration(t *testing.T) {
	t.Parallel()
	var stdout, stderr syncBuffer

	code := serve(envGetter(map[string]string{
		// SYMBOL/NDAYS/APIKEY all missing: loadConfig must reject.
		"PORT": "8080",
	}), &stdout, &stderr, nil, net.Listen)

	if code != 1 {
		t.Errorf("serve() = %d, want 1", code)
	}
	if message := stderr.String(); !strings.Contains(message, "SYMBOL") {
		t.Errorf("stderr = %q, want the configuration problems", message)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing logged", stdout.String())
	}
}

// busyPort keeps a TCP port occupied so a second listener on it fails
// with "address already in use".
func busyPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().(*net.TCPAddr).Port
}

func TestServeReportsListenFailure(t *testing.T) {
	t.Parallel()
	var stdout syncBuffer

	// A no-op notify keeps even this failure path from ever wiring the
	// real signal handlers; serve returns before shutdown installs.
	code := serve(envGetter(map[string]string{
		"SYMBOL": "MSFT",
		"NDAYS":  "5",
		"APIKEY": "demo-key",
		"PORT":   strconv.Itoa(busyPort(t)),
	}), &stdout, &syncBuffer{}, func(chan<- os.Signal, ...os.Signal) {}, net.Listen)

	if code != 1 {
		t.Errorf("serve() = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), `"msg":"listen failed"`) {
		t.Errorf("stdout = %q, want a listen failed entry", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"level":"ERROR"`) {
		t.Errorf("stdout = %q, want the failure logged at error", stdout.String())
	}
}

func TestForwardExitCodeDropsOverflowReports(t *testing.T) {
	t.Parallel()
	dst := make(chan int, 1)
	exit := forwardExitCode(dst)

	// Both shutdown paths may report — the graceful drain's 0 and a
	// second signal's 1 — and the loser must be dropped, not parked on
	// the full channel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		exit(0)
		exit(1)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second exit report blocked on the full channel")
	}

	if code := <-dst; code != 0 {
		t.Errorf("first code = %d, want 0", code)
	}
	select {
	case code := <-dst:
		t.Errorf("dropped code %d was delivered, want the channel empty", code)
	default:
	}
}

func TestRunExitsWhenServeFailsUnexpectedly(t *testing.T) {
	t.Parallel()
	var log captureLogger

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// A no-op notify keeps real signal handlers out of the test
	// binary; run returns via the serve failure, not a signal.
	code := run(config{symbol: "MSFT", ndays: 5, apiKey: "k"}, listener, &log,
		func(chan<- os.Signal, ...os.Signal) {},
		func(_ *http.Server, _ net.Listener) error { return errors.New("accept: broken pipe") })

	if code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	// The serve goroutine races run's own listening line, so locate the
	// entry instead of assuming its position.
	entries := log.entries()
	i := slices.IndexFunc(entries, func(entry capturedLog) bool { return entry.msg == "serve failed" })
	if i < 0 {
		t.Fatalf("logs = %+v, want a serve failed entry", entries)
	}
	if entries[i].level != slog.LevelError {
		t.Errorf("serve failure logged at %v, want error", entries[i].level)
	}
	if message, ok := entries[i].field("error"); !ok || message == "" {
		t.Errorf("error field = %v, want the serve error text", message)
	}
}

func TestServeBootsListensProbesHealthAndShutsDown(t *testing.T) {
	t.Parallel()
	var stdout syncBuffer

	// The notify double proves SIGTERM/SIGINT registration and lets the
	// test deliver the shutdown signal itself.
	signals := make(chan os.Signal, 1)
	registered := make(chan []os.Signal, 1)
	notify := func(ch chan<- os.Signal, sigs ...os.Signal) {
		registered <- append([]os.Signal(nil), sigs...)
		go func() {
			for sig := range signals {
				ch <- sig
			}
		}()
	}

	// The listener is bound once and handed to serve through an
	// injected listen func, so there is no find-a-free-port, release,
	// rebind window for another process to steal the port from under
	// the test.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	handOver := func(string, string) (net.Listener, error) { return listener, nil }

	exitCode := make(chan int, 1)
	go func() {
		exitCode <- serve(envGetter(map[string]string{
			"SYMBOL": "MSFT",
			"NDAYS":  "5",
			"APIKEY": "demo-key",
			"PORT":   strconv.Itoa(listener.Addr().(*net.TCPAddr).Port),
		}), &stdout, &syncBuffer{}, notify, handOver)
	}()

	wantSignals := []os.Signal{syscall.SIGTERM, syscall.SIGINT}
	gotSignals := <-registered
	if len(gotSignals) != len(wantSignals) || gotSignals[0] != wantSignals[0] || gotSignals[1] != wantSignals[1] {
		t.Fatalf("registered signals = %v, want %v", gotSignals, wantSignals)
	}

	// The listening line carries the actual bound port, then /health
	// must answer on it.
	port := awaitListeningPort(t, &stdout, exitCode)
	probeHealth(t, port)

	select {
	case code := <-exitCode:
		t.Fatalf("serve() returned %d before any signal", code)
	case <-time.After(50 * time.Millisecond):
	}

	// SIGTERM triggers the graceful shutdown path and exits 0.
	signals <- syscall.SIGTERM
	select {
	case code := <-exitCode:
		if code != 0 {
			t.Errorf("serve() = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not return after SIGTERM")
	}
}

func awaitListeningPort(t *testing.T, out *syncBuffer, exitCode <-chan int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case code := <-exitCode:
			t.Fatalf("serve() exited early with %d before listening", code)
		default:
		}
		for line := range strings.SplitSeq(out.String(), "\n") {
			if !strings.Contains(line, `"msg":"listening"`) {
				continue
			}
			var entry struct {
				Port            int    `json:"port"`
				Symbol          string `json:"symbol"`
				NDays           int    `json:"ndays"`
				CacheTTLSeconds int    `json:"cacheTtlSeconds"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &entry); err != nil {
				t.Fatalf("listening line %q is not JSON: %v", line, err)
			}
			if entry.Port == 0 || entry.Symbol != "MSFT" || entry.NDays != 5 {
				t.Fatalf("listening line = %+v, want port/symbol/ndays", entry)
			}
			if entry.CacheTTLSeconds != 3600 {
				t.Fatalf("listening line cacheTtlSeconds = %d, want the 1h default", entry.CacheTTLSeconds)
			}
			return entry.Port
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no listening line appeared: %q", out.String())
	return 0
}

func probeHealth(t *testing.T, port int) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://localhost:" + strconv.Itoa(port) + "/health"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("GET /health never answered 200")
}

func TestHealthcheckExitsZeroWhenHealthy(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	t.Cleanup(server.Close)
	port := strconv.Itoa(mustPort(t, server.URL))

	if code := healthcheck(func(string) (string, bool) { return port, true }); code != 0 {
		t.Errorf("healthcheck() = %d, want 0", code)
	}
}

func TestHealthcheckExitsOneOnUnhealthyStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	port := strconv.Itoa(mustPort(t, server.URL))

	if code := healthcheck(func(string) (string, bool) { return port, true }); code != 1 {
		t.Errorf("healthcheck() = %d, want 1", code)
	}
}

func TestHealthcheckExitsOneWhenRefused(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	port := strconv.Itoa(mustPort(t, server.URL))
	server.Close() // the port is now dead: connection refused

	if code := healthcheck(func(string) (string, bool) { return port, true }); code != 1 {
		t.Errorf("healthcheck() = %d, want 1", code)
	}
}

func TestHealthcheckExitsOneOnInvalidPort(t *testing.T) {
	t.Parallel()
	if code := healthcheck(func(string) (string, bool) { return "http", true }); code != 1 {
		t.Errorf("healthcheck() = %d, want 1", code)
	}
}

func TestHealthcheckPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw     string
		present bool
		want    int
		wantOK  bool
	}{
		{raw: "", present: true, want: defaultPort, wantOK: true},
		{raw: "  ", present: true, want: defaultPort, wantOK: true},
		{raw: "8080", present: true, want: 8080, wantOK: true},
		{raw: "8080", present: false, want: defaultPort, wantOK: true},
		{raw: "0", present: true, wantOK: false},
		{raw: "70000", present: true, wantOK: false},
		{raw: "http", present: true, wantOK: false},
		{raw: "2.5", present: true, wantOK: false},
	}
	for _, tt := range tests {
		got, ok := healthcheckPort(tt.raw, tt.present)
		if ok != tt.wantOK {
			t.Errorf("healthcheckPort(%q, %v) ok = %v, want %v", tt.raw, tt.present, ok, tt.wantOK)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("healthcheckPort(%q, %v) = %d, want %d", tt.raw, tt.present, got, tt.want)
		}
	}
}

func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", rawURL, err)
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return value
}
