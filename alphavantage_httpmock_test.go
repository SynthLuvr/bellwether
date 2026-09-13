package main

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jarcoal/httpmock"
)

// upstreamMock stands in for Alpha Vantage at the transport level:
// requests still target the production endpoint, but each test owns a
// fresh MockTransport with its own responders and call counters, so
// tests stay parallel-safe. Unmatched requests fail loudly through
// the NotFound responder.
type upstreamMock struct {
	transport *httpmock.MockTransport
	options   fetchDailyClosesOptions
}

func newUpstreamMock(t *testing.T) *upstreamMock {
	t.Helper()
	transport := httpmock.NewMockTransport()
	transport.RegisterNoResponder(httpmock.NewNotFoundResponder(t.Log))
	return &upstreamMock{
		transport: transport,
		options: fetchDailyClosesOptions{
			symbol:     "MSFT",
			apiKey:     "demo-key",
			outputSize: outputSizeCompact,
			client:     &http.Client{Transport: transport},
		},
	}
}

// dailyQuery pins the upstream wire contract: all four query
// parameters of the compact window.
func dailyQuery() url.Values {
	return url.Values{
		"apikey":     {"demo-key"},
		"function":   {"TIME_SERIES_DAILY"},
		"symbol":     {"MSFT"},
		"outputsize": {string(outputSizeCompact)},
	}
}

// respond serves responder for requests matching the pinned daily
// query on the production endpoint.
func (m *upstreamMock) respond(responder httpmock.Responder) {
	m.transport.RegisterResponderWithQuery(http.MethodGet, alphaVantageBaseURL, dailyQuery(), responder)
}

func (m *upstreamMock) calls() int {
	return m.transport.GetTotalCallCount()
}

// assertSeries pins the series okPayload decodes to.
func assertSeries(t *testing.T, series dailySeries) {
	t.Helper()
	if series.lastRefreshed != "2026-09-11" {
		t.Errorf("lastRefreshed = %q, want 2026-09-11", series.lastRefreshed)
	}
	want := []dailyClose{
		{Date: "2026-09-11", Close: 370.1},
		{Date: "2026-09-10", Close: 368.5},
	}
	if !slices.Equal(series.closes, want) {
		t.Errorf("closes = %+v, want %+v", series.closes, want)
	}
}

// assertUpstreamError pins the status and message fragment an
// upstream failure maps to.
func assertUpstreamError(t *testing.T, err error, status int, messagePart string) {
	t.Helper()
	var upstreamErr *alphaVantageError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %v, want alphaVantageError", err)
	}
	if upstreamErr.status != status {
		t.Errorf("status = %d, want %d", upstreamErr.status, status)
	}
	if !strings.Contains(upstreamErr.message, messagePart) {
		t.Errorf("message = %q, want it to contain %q", upstreamErr.message, messagePart)
	}
}

func TestFetchDailyClosesReturnsSeriesInOneAttempt(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	mock.respond(httpmock.NewStringResponder(http.StatusOK, okPayload()))

	series, err := fetchDailyCloses(mock.options)
	if err != nil {
		t.Fatalf("fetchDailyCloses() error = %v", err)
	}
	assertSeries(t, series)
	if got := mock.calls(); got != 1 {
		t.Errorf("upstream hit %d times, want 1", got)
	}
}

// A responder registered for the wrong query trips the NotFound
// responder: the wire contract is pinned on real outgoing requests,
// not just in buildDailyURL unit tests.
func TestFetchDailyClosesPinsUpstreamWireContract(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	wrongQuery := dailyQuery()
	wrongQuery.Set("outputsize", "full")
	mock.transport.RegisterResponderWithQuery(http.MethodGet, alphaVantageBaseURL, wrongQuery,
		httpmock.NewStringResponder(http.StatusOK, okPayload()))

	_, err := fetchDailyCloses(mock.options)

	if err == nil {
		t.Fatal("fetchDailyCloses() succeeded, want a contract mismatch failure")
	}
	if !strings.Contains(err.Error(), "Responder not found") {
		t.Errorf("error = %q, want the unmatched-request failure", err.Error())
	}
	// A transport-level miss is a network failure to the retry
	// policy: it retries exactly once before surfacing.
	if got := mock.calls(); got != 2 {
		t.Errorf("upstream hit %d times, want 2 (one retry)", got)
	}
}

// A 500 followed by success: every other retry test exhausts into
// failure, so this is the only retry-then-recover path.
func TestFetchDailyClosesRetries500ThenSucceeds(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	mock.respond(httpmock.ResponderFromMultipleResponses(
		[]*http.Response{
			httpmock.NewStringResponse(http.StatusInternalServerError, "boom"),
			httpmock.NewStringResponse(http.StatusOK, okPayload()),
		},
		t.Log,
	))

	series, err := fetchDailyCloses(mock.options)
	if err != nil {
		t.Fatalf("fetchDailyCloses() error = %v", err)
	}
	assertSeries(t, series)
	if got := mock.calls(); got != 2 {
		t.Errorf("upstream hit %d times, want 2 (one retry)", got)
	}
}

func TestFetchDailyClosesRetriesHTTP500OnceThenMapsTo502(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	mock.respond(httpmock.NewStringResponder(http.StatusInternalServerError, "boom"))

	started := time.Now()
	_, err := fetchDailyCloses(mock.options)
	elapsed := time.Since(started)

	assertUpstreamError(t, err, http.StatusBadGateway, "HTTP 500")
	if got := mock.calls(); got != 2 {
		t.Errorf("upstream hit %d times, want 2", got)
	}
	// The single retry is delayed by the ~300ms backoff.
	if elapsed < 250*time.Millisecond {
		t.Errorf("took %v, want >= 250ms of backoff", elapsed)
	}
}

func TestFetchDailyClosesRetriesNetworkFailureOnce(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	mock.respond(httpmock.NewErrorResponder(errors.New("connection reset by peer")))

	_, err := fetchDailyCloses(mock.options)

	if _, ok := errors.AsType[*alphaVantageError](err); ok {
		t.Fatalf("error = %v, want a raw transport error", err)
	}
	if err == nil {
		t.Fatal("fetchDailyCloses() succeeded, want network error")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("error = %v, want the wrapped cause", err)
	}
	if got := mock.calls(); got != 2 {
		t.Errorf("upstream hit %d times, want 2", got)
	}
	// The wrapped error must not carry the upstream URL (and API key).
	if strings.Contains(err.Error(), alphaVantageBaseURL) || strings.Contains(err.Error(), "demo-key") {
		t.Errorf("error %q leaks the upstream URL", err)
	}
}

// httpmock's unmatched-request error embeds the full URL — API key
// included — in its own message, so the surfaced error must keep the
// diagnostic while carrying no trace of the URL or key.
func TestFetchDailyClosesRedactsUnmatchedRequestError(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)

	_, err := fetchDailyCloses(mock.options)

	if err == nil {
		t.Fatal("fetchDailyCloses() succeeded, want the unmatched-request failure")
	}
	if !strings.Contains(err.Error(), "network error: Responder not found") {
		t.Errorf("error = %q, want the unmatched-request failure", err.Error())
	}
	for _, secret := range []string{"demo-key", alphaVantageBaseURL, "www.alphavantage.co"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks %q", err.Error(), secret)
		}
	}
	if got := mock.calls(); got != 2 {
		t.Errorf("upstream hit %d times, want 2 (one retry)", got)
	}
}

func TestFetchDailyClosesKeepsHTTP429SingleShot(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	mock.respond(httpmock.NewStringResponder(http.StatusTooManyRequests, ""))

	_, err := fetchDailyCloses(mock.options)

	assertUpstreamError(t, err, http.StatusBadGateway, "HTTP 429")
	if got := mock.calls(); got != 1 {
		t.Errorf("upstream hit %d times, want 1", got)
	}
}

// Alpha Vantage's quota-exceeded notices can embed the caller's API
// key in the message text; the surfaced message must not carry it to
// clients. The daily-quota wording arrives under Information, so it
// must also map to 429, not the generic 503.
func TestFetchDailyClosesRedactsAPIKeyFromQuotaInformation(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	payload := `{"Information": "We have detected your API key as demo-key ` +
		`and our standard API rate limit is 25 requests per day."}`
	mock.respond(httpmock.NewStringResponder(http.StatusOK, payload))

	_, err := fetchDailyCloses(mock.options)

	assertUpstreamError(t, err, http.StatusTooManyRequests, "rate limit")
	if strings.Contains(err.Error(), "demo-key") {
		t.Errorf("error message %q contains the API key", err.Error())
	}
}

func TestFetchDailyClosesSurfacesGenericInformationAs503(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	// Not a rate-limit notice: demo-key and premium-tier restrictions
	// keep the informational 503.
	mock.respond(httpmock.NewStringResponder(http.StatusOK,
		`{"Information": "The **demo** API key is for demo purposes only."}`))

	_, err := fetchDailyCloses(mock.options)

	assertUpstreamError(t, err, http.StatusServiceUnavailable, "demo purposes")
}

func TestFetchDailyClosesSurfacesRateLimitNoteAs429(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	mock.respond(httpmock.NewStringResponder(http.StatusOK, `{"Note": "API call frequency is 25 per day."}`))

	_, err := fetchDailyCloses(mock.options)

	assertUpstreamError(t, err, http.StatusTooManyRequests, "25 per day")
	if got := mock.calls(); got != 1 {
		t.Errorf("upstream hit %d times, want 1", got)
	}
}

func TestFetchDailyClosesCapsRetried503RetryAfterAtOneSecond(t *testing.T) {
	t.Parallel()
	mock := newUpstreamMock(t)
	mock.respond(httpmock.NewStringResponder(http.StatusServiceUnavailable, "").
		HeaderAdd(http.Header{"Retry-After": {"30"}}))

	started := time.Now()
	_, err := fetchDailyCloses(mock.options)
	elapsed := time.Since(started)

	assertUpstreamError(t, err, http.StatusBadGateway, "HTTP 503")
	if got := mock.calls(); got != 2 {
		t.Errorf("upstream hit %d times, want 2", got)
	}
	// Retry-After: 30 is capped at 1s by maxRetryAfter.
	if elapsed < 900*time.Millisecond {
		t.Errorf("took %v, want >= ~1s capped Retry-After delay", elapsed)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("took %v, want the Retry-After capped well below 30s", elapsed)
	}
}

func TestFetchDailyClosesIgnoresRetryAfterOn500(t *testing.T) {
	t.Parallel()
	// Retry-After is consulted only for a retried 503, so a 500 with
	// the header retries on the fixed backoff instead of honoring a
	// 30s Retry-After.
	mock := newUpstreamMock(t)
	mock.respond(httpmock.NewStringResponder(http.StatusInternalServerError, "").
		HeaderAdd(http.Header{"Retry-After": {"30"}}))

	started := time.Now()
	_, err := fetchDailyCloses(mock.options)
	elapsed := time.Since(started)

	assertUpstreamError(t, err, http.StatusBadGateway, "HTTP 500")
	if got := mock.calls(); got != 2 {
		t.Errorf("upstream hit %d times, want 2", got)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("took %v, want the fixed ~300ms backoff", elapsed)
	}
}
