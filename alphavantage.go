package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// alphaVantageBaseURL is the production TIME_SERIES_DAILY endpoint.
	alphaVantageBaseURL = "https://www.alphavantage.co/query"
	// requestTimeout pins the per-attempt outbound timeout: a stalled
	// upstream must fail fast.
	requestTimeout = 5 * time.Second
)

// defaultEndpoint is alphaVantageBaseURL pre-parsed; the constant URL
// cannot fail to parse.
var defaultEndpoint, _ = url.Parse(alphaVantageBaseURL)

// Retry policy: exactly one retry for transport-level failures only —
// network errors and the 5xx statuses in retryStatusCodes. 429 stays
// single-shot so a retry cannot re-hit a rate limit; a retried 503's
// Retry-After is capped at 1s; anything else backs off ~300ms.
// Timeouts are terminal rather than retried.
const (
	retryLimit    = 1
	retryBackoff  = 300 * time.Millisecond
	maxRetryAfter = time.Second
)

var retryStatusCodes = []int{500, 502, 503, 504}

// outputSize selects Alpha Vantage's TIME_SERIES_DAILY window.
type outputSize string

const (
	outputSizeCompact outputSize = "compact"
	outputSizeFull    outputSize = "full"
)

// dailyClose is one dated closing price.
type dailyClose struct {
	Date  string  `json:"date"`
	Close float64 `json:"close"`
}

// dailySeries is the parsed TIME_SERIES_DAILY payload: newest first.
type dailySeries struct {
	lastRefreshed string
	closes        []dailyClose
}

// alphaVantageError maps an upstream problem to the HTTP status the
// service answers with: 429 (rate limit), 502 (bad call, or a payload
// that decodes but holds no usable series), or 503 (informational
// notice). A payload that fails to decode as JSON at all is not an
// alphaVantageError — it surfaces as a 500 from the handler.
type alphaVantageError struct {
	status  int
	message string
}

func (e *alphaVantageError) Error() string {
	return e.message
}

// fetchDailyClosesOptions parameterizes one upstream call.
type fetchDailyClosesOptions struct {
	symbol     string
	apiKey     string
	outputSize outputSize
	// timeout is the per-attempt outbound timeout; zero uses
	// requestTimeout. Tests shorten it to exercise timeout behavior
	// without real-time waits.
	timeout time.Duration
	// baseURL overrides the Alpha Vantage endpoint; tests point it at
	// an httptest.Server. Nil uses alphaVantageBaseURL.
	baseURL *url.URL
	// client overrides the pooled upstream HTTP client; tests attach
	// a mock transport to it. Nil uses upstreamClient.
	client *http.Client
}

// upstreamClient pools connections for outbound calls.
var upstreamClient = &http.Client{}

// fetchDailyCloses retrieves the daily closing series for symbol,
// applying the retry/timeout policy and mapping upstream failures to
// alphaVantageError values.
func fetchDailyCloses(options fetchDailyClosesOptions) (dailySeries, error) {
	response, err := requestDailyCloses(options)
	if err != nil {
		return dailySeries{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var payload dailyResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return dailySeries{}, fmt.Errorf("decode daily response: %w", err)
	}
	series, err := parseDailySeries(payload)
	if err != nil {
		// Upstream messages are surfaced to clients verbatim, and
		// Alpha Vantage's quota notices embed the caller's API key in
		// the text; scrub it here, where the key is known.
		if upstreamErr, ok := errors.AsType[*alphaVantageError](err); ok {
			upstreamErr.message = redactSecrets(upstreamErr.message, options.apiKey)
		}
		return dailySeries{}, err
	}
	return series, nil
}

// buildDailyURL assembles the TIME_SERIES_DAILY query URL.
func buildDailyURL(options fetchDailyClosesOptions) *url.URL {
	base := options.baseURL
	if base == nil {
		base = defaultEndpoint
	}
	target := *base
	query := url.Values{}
	query.Set("apikey", options.apiKey)
	query.Set("function", "TIME_SERIES_DAILY")
	query.Set("symbol", options.symbol)
	query.Set("outputsize", string(options.outputSize))
	target.RawQuery = query.Encode()
	return &target
}

// requestDailyCloses performs the HTTP exchange with exactly one retry
// for retriable failures. Non-2xx responses (including HTTP 429) map
// to a 502 alphaVantageError single-shot; every 5xx that survives its
// retry maps the same way. Timeouts are terminal; network errors are
// retried once, then surfaced as a redacted networkError.
func requestDailyCloses(options fetchDailyClosesOptions) (*http.Response, error) {
	timeout := options.timeout
	if timeout <= 0 {
		timeout = requestTimeout
	}
	request := &http.Request{
		Method: http.MethodGet,
		URL:    buildDailyURL(options),
		Header: make(http.Header),
	}
	client := cmp.Or(options.client, upstreamClient)
	for attempt := 0; ; attempt++ {
		// Deliberately detached from any inbound request's context:
		// the fetch backs the shared single-flight cache, so one
		// client's disconnect must not cancel a load that concurrent
		// callers are already waiting on. The per-attempt timeout
		// above bounds the exchange instead.
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		response, err := client.Do(request.Clone(ctx))
		if err != nil {
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("request timed out after %s: %w", timeout, context.DeadlineExceeded)
			}
			if attempt < retryLimit {
				time.Sleep(retryBackoff)
				continue
			}
			// *url.Error's message embeds the full URL; unwrap to the
			// cause and let newNetworkError redact whatever it prints.
			if urlErr, ok := errors.AsType[*url.Error](err); ok {
				err = urlErr.Err
			}
			return nil, newNetworkError(err, request.URL, options.apiKey)
		}
		// cancel stays with the body: Do returns once the headers
		// arrive, but the per-attempt timeout must cover the body too.
		response.Body = &cancelingBody{ReadCloser: response.Body, cancel: cancel}
		status := response.StatusCode
		if status < 200 || status >= 300 {
			if attempt < retryLimit && slices.Contains(retryStatusCodes, status) {
				delay := retryDelay(response)
				drainAndClose(response)
				time.Sleep(delay)
				continue
			}
			drainAndClose(response)
			return nil, httpFailure(status)
		}
		return response, nil
	}
}

// isRateLimitNotice reports whether an Information body is a
// rate/quota notice rather than a generic informational one: Alpha
// Vantage serves quota exhaustion under Information (not only the
// legacy Note field) with "rate limit" or the older "call frequency"
// wording, and only those keep retry-later semantics.
func isRateLimitNotice(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "rate limit") || strings.Contains(message, "call frequency")
}

// redacted replaces every scrubbed secret in surfaced error text.
const redacted = "[redacted]"

// networkError surfaces a transport-level failure with its text
// scrubbed of the request URL and API key: the cause — a dial, TLS,
// or test-double error — may print the URL, whose query carries the
// key. Unwrap keeps the cause intact for errors.Is/As; only Error's
// text is guaranteed secret-free.
type networkError struct {
	cause   error
	message string
}

func (e *networkError) Error() string { return e.message }

func (e *networkError) Unwrap() error { return e.cause }

// newNetworkError wraps a transport failure, scrubbing every rendering
// of the request URL (with and without its query) and the API key
// (raw, query-, and path-escaped) from the surfaced message.
func newNetworkError(cause error, requestURL *url.URL, apiKey string) *networkError {
	bare := *requestURL
	bare.RawQuery = ""
	return &networkError{
		cause: cause,
		message: redactSecrets("network error: "+cause.Error(),
			requestURL.String(),
			bare.String(),
			apiKey,
			url.QueryEscape(apiKey),
			url.PathEscape(apiKey)),
	}
}

// redactSecrets replaces every occurrence of each non-empty secret
// with the redaction marker, longest secrets first so a URL collapses
// as a whole before its embedded key is redacted piecemeal. A short
// key can collide with ordinary message words (the public "demo" key
// scrubs every "demo" it passes over); that over-redaction is
// deliberate — under-redaction is the dangerous direction, and real
// keys are long random strings that collide with nothing.
func redactSecrets(message string, secrets ...string) string {
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, redacted)
	}
	return message
}

// httpFailure maps a non-ok upstream status to the 502 the service
// answers with.
func httpFailure(status int) *alphaVantageError {
	return &alphaVantageError{
		status:  http.StatusBadGateway,
		message: "Alpha Vantage request failed with HTTP " + strconv.Itoa(status),
	}
}

// retryDelay resolves the pause before the single retry. Only a 503
// says the server timed itself out, so only its Retry-After is
// honored (capped at maxRetryAfter); every other retriable status
// uses the fixed backoff — a 500 with Retry-After included.
func retryDelay(response *http.Response) time.Duration {
	if response.StatusCode == http.StatusServiceUnavailable {
		if after, ok := parseRetryAfter(response.Header.Get("Retry-After")); ok {
			return min(maxRetryAfter, after)
		}
	}
	return retryBackoff
}

// parseRetryAfter parses a Retry-After header: plain non-negative
// integer seconds, or an IMF-fixdate HTTP date; anything else is
// malformed (!ok) and loses its server-provided timing. Past dates
// clamp to zero.
func parseRetryAfter(value string) (time.Duration, bool) {
	if value == "" || !isDigits(value) {
		if timestamp, err := time.Parse(http.TimeFormat, value); err == nil {
			return max(0, time.Until(timestamp)), true
		}
		return 0, false
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// drainAndClose consumes and closes a response body so the connection
// returns to the pool.
func drainAndClose(response *http.Response) {
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

// cancelingBody keeps a per-attempt timeout context alive until the
// response body is closed. Client.Do returns as soon as the headers
// arrive; canceling there would fail every body read not already
// buffered with "context canceled", so reads must keep the context
// alive and let the timeout bound the whole exchange.
type cancelingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

// Close cancels the context before closing the underlying body.
func (b *cancelingBody) Close() error {
	b.cancel()
	if err := b.ReadCloser.Close(); err != nil {
		return fmt.Errorf("close response body: %w", err)
	}
	return nil
}

// dailyResponse is the subset of Alpha Vantage's JSON payload the
// service reads.
type dailyResponse struct {
	MetaData *struct {
		LastRefreshed string `json:"3. Last Refreshed"`
	} `json:"Meta Data"`
	TimeSeries   map[string]map[string]string `json:"Time Series (Daily)"`
	ErrorMessage string                       `json:"Error Message"`
	Note         string                       `json:"Note"`
	Information  string                       `json:"Information"`
}

// parseDailySeries validates a decoded payload and extracts the sorted
// daily closes, mapping Alpha Vantage's error conventions to
// alphaVantageError statuses.
func parseDailySeries(payload dailyResponse) (dailySeries, error) {
	if payload.ErrorMessage != "" {
		return dailySeries{}, &alphaVantageError{status: http.StatusBadGateway, message: payload.ErrorMessage}
	}
	if payload.Note != "" {
		return dailySeries{}, &alphaVantageError{status: http.StatusTooManyRequests, message: payload.Note}
	}
	if payload.Information != "" {
		status := http.StatusServiceUnavailable
		if isRateLimitNotice(payload.Information) {
			status = http.StatusTooManyRequests
		}
		return dailySeries{}, &alphaVantageError{status: status, message: payload.Information}
	}
	if payload.TimeSeries == nil {
		return dailySeries{}, &alphaVantageError{
			status:  http.StatusBadGateway,
			message: "Alpha Vantage response contained no daily time series",
		}
	}

	var closes []dailyClose
	for date, entry := range payload.TimeSeries {
		closePrice, err := strconv.ParseFloat(entry["4. close"], 64)
		// Unparseable or non-finite closes (NaN or ±Inf) are
		// dropped rather than propagated into the series.
		if err != nil || math.IsNaN(closePrice) || math.IsInf(closePrice, 0) {
			continue
		}
		closes = append(closes, dailyClose{Date: date, Close: closePrice})
	}
	slices.SortFunc(closes, func(a, b dailyClose) int {
		return strings.Compare(b.Date, a.Date)
	})
	if len(closes) == 0 {
		return dailySeries{}, &alphaVantageError{
			status:  http.StatusBadGateway,
			message: "Alpha Vantage daily time series had no parseable closing prices",
		}
	}

	lastRefreshed := ""
	if payload.MetaData != nil {
		lastRefreshed = payload.MetaData.LastRefreshed
	}
	return dailySeries{lastRefreshed: lastRefreshed, closes: closes}, nil
}
