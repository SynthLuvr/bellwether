package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

// upstreamSpy stands in for the real Alpha Vantage API at the network
// boundary, recording every requested URL so tests can count upstream
// attempts.
type upstreamSpy struct {
	mu        sync.Mutex
	requested []string
	handler   http.HandlerFunc
}

func (s *upstreamSpy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requested = append(s.requested, r.URL.String())
	s.mu.Unlock()
	s.handler(w, r)
}

func (s *upstreamSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requested)
}

func (s *upstreamSpy) firstURL() *url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requested) == 0 {
		return nil
	}
	return mustURL(s.requested[0])
}

// newUpstream boots a recording Alpha Vantage double and returns the
// spy plus client options pointed at it.
func newUpstream(t *testing.T, handler http.HandlerFunc) (*upstreamSpy, fetchDailyClosesOptions) {
	t.Helper()
	spy := &upstreamSpy{handler: handler}
	server := httptest.NewServer(spy)
	t.Cleanup(server.Close)
	return spy, fetchDailyClosesOptions{
		symbol:     "MSFT",
		apiKey:     "demo-key",
		outputSize: outputSizeCompact,
		baseURL:    mustURL(server.URL),
	}
}

func okPayload() string {
	return `{
		"Meta Data": {"3. Last Refreshed": "2026-09-11"},
		"Time Series (Daily)": {
			"2026-09-11": {"4. close": "370.1000"},
			"2026-09-10": {"4. close": "368.5000"}
		}
	}`
}

func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func statusHandler(status int, header http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for key, values := range header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
	}
}

// dropConnection hijacks and closes the socket: the client sees a
// transport-level failure, never an HTTP response.
func dropConnection(w http.ResponseWriter, _ *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("test server does not support hijacking")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

func TestRequestTimeoutIsFiveSeconds(t *testing.T) {
	t.Parallel()
	if requestTimeout != 5*time.Second {
		t.Errorf("requestTimeout = %v, want a deliberate 5s", requestTimeout)
	}
}

// slowBodyHandler sends the headers immediately and delivers the
// payload only after delay, so the client must keep reading a body
// that trails its headers.
func slowBodyHandler(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(delay)
		fmt.Fprint(w, okPayload())
	}
}

// assertTerminalTimeout pins the timeout contract: the exchange fails
// fast with DeadlineExceeded and is never retried.
func assertTerminalTimeout(t *testing.T, err error, elapsed time.Duration, attempts int) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("took %v, want under 2s", elapsed)
	}
	if attempts != 1 {
		t.Errorf("upstream hit %d times, want 1 (timeouts are single-shot)", attempts)
	}
}

func TestFetchDailyClosesAbortsStalledUpstreamAtTimeout(t *testing.T) {
	t.Parallel()
	spy, options := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, okPayload())
	})
	options.timeout = 100 * time.Millisecond

	started := time.Now()
	_, err := fetchDailyCloses(options)

	assertTerminalTimeout(t, err, time.Since(started), spy.count())
}

// A body that trails its headers must still decode: canceling the
// context at header time made every multi-packet response fail with
// "context canceled" — the failure seen against real Alpha Vantage.
func TestFetchDailyClosesDecodesBodyArrivingAfterHeaders(t *testing.T) {
	t.Parallel()
	spy, options := newUpstream(t, slowBodyHandler(200*time.Millisecond))

	series, err := fetchDailyCloses(options)
	if err != nil {
		t.Fatalf("fetchDailyCloses() error = %v", err)
	}
	if series.lastRefreshed != "2026-09-11" {
		t.Errorf("lastRefreshed = %q, want 2026-09-11", series.lastRefreshed)
	}
	if spy.count() != 1 {
		t.Errorf("upstream hit %d times, want 1", spy.count())
	}
}

func TestFetchDailyClosesAbortsStalledBodyAtTimeout(t *testing.T) {
	t.Parallel()
	spy, options := newUpstream(t, slowBodyHandler(1500*time.Millisecond))
	options.timeout = 100 * time.Millisecond

	started := time.Now()
	_, err := fetchDailyCloses(options)

	assertTerminalTimeout(t, err, time.Since(started), spy.count())
}

// failingCloser is an io.ReadCloser whose Close always fails.
type failingCloser struct {
	io.ReadCloser
	err error
}

func (f *failingCloser) Close() error { return f.err }

// Close must cancel the context before closing the body, and a close
// failure must still surface, wrapped.
func TestCancelingBodyCloseCancelsBeforeClosing(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("close failed")
	canceled := false
	body := &cancelingBody{
		ReadCloser: &failingCloser{
			ReadCloser: io.NopCloser(strings.NewReader("")),
			err:        closeErr,
		},
		cancel: func() { canceled = true },
	}

	err := body.Close()
	if !canceled {
		t.Error("context not canceled by Close")
	}
	if !errors.Is(err, closeErr) {
		t.Errorf("error = %v, want wrapping %v", err, closeErr)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	future := time.Now().UTC().Add(2 * time.Second).Format(http.TimeFormat)
	tests := []struct {
		value    string
		want     time.Duration
		wantOK   bool
		minDelay bool // want a strictly positive delay
	}{
		{value: "", wantOK: false},
		{value: "junk", wantOK: false},
		{value: "+5", wantOK: false},  // ky's ^\d+$ rejects signs
		{value: "5.5", wantOK: false}, // and decimals
		{value: "0", want: 0, wantOK: true},
		{value: "30", want: 30 * time.Second, wantOK: true},
		{value: "Mon, 02 Jan 2006 15:04:05 GMT", want: 0, wantOK: true}, // past clamps to 0
		{value: future, wantOK: true, minDelay: true},
	}
	for _, tt := range tests {
		got, ok := parseRetryAfter(tt.value)
		if ok != tt.wantOK {
			t.Errorf("parseRetryAfter(%q) ok = %v, want %v", tt.value, ok, tt.wantOK)
			continue
		}
		if !tt.wantOK {
			continue
		}
		if tt.minDelay {
			if got <= 0 {
				t.Errorf("parseRetryAfter(%q) = %v, want positive", tt.value, got)
			}
		} else if got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

// A transport cause may print any rendering of the request URL or
// API key; none may survive the surfaced text, and the cause stays
// reachable through Unwrap.
func TestNewNetworkErrorRedactsEverySecretRendering(t *testing.T) {
	t.Parallel()
	const apiKey = "s3cr3t key&="
	request := buildDailyURL(fetchDailyClosesOptions{apiKey: apiKey})
	bare := *request
	bare.RawQuery = ""
	renderings := []string{
		request.String(),
		bare.String(),
		apiKey,
		url.QueryEscape(apiKey),
		url.PathEscape(apiKey),
	}
	for _, rendering := range renderings {
		cause := fmt.Errorf("transport died mid-exchange to %s", rendering)
		err := newNetworkError(cause, request, apiKey)
		if strings.Contains(err.Error(), rendering) {
			t.Errorf("message %q leaks %q", err.Error(), rendering)
		}
		if !strings.Contains(err.Error(), redacted) {
			t.Errorf("message = %q, want the redaction marker", err.Error())
		}
		if !errors.Is(err, cause) {
			t.Errorf("errors.Is(%v, cause) = false, want the cause preserved for errors.Is/As", err)
		}
	}
}

// A secret-free cause passes through verbatim, and an empty API key
// contributes no (empty) secrets to scrub.
func TestNewNetworkErrorPassesThroughSecretFreeText(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection reset by peer")
	err := newNetworkError(cause, mustURL("https://upstream.test/query"), "")
	if want := "network error: connection reset by peer"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// Longest secret first: redacting the key before the URL that embeds
// it would leave the URL's non-secret remainder scattered through the
// message.
func TestRedactSecretsCollapsesLongestFirst(t *testing.T) {
	t.Parallel()
	got := redactSecrets("GET https://x.test/q?k=1 then k=1", "k=1", "https://x.test/q?k=1")
	if want := "GET [redacted] then [redacted]"; got != want {
		t.Errorf("redactSecrets() = %q, want %q", got, want)
	}
}

func TestBuildDailyURLEncodesParameters(t *testing.T) {
	t.Parallel()
	options := fetchDailyClosesOptions{
		symbol:     "BRK.B",
		apiKey:     "a b&c",
		outputSize: outputSizeFull,
		baseURL:    mustURL("https://example.test/base"),
	}
	got := buildDailyURL(options)
	if got.Scheme != "https" || got.Host != "example.test" || got.Path != "/base" {
		t.Errorf("URL base = %s, want https://example.test/base", got)
	}
	query := got.Query()
	for key, want := range map[string]string{
		"symbol":     "BRK.B",
		"apikey":     "a b&c",
		"function":   "TIME_SERIES_DAILY",
		"outputsize": "full",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestBuildDailyURLDefaultsToAlphaVantage(t *testing.T) {
	t.Parallel()
	got := buildDailyURL(fetchDailyClosesOptions{})
	if got.String() != "https://www.alphavantage.co/query?apikey=&function=TIME_SERIES_DAILY&outputsize=&symbol=" {
		t.Errorf("URL = %s, want the default Alpha Vantage endpoint", got)
	}
}
