package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"time"
)

// appOptions parameterizes one service instance.
type appOptions struct {
	symbol string
	ndays  int
	apiKey string
	// cacheTTL overrides the series cache lifetime; zero uses
	// seriesTTL. Tests shorten it to exercise expiry without
	// real-time waits.
	cacheTTL time.Duration
	// failureCooldown overrides how long a failed series load
	// fast-fails before the upstream is retried; zero uses
	// defaultFailureCooldown. Tests shrink it to exercise retry
	// semantics without real-time waits.
	failureCooldown time.Duration
	// baseURL overrides the Alpha Vantage endpoint; tests point it at
	// an httptest.Server.
	baseURL *url.URL
}

// stockSummary is the GET / response body.
type stockSummary struct {
	Symbol        string       `json:"symbol"`
	NDays         int          `json:"ndays"`
	LastRefreshed string       `json:"lastRefreshed"`
	AverageClose  float64      `json:"averageClose"`
	Closes        []dailyClose `json:"closes"`
}

// An hour of server- and client-side caching keeps the worst case
// (24 upstream refreshes/day) inside Alpha Vantage's 25/day free-tier
// quota; end-of-day closes change at most once per trading day. The
// TTL is tunable through CACHE_TTL (unset means seriesTTL) so a fleet
// of replicas can hold the same budget (see the README's quota math).
const (
	seriesTTL = time.Hour
	// maxDataAge bounds how long a cached series may be served, fresh
	// or stale: a day matches the useful life of an end-of-day close
	// (the last completed close stays correct until the next one), and
	// keeps a long upstream outage from silently serving ever-older
	// data — past the bound the mapped upstream error surfaces again.
	maxDataAge = 24 * time.Hour
	// defaultFailureCooldown is how long a failed series load
	// fast-fails before the upstream is retried: a quota-exhausted or
	// hard-down upstream costs one exchange per window, not one per
	// request, and recovery is detected at most one window late.
	// Thirty seconds keeps that window short enough that a blip
	// shorter than it costs at most one delayed retry.
	defaultFailureCooldown = 30 * time.Second
)

const (
	// dataStaleHeader marks a response served from cache after its TTL
	// because the upstream reload failed, so clients and probes can
	// tell degraded data from fresh data.
	dataStaleHeader = "X-Data-Stale"
	// neverCache keeps probes and failures from being served by any
	// cache.
	neverCache = "no-store"
)

// cacheFor is the Cache-Control value for a fresh series response:
// shared caches may serve it for exactly one server-side TTL.
func cacheFor(ttl time.Duration) string {
	return "public, max-age=" + strconv.Itoa(int(ttl/time.Second))
}

// newApp builds the service handler: GET / and GET /health plus a
// logging middleware, with JSON error responses throughout.
func newApp(options appOptions, log logger) http.Handler {
	options.cacheTTL = cmp.Or(options.cacheTTL, seriesTTL)
	app := &appServer{
		loadSummary: newSummaryLoader(options),
		log:         log,
		cacheTTL:    options.cacheTTL,
	}

	mux := http.NewServeMux()
	// "GET /{$}" also matches HEAD; unknown paths and wrong methods
	// alike fall to the catch-all JSON 404.
	mux.HandleFunc("GET /{$}", app.serveRoot)
	mux.HandleFunc("GET /health", app.serveHealth)
	mux.HandleFunc("/", serveNotFound)

	return withRequestLogging(log, mux)
}

type appServer struct {
	loadSummary func() cachedResult[stockSummary]
	log         logger
	cacheTTL    time.Duration
}

func (a *appServer) serveRoot(w http.ResponseWriter, r *http.Request) {
	// Times only the series load — upstreamMs must stay near-zero on a
	// cache hit, which it would not if response writing were included.
	// Recorded even when the load fails: a failure is either a real
	// upstream exchange or a near-zero cooldown fast-fail.
	loadStart := time.Now()
	result := a.loadSummary()
	spanFrom(r).record(elapsedMs(loadStart))
	if result.err != nil {
		if result.stale {
			// The previous series is still within maxDataAge: serve it
			// degraded rather than failing, with the marker header and
			// an uncachable response. The next request retries the
			// reload once the failure cooldown elapses.
			a.log.Log(slog.LevelWarn, "serving stale series after upstream failure",
				logField{Key: "error", Value: result.err.Error()},
			)
			w.Header().Set(dataStaleHeader, "true")
			writeJSON(w, http.StatusOK, neverCache, result.value)
			return
		}
		if upstreamErr, ok := errors.AsType[*alphaVantageError](result.err); ok {
			writeJSON(w, upstreamErr.status, neverCache, errorBody{Error: upstreamErr.message})
			return
		}
		a.log.Log(slog.LevelError, "unhandled error",
			logField{Key: "error", Value: result.err.Error()},
			logField{Key: "stack", Value: stackTrace()},
		)
		writeJSON(w, http.StatusInternalServerError, neverCache, errorBody{Error: "Internal Server Error"})
		return
	}
	writeJSON(w, http.StatusOK, cacheFor(a.cacheTTL), result.value)
}

func (a *appServer) serveHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, neverCache, healthBody{Status: "ok"})
}

func serveNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, neverCache, errorBody{Error: "Not Found"})
}

// newSummaryLoader returns the cached GET / loader: one upstream call
// per TTL window (or a stale serve after a failed reload), sliced to
// the newest ndays closes with their average.
func newSummaryLoader(options appOptions) func() cachedResult[stockSummary] {
	// Alpha Vantage's compact output only covers the latest 100 points.
	size := outputSizeCompact
	if options.ndays > 100 {
		size = outputSizeFull
	}
	// Built per app instance, so every process (and every test) caches
	// its own series.
	loadSeries := newCachedLoader(
		func() (dailySeries, error) {
			return fetchDailyCloses(fetchDailyClosesOptions{
				symbol:     options.symbol,
				apiKey:     options.apiKey,
				outputSize: size,
				baseURL:    options.baseURL,
			})
		},
		cachedLoaderOptions{
			ttl:      options.cacheTTL,
			maxAge:   maxDataAge,
			cooldown: cmp.Or(options.failureCooldown, defaultFailureCooldown),
		},
	)
	return func() cachedResult[stockSummary] {
		loaded := loadSeries()
		if loaded.err != nil && !loaded.stale {
			return cachedResult[stockSummary]{err: loaded.err}
		}
		closes := loaded.value.closes
		if len(closes) > options.ndays {
			closes = closes[:options.ndays]
		}
		return cachedResult[stockSummary]{
			value: stockSummary{
				Symbol:        options.symbol,
				NDays:         options.ndays,
				LastRefreshed: loaded.value.lastRefreshed,
				AverageClose:  averageClose(closes),
				Closes:        closes,
			},
			err:   loaded.err,
			stale: loaded.stale,
		}
	}
}

// averageClose is the mean closing price rounded to four decimals.
func averageClose(closes []dailyClose) float64 {
	sum := 0.0
	for _, entry := range closes {
		sum += entry.Close
	}
	return math.Round(sum/float64(len(closes))*1e4) / 1e4
}

// elapsedMs rounds elapsed milliseconds to one decimal place to keep
// sub-millisecond requests readable.
func elapsedMs(start time.Time) float64 {
	return math.Round(float64(time.Since(start))/float64(time.Millisecond)*10) / 10
}

// stackTrace renders the calling goroutine's stack.
func stackTrace() string {
	buffer := make([]byte, 64<<10)
	written := runtime.Stack(buffer, false)
	return string(buffer[:written])
}

// levelForStatus logs 5xx as errors, 4xx as warnings, the rest as info.
func levelForStatus(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

type errorBody struct {
	Error string `json:"error"`
}

type healthBody struct {
	Status string `json:"status"`
}

// upstreamSpan carries the measured series-load latency from the root
// handler to the request-logging middleware via the request context.
type upstreamSpan struct {
	recorded bool
	ms       float64
}

func (s *upstreamSpan) record(ms float64) {
	s.recorded = true
	s.ms = ms
}

type upstreamContextKey struct{}

func spanFrom(r *http.Request) *upstreamSpan {
	return r.Context().Value(upstreamContextKey{}).(*upstreamSpan)
}

// withRequestLogging logs one entry per request with the method, path,
// status, and total duration — plus upstreamMs when the request
// contacted Alpha Vantage. The entry is emitted from a defer, so a
// panicking handler still gets its line (status 500 and panicked true
// when nothing was written) before the panic continues on to net/http,
// which recovers it and closes the connection. It never logs a URL:
// the upstream URL's query string carries the API key.
func withRequestLogging(log logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		span := &upstreamSpan{}
		r = r.WithContext(context.WithValue(r.Context(), upstreamContextKey{}, span))
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Set to true only on normal return: a panic unwinding through
		// the handler leaves it false.
		completed := false
		defer func() {
			panicked := !completed
			status := recorder.status
			if panicked && !recorder.written {
				// Nothing was written before the panic; net/http will
				// abort the connection, so the recorder's 200 default
				// would misreport the outcome.
				status = http.StatusInternalServerError
			}
			fields := []logField{
				{Key: "method", Value: r.Method},
				{Key: "path", Value: r.URL.Path},
				{Key: "status", Value: status},
				{Key: "durationMs", Value: elapsedMs(start)},
			}
			if panicked {
				fields = append(fields, logField{Key: "panicked", Value: true})
			}
			if span.recorded {
				fields = append(fields, logField{Key: "upstreamMs", Value: span.ms})
			}
			log.Log(levelForStatus(status), "request", fields...)
		}()
		next.ServeHTTP(recorder, r)
		completed = true
	})
}

// statusRecorder captures the status the handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.written {
		r.status = status
		r.written = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	//nolint:wrapcheck // deliberate pass-through of the wrapped writer
	return r.ResponseWriter.Write(b)
}

// writeJSON marshals body with a JSON content type and cache policy.
// Marshaling these structs cannot fail.
func writeJSON(w http.ResponseWriter, status int, cacheControl string, body any) {
	payload, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
