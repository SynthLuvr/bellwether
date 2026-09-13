package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var demoOptions = appOptions{symbol: "MSFT", ndays: 5, apiKey: "demo-key"}

func appOkPayload() string {
	return `{
		"Meta Data": {
			"1. Information": "Daily Prices (open, high, low, close) and Volumes",
			"2. Symbol": "MSFT",
			"3. Last Refreshed": "2026-09-11",
			"4. Output Size": "Compact size",
			"5. Time Zone": "US/Eastern"
		},
		"Time Series (Daily)": {
			"2026-09-11": {"4. close": "370.1000"},
			"2026-09-10": {"4. close": "368.5000"},
			"2026-09-09": {"4. close": "369.9900"},
			"2026-09-08": {"4. close": "371.0700"},
			"2026-09-04": {"4. close": "366.1100"},
			"2026-09-03": {"4. close": "not-available"},
			"2026-09-02": {"4. close": "365.0200"}
		}
	}`
}

// capturedLog is one collected logger call.
type capturedLog struct {
	level  slog.Level
	msg    string
	fields []logField
}

// captureLogger keeps the default stdout logger out of test output.
type captureLogger struct {
	mu   sync.Mutex
	logs []capturedLog
}

func (c *captureLogger) Log(level slog.Level, msg string, fields ...logField) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, capturedLog{level: level, msg: msg, fields: fields})
}

func (c *captureLogger) entries() []capturedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedLog(nil), c.logs...)
}

// requestLogEntry finds the "request" line, failing the test when
// absent.
func requestLogEntry(t *testing.T, logs []capturedLog) capturedLog {
	t.Helper()
	for _, entry := range logs {
		if entry.msg == "request" {
			return entry
		}
	}
	t.Fatal("no request log line was captured")
	return capturedLog{}
}

func (l capturedLog) field(key string) (any, bool) {
	for _, f := range l.fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

func (l capturedLog) hasField(key string) bool {
	_, ok := l.field(key)
	return ok
}

// testApp serves the application for real on an OS-assigned port, with
// Alpha Vantage pointed at the recording upstream double.
type testApp struct {
	spy *upstreamSpy
	log *captureLogger
	url string
}

// bootApp serves the app on an ephemeral port and hands back the
// running pair plus a request function, so several real HTTP requests
// can hit the same app instance.
func bootApp(t *testing.T, options appOptions, upstream http.HandlerFunc, log logger) (*testApp, func(path string) *http.Response) {
	t.Helper()
	spy := &upstreamSpy{handler: upstream}
	upstreamServer := httptest.NewServer(spy)
	t.Cleanup(upstreamServer.Close)

	options.baseURL = mustURL(upstreamServer.URL)
	if log == nil {
		log = &captureLogger{}
	}
	app := httptest.NewServer(newApp(options, log))
	t.Cleanup(app.Close)

	capture, _ := log.(*captureLogger)
	pair := &testApp{spy: spy, log: capture, url: app.URL}
	client := &http.Client{}
	return pair, func(path string) *http.Response {
		request, err := http.NewRequest(http.MethodGet, app.URL+path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
}

func decodeJSON(t *testing.T, response *http.Response, target any) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
}

// expectJSONError pins the error-response contract: JSON body, never
// cached.
func expectJSONError(t *testing.T, response *http.Response, message string) {
	t.Helper()
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("content-type = %q, want application/json", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	decodeJSON(t, response, &body)
	if !strings.Contains(body.Error, message) {
		t.Errorf("error = %q, want it to contain %q", body.Error, message)
	}
}

func TestHealthReportsLiveness(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, jsonHandler(appOkPayload()), nil)

	response := get("/health")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
	}
	var body healthBody
	decodeJSON(t, response, &body)
	if body.Status != "ok" {
		t.Errorf("body = %+v, want status ok", body)
	}
	if hits := app.spy.count(); hits != 0 {
		t.Errorf("upstream hit %d times, want 0", hits)
	}
}

func TestRootReturnsLastNDaysClosesAndAverage(t *testing.T) {
	t.Parallel()
	_, get := bootApp(t, demoOptions, jsonHandler(appOkPayload()), nil)

	response := get("/")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("cache-control = %q, want public, max-age=3600", got)
	}

	var body struct {
		Symbol        string  `json:"symbol"`
		NDays         int     `json:"ndays"`
		LastRefreshed string  `json:"lastRefreshed"`
		AverageClose  float64 `json:"averageClose"`
		Closes        []struct {
			Date  string  `json:"date"`
			Close float64 `json:"close"`
		} `json:"closes"`
	}
	decodeJSON(t, response, &body)
	if body.Symbol != "MSFT" || body.NDays != 5 || body.LastRefreshed != "2026-09-11" {
		t.Errorf("summary header fields = %+v", body)
	}
	if body.AverageClose != 369.154 {
		t.Errorf("averageClose = %v, want 369.154", body.AverageClose)
	}
	want := []struct {
		date  string
		close float64
	}{
		{"2026-09-11", 370.1},
		{"2026-09-10", 368.5},
		{"2026-09-09", 369.99},
		{"2026-09-08", 371.07},
		{"2026-09-04", 366.11},
	}
	if len(body.Closes) != len(want) {
		t.Fatalf("closes = %+v, want %d entries", body.Closes, len(want))
	}
	for i := range want {
		if body.Closes[i].Date != want[i].date || body.Closes[i].Close != want[i].close {
			t.Errorf("closes[%d] = %+v, want %+v", i, body.Closes[i], want[i])
		}
	}
}

func TestRootRequestsCompactWithConfiguredCredentials(t *testing.T) {
	t.Parallel()
	options := demoOptions
	options.ndays = 100
	app, get := bootApp(t, options, jsonHandler(appOkPayload()), nil)

	if response := get("/"); response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	sent := app.spy.firstURL()
	if sent == nil {
		t.Fatal("no upstream request was recorded")
	}
	for key, want := range map[string]string{
		"function":   "TIME_SERIES_DAILY",
		"symbol":     "MSFT",
		"apikey":     "demo-key",
		"outputsize": "compact",
	} {
		if got := sent.Query().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestRootRequestsFullOutputBeyondCompactWindow(t *testing.T) {
	t.Parallel()
	options := demoOptions
	options.ndays = 101
	app, get := bootApp(t, options, jsonHandler(appOkPayload()), nil)

	response := get("/")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := app.spy.firstURL().Query().Get("outputsize"); got != "full" {
		t.Errorf("outputsize = %q, want full", got)
	}
	var body struct {
		Closes []any `json:"closes"`
	}
	decodeJSON(t, response, &body)
	if len(body.Closes) != 6 {
		t.Errorf("closes = %d entries, want 6", len(body.Closes))
	}
}

func TestRootServesRepeatRequestsFromCache(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, jsonHandler(appOkPayload()), nil)

	first, second := get("/"), get("/")

	if hits := app.spy.count(); hits != 1 {
		t.Errorf("upstream hit %d times, want 1", hits)
	}
	firstBody, _ := io.ReadAll(first.Body)
	secondBody, _ := io.ReadAll(second.Body)
	if string(firstBody) != string(secondBody) {
		t.Errorf("cached response differs:\nfirst: %s\nsecond: %s", firstBody, secondBody)
	}
}

func TestRootCollapsesConcurrentRequestsIntoOneUpstreamCall(t *testing.T) {
	t.Parallel()
	app, _ := bootApp(t, demoOptions, func(w http.ResponseWriter, _ *http.Request) {
		// Hold the upstream response open long enough that every
		// concurrent request arrives while the single flight is pending.
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, appOkPayload())
	}, nil)

	client := &http.Client{}
	var wg sync.WaitGroup
	statuses := make(chan int, 3)
	for range 3 {
		wg.Go(func() {
			response, err := client.Get(app.url)
			if err != nil {
				t.Errorf("GET /: %v", err)
				return
			}
			statuses <- response.StatusCode
			_ = response.Body.Close()
		})
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	}
	if hits := app.spy.count(); hits != 1 {
		t.Errorf("upstream hit %d times, want 1", hits)
	}
}

func TestRootDoesNotCacheUpstreamFailures(t *testing.T) {
	t.Parallel()
	// A near-zero cooldown keeps this test's contract — recovery on
	// the very next request — while production uses the 30s default.
	options := appOptions{symbol: "MSFT", ndays: 5, apiKey: "demo-key", failureCooldown: time.Nanosecond}
	var calls sync.Mutex
	call := 0
	app, get := bootApp(t, options, func(w http.ResponseWriter, _ *http.Request) {
		calls.Lock()
		call++
		first := call == 1
		calls.Unlock()
		if first {
			fmt.Fprint(w, `{"Note": "API call frequency is 25 per day."}`)
			return
		}
		fmt.Fprint(w, appOkPayload())
	}, nil)

	if response := get("/"); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first status = %d, want 429", response.StatusCode)
	}
	if response := get("/"); response.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", response.StatusCode)
	}
	if hits := app.spy.count(); hits != 2 {
		t.Errorf("upstream hit %d times, want 2", hits)
	}
}

// While the upstream keeps failing, the failure cooldown fast-fails
// every request inside the window: client traffic must not turn into
// one upstream attempt per request against a quota-exhausted or
// hard-down upstream.
func TestRootFastFailsRequestsWhileUpstreamFails(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, jsonHandler(`{"Note": "API call frequency is 25 per day."}`), nil)

	for i := range 3 {
		if response := get("/"); response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("request %d status = %d, want 429", i+1, response.StatusCode)
		}
	}
	// One upstream exchange arms the cooldown; the remaining requests
	// are answered from the recorded error without another attempt.
	if hits := app.spy.count(); hits != 1 {
		t.Errorf("upstream hit %d times for 3 requests, want 1", hits)
	}
}

// A short TTL exercised through real HTTP: the first request caches
// the series, the TTL lapses, and the (retrying) failed reload serves
// the previous series degraded instead of failing.
func TestRootServesStaleSeriesWithHeaderWhenUpstreamFailsAfterTTL(t *testing.T) {
	t.Parallel()
	options := appOptions{symbol: "MSFT", ndays: 5, apiKey: "demo-key", cacheTTL: 10 * time.Millisecond}
	var mu sync.Mutex
	calls := 0
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			fmt.Fprint(w, appOkPayload())
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	app, get := bootApp(t, options, upstream, nil)

	first := get("/")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}
	if got := first.Header.Get(dataStaleHeader); got != "" {
		t.Errorf("first %s = %q, want unset", dataStaleHeader, got)
	}
	firstBody, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatalf("read first body: %v", err)
	}

	time.Sleep(30 * time.Millisecond)
	second := get("/")
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (stale serve)", second.StatusCode)
	}
	if got := second.Header.Get(dataStaleHeader); got != "true" {
		t.Errorf("second %s = %q, want true", dataStaleHeader, got)
	}
	if got := second.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("second cache-control = %q, want no-store", got)
	}
	secondBody, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatalf("read second body: %v", err)
	}
	if string(firstBody) != string(secondBody) {
		t.Errorf("stale body differs:\nfirst: %s\nsecond: %s", firstBody, secondBody)
	}
	// One initial load plus the failed reload (500s retry once).
	if hits := app.spy.count(); hits != 3 {
		t.Errorf("upstream hit %d times, want 3", hits)
	}

	var warn *capturedLog
	for _, entry := range app.log.entries() {
		if entry.msg == "serving stale series after upstream failure" {
			warn = &entry
			break
		}
	}
	if warn == nil {
		t.Fatal("no stale-serve warning was logged")
	}
	if warn.level != slog.LevelWarn {
		t.Errorf("stale-serve level = %v, want warn", warn.level)
	}
	if message, ok := warn.field("error"); !ok || message == "" {
		t.Errorf("stale-serve error field = %v, want non-empty", message)
	}
}

func TestRootURLEncodesSpecialCharacters(t *testing.T) {
	t.Parallel()
	options := appOptions{symbol: "BRK.B", ndays: 5, apiKey: "a b&c"}
	app, get := bootApp(t, options, jsonHandler(appOkPayload()), nil)

	if response := get("/"); response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	sent := app.spy.firstURL()
	if got := sent.Query().Get("symbol"); got != "BRK.B" {
		t.Errorf("symbol = %q, want BRK.B", got)
	}
	if got := sent.Query().Get("apikey"); got != "a b&c" {
		t.Errorf("apikey = %q, want a b&c", got)
	}
}

func TestRootReturnsFewerClosesWhenSeriesShorterThanNDays(t *testing.T) {
	t.Parallel()
	_, get := bootApp(t, demoOptions, jsonHandler(`{
		"Meta Data": {"3. Last Refreshed": "2026-09-10"},
		"Time Series (Daily)": {
			"2026-09-10": {"4. close": "368.5000"},
			"2026-09-09": {"4. close": "369.9900"}
		}
	}`), nil)

	response := get("/")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var body struct {
		AverageClose float64 `json:"averageClose"`
		Closes       []any   `json:"closes"`
	}
	decodeJSON(t, response, &body)
	if len(body.Closes) != 2 {
		t.Errorf("closes = %d, want 2", len(body.Closes))
	}
	if body.AverageClose != 369.245 {
		t.Errorf("averageClose = %v, want 369.245", body.AverageClose)
	}
}

func TestRootDefaultsLastRefreshedWhenMetaDataMissing(t *testing.T) {
	t.Parallel()
	payloads := []string{
		`{"Time Series (Daily)": {"2026-09-10": {"4. close": "368.5000"}}}`,
		`{"Meta Data": {"1. Information": "Daily Prices"},
		  "Time Series (Daily)": {"2026-09-10": {"4. close": "368.5000"}}}`,
	}
	for _, payload := range payloads {
		_, get := bootApp(t, demoOptions, jsonHandler(payload), nil)
		response := get("/")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
		var body struct {
			LastRefreshed string `json:"lastRefreshed"`
		}
		decodeJSON(t, response, &body)
		if body.LastRefreshed != "" {
			t.Errorf("lastRefreshed = %q, want empty", body.LastRefreshed)
		}
	}
}

func TestRootMapsUpstreamFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		respond  http.HandlerFunc
		status   int
		message  string
		attempts int
	}{
		{
			name:    "maps an upstream Error Message to 502",
			respond: jsonHandler(`{"Error Message": "Invalid API call. Please retry or visit ..."}`),
			status:  http.StatusBadGateway,
			message: "Invalid API call",
		},
		{
			name:    "surfaces rate-limit notes as 429",
			respond: jsonHandler(`{"Note": "API call frequency is 25 per day."}`),
			status:  http.StatusTooManyRequests,
			message: "25 per day",
		},
		{
			name:    "surfaces quota-flavored Information notices as 429",
			respond: jsonHandler(`{"Information": "We have detected your API key as demo-key and our standard API rate limit is 25 requests per day."}`),
			status:  http.StatusTooManyRequests,
			message: "rate limit",
		},
		{
			name:    "surfaces call-frequency Information notices as 429",
			respond: jsonHandler(`{"Information": "Thank you for using ALPHA VANTAGE! Our standard API call frequency is 25 calls per day."}`),
			status:  http.StatusTooManyRequests,
			message: "25 calls per day",
		},
		{
			name:    "surfaces other informational notices as 503",
			respond: jsonHandler(`{"Information": "The **demo** API key is for demo purposes only. Please claim your free API key."}`),
			status:  http.StatusServiceUnavailable,
			message: "demo purposes",
		},
		{
			name:     "maps upstream HTTP failures to 502 after one retry",
			respond:  statusHandler(http.StatusInternalServerError, nil),
			status:   http.StatusBadGateway,
			message:  "HTTP 500",
			attempts: 2,
		},
		{
			name:    "maps payloads without a time series to 502",
			respond: jsonHandler(`{"Meta Data": {"2. Symbol": "MSFT"}}`),
			status:  http.StatusBadGateway,
			message: "no daily time series",
		},
		{
			name: "maps fully unparseable series to 502",
			respond: jsonHandler(`{
				"Meta Data": {"2. Symbol": "MSFT"},
				"Time Series (Daily)": {
					"2026-09-10": {"4. close": "not-available"},
					"2026-09-09": {"4. close": "-"}
				}
			}`),
			status:  http.StatusBadGateway,
			message: "no parseable closing prices",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app, get := bootApp(t, demoOptions, tt.respond, nil)
			response := get("/")
			if response.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, tt.status)
			}
			expectJSONError(t, response, tt.message)
			wantAttempts := tt.attempts
			if wantAttempts == 0 {
				wantAttempts = 1
			}
			if hits := app.spy.count(); hits != wantAttempts {
				t.Errorf("upstream hit %d times, want %d", hits, wantAttempts)
			}
		})
	}
}

func TestRootReturnsJSON500OnNetworkFailureAfterOneRetry(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, dropConnection, nil)

	response := get("/")
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	expectJSONError(t, response, "Internal Server Error")
	if hits := app.spy.count(); hits != 2 {
		t.Errorf("upstream hit %d times, want 2", hits)
	}

	entry := requestLogEntry(t, app.log.entries())
	if entry.level != slog.LevelError {
		t.Errorf("level = %v, want error", entry.level)
	}
	if method, _ := entry.field("method"); method != "GET" {
		t.Errorf("method = %v, want GET", method)
	}
	if path, _ := entry.field("path"); path != "/" {
		t.Errorf("path = %v, want /", path)
	}
	if status, _ := entry.field("status"); status != http.StatusInternalServerError {
		t.Errorf("status = %v, want 500", status)
	}
}

func TestRootReturnsJSON500OnMalformedUpstreamPayload(t *testing.T) {
	t.Parallel()
	_, get := bootApp(t, demoOptions, jsonHandler("definitely not json"), nil)

	response := get("/")
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	expectJSONError(t, response, "Internal Server Error")
}

func TestRootLogsUnhandledErrorsWithStackAndNoUpstreamURL(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, dropConnection, nil)
	get("/")

	entries := app.log.entries()
	found := false
	for _, entry := range entries {
		if entry.msg != "unhandled error" {
			continue
		}
		found = true
		if entry.level != slog.LevelError {
			t.Errorf("level = %v, want error", entry.level)
		}
		if message, ok := entry.field("error"); !ok || message == "" {
			t.Errorf("error field = %v, want a non-empty string", message)
		}
		if stack, ok := entry.field("stack"); !ok || stack == "" {
			t.Errorf("stack field = %v, want a non-empty string", stack)
		}
	}
	if !found {
		t.Error("no unhandled error log line was captured")
	}
}

func TestRequestLoggingFields(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, jsonHandler(appOkPayload()), nil)
	if response := get("/"); response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	entry := requestLogEntry(t, app.log.entries())
	if entry.level != slog.LevelInfo {
		t.Errorf("level = %v, want info", entry.level)
	}
	for key, want := range map[string]any{
		"method": "GET",
		"path":   "/",
		"status": http.StatusOK,
	} {
		if got, ok := entry.field(key); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", key, got, ok, want)
		}
	}
	duration, ok := entry.field("durationMs")
	if !ok || duration.(float64) < 0 {
		t.Errorf("durationMs = %v, want >= 0", duration)
	}
	upstream, ok := entry.field("upstreamMs")
	if !ok || upstream.(float64) < 0 {
		t.Errorf("upstreamMs = %v, want >= 0", upstream)
	}
	wantKeys := []string{"durationMs", "method", "path", "status", "upstreamMs"}
	if len(entry.fields) != len(wantKeys) {
		t.Errorf("fields = %v, want exactly %v", entry.fields, wantKeys)
	}
}

func TestRequestLoggingOmitsUpstreamMsForHealth(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, jsonHandler(appOkPayload()), nil)
	get("/health")

	entry := requestLogEntry(t, app.log.entries())
	if path, _ := entry.field("path"); path != "/health" {
		t.Errorf("path = %v, want /health", path)
	}
	if entry.hasField("upstreamMs") {
		t.Error("upstreamMs present, want omitted")
	}
}

func TestRequestLoggingRecordsUpstreamLatencyOnFailure(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, statusHandler(http.StatusInternalServerError, nil), nil)

	response := get("/")
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
	entry := requestLogEntry(t, app.log.entries())
	if entry.level != slog.LevelError {
		t.Errorf("level = %v, want error", entry.level)
	}
	if upstream, ok := entry.field("upstreamMs"); !ok || upstream.(float64) < 0 {
		t.Errorf("upstreamMs = %v, want >= 0", upstream)
	}
}

// slowWriter simulates a sluggish client so response-write time is
// visible in durationMs but must stay out of upstreamMs.
type slowWriter struct {
	http.ResponseWriter
	delay time.Duration
}

func (w *slowWriter) Write(b []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseWriter.Write(b)
}

func TestRequestLoggingUpstreamMsExcludesResponseWriteTime(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(jsonHandler(appOkPayload()))
	t.Cleanup(upstream.Close)

	options := demoOptions
	options.baseURL = mustURL(upstream.URL)
	log := &captureLogger{}
	handler := newApp(options, log)

	// Prime this instance's cache, then measure a cache hit through a
	// slow writer: upstreamMs must time the series load only, so a
	// hit stays near-zero no matter how long the response write takes.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	delay := 100 * time.Millisecond
	handler.ServeHTTP(&slowWriter{ResponseWriter: httptest.NewRecorder(), delay: delay},
		httptest.NewRequest(http.MethodGet, "/", nil))

	entry := requestLogEntry(t, log.entries()[1:])
	if upstream, ok := entry.field("upstreamMs"); !ok || upstream.(float64) >= float64(delay.Milliseconds()) {
		t.Errorf("upstreamMs = %v (present %v), want < %dms on a cache hit", upstream, ok, delay.Milliseconds())
	}
	if duration, _ := entry.field("durationMs"); duration.(float64) < float64(delay.Milliseconds()) {
		t.Errorf("durationMs = %v, want >= %dms including the slow write", duration, delay.Milliseconds())
	}
}

func TestRequestLoggingNeverLogsUpstreamURLOrAPIKey(t *testing.T) {
	t.Parallel()
	options := appOptions{symbol: "MSFT", ndays: 5, apiKey: "super-secret-key"}
	app, get := bootApp(t, options, jsonHandler(appOkPayload()), nil)
	if response := get("/"); response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	for _, entry := range app.log.entries() {
		serialized := fmt.Sprintf("%+v %v", entry, entry.fields)
		if strings.Contains(serialized, "super-secret-key") {
			t.Errorf("log line %q contains the API key", serialized)
		}
		if strings.Contains(serialized, app.spy.firstURL().String()) {
			t.Errorf("log line %q contains the upstream URL", serialized)
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRequestLoggingLogsPanickingHandlerBeforeRepanic(t *testing.T) {
	t.Parallel()
	log := &captureLogger{}
	handler := withRequestLogging(log, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom inside handler")
	}))

	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Error("panic was swallowed, want it to continue to net/http")
				return
			}
			if recovered != "boom inside handler" {
				t.Errorf("recovered = %v, want the original panic value", recovered)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	entry := requestLogEntry(t, log.entries())
	if entry.level != slog.LevelError {
		t.Errorf("level = %v, want error", entry.level)
	}
	if status, _ := entry.field("status"); status != http.StatusInternalServerError {
		t.Errorf("status = %v, want 500", status)
	}
	if panicked, ok := entry.field("panicked"); !ok || panicked != true {
		t.Errorf("panicked = %v (present %v), want true", panicked, ok)
	}
	if method, _ := entry.field("method"); method != http.MethodGet {
		t.Errorf("method = %v, want GET", method)
	}
}

func TestRequestLoggingKeepsWrittenStatusWhenPanicFollowsPartialWrite(t *testing.T) {
	t.Parallel()
	log := &captureLogger{}
	handler := withRequestLogging(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		panic("boom after write")
	}))

	recovered := func() (recovered any) {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		return nil
	}()
	if recovered != "boom after write" {
		t.Fatalf("recovered = %v, want the original panic value", recovered)
	}

	entry := requestLogEntry(t, log.entries())
	if status, _ := entry.field("status"); status != http.StatusServiceUnavailable {
		t.Errorf("status = %v, want the already-written 503", status)
	}
}

func TestUnknownPathReturnsJSON404LoggedAsWarning(t *testing.T) {
	t.Parallel()
	app, get := bootApp(t, demoOptions, jsonHandler(appOkPayload()), nil)

	response := get("/nope")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	expectJSONError(t, response, "Not Found")

	entry := requestLogEntry(t, app.log.entries())
	if entry.level != slog.LevelWarn {
		t.Errorf("level = %v, want warn", entry.level)
	}
	if path, _ := entry.field("path"); path != "/nope" {
		t.Errorf("path = %v, want /nope", path)
	}
	if method, _ := entry.field("method"); method != "GET" {
		t.Errorf("method = %v, want GET", method)
	}
	if status, _ := entry.field("status"); status != http.StatusNotFound {
		t.Errorf("status = %v, want 404", status)
	}
}

func TestWrongMethodOnKnownPathIsNotFound(t *testing.T) {
	t.Parallel()
	spy := &upstreamSpy{handler: jsonHandler(appOkPayload())}
	upstreamServer := httptest.NewServer(spy)
	t.Cleanup(upstreamServer.Close)

	options := demoOptions
	options.baseURL = mustURL(upstreamServer.URL)
	app := httptest.NewServer(newApp(options, &captureLogger{}))
	t.Cleanup(app.Close)

	request, err := http.NewRequest(http.MethodPost, app.URL+"/health", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("POST /health: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}
