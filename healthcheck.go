package main

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// healthcheck answers Docker's HEALTHCHECK probes: it GETs the
// service's own /health endpoint and exits 0 only on a 200. The
// distroless runtime image ships no wget, so the binary probes itself.
func healthcheck(getenv func(string) (string, bool)) int {
	port, ok := healthcheckPort(getenv("PORT"))
	if !ok {
		return 1
	}
	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("localhost", strconv.Itoa(port)),
		Path:   "/health",
	}
	client := &http.Client{Timeout: 3 * time.Second}
	request := &http.Request{
		Method: http.MethodGet,
		URL:    target,
		Header: make(http.Header),
	}
	response, err := client.Do(request)
	if err != nil {
		return 1
	}
	drainAndClose(response)
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// healthcheckPort resolves the listen port the same way the service
// does: unset or blank falls back to the default, anything else must be
// a port between 1 and 65535.
func healthcheckPort(raw string, ok bool) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if !ok || trimmed == "" {
		return defaultPort, true
	}
	port, err := strconv.Atoi(trimmed)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}
