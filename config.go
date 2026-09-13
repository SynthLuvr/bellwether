package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultPort = 3000

// config is the validated service configuration from the environment.
type config struct {
	symbol   string
	ndays    int
	apiKey   string
	port     int
	cacheTTL time.Duration
}

// configError reports every invalid environment variable at once, one
// problem per variable, in declaration order.
type configError struct {
	problems []string
}

func (e *configError) Error() string {
	return strings.Join(e.problems, "; ")
}

// loadConfig reads and validates SYMBOL, NDAYS, APIKEY, PORT, and
// CACHE_TTL from getenv. A nil getenv falls back to the real
// environment. Values are trimmed; missing or blank SYMBOL and APIKEY,
// an NDAYS that is not a plain integer >= 1, a PORT outside 1–65535,
// and a CACHE_TTL that is not a positive duration of at most
// maxDataAge are rejected, and every problem is reported in a single
// configError.
func loadConfig(getenv func(string) (string, bool)) (config, error) {
	if getenv == nil {
		getenv = os.LookupEnv
	}

	symbol, symbolProblem := requiredString("SYMBOL", getenv)
	ndays, ndaysProblem := positiveInteger("NDAYS", getenv)
	apiKey, apiKeyProblem := requiredString("APIKEY", getenv)
	port, portProblem := listenPort("PORT", getenv)
	cacheTTL, cacheTTLProblem := cacheDuration("CACHE_TTL", getenv)

	var problems []string
	for _, problem := range []string{symbolProblem, ndaysProblem, apiKeyProblem, portProblem, cacheTTLProblem} {
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		return config{}, &configError{problems: problems}
	}
	return config{symbol: symbol, ndays: ndays, apiKey: apiKey, port: port, cacheTTL: cacheTTL}, nil
}

// requiredString reads a variable that must be present and non-blank
// after trimming.
func requiredString(name string, getenv func(string) (string, bool)) (string, string) {
	raw, ok := getenv(name)
	trimmed := strings.TrimSpace(raw)
	if !ok || trimmed == "" {
		return "", name + " must be a non-empty string (was " + wasValue(trimmed, ok) + ")"
	}
	return trimmed, ""
}

// positiveInteger reads a required variable that must be a plain
// base-10 integer >= 1: digits only — no sign (strconv.Atoi alone
// would accept "+5"), decimal point, or exponent.
func positiveInteger(name string, getenv func(string) (string, bool)) (int, string) {
	raw, ok := getenv(name)
	trimmed := strings.TrimSpace(raw)
	value, err := strconv.Atoi(trimmed)
	if !ok || err != nil || !isDigits(trimmed) || value < 1 {
		return 0, name + " must be a positive integer (was " + wasValue(trimmed, ok) + ")"
	}
	return value, ""
}

// listenPort reads the optional PORT variable: missing or blank means
// the default port; anything else must be an integer between 1 and
// 65535.
func listenPort(name string, getenv func(string) (string, bool)) (int, string) {
	raw, ok := getenv(name)
	trimmed := strings.TrimSpace(raw)
	if !ok || trimmed == "" {
		return defaultPort, ""
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value < 1 || value > 65535 {
		return 0, name + ` must be a port between 1 and 65535 (was "` + trimmed + `")`
	}
	return value, ""
}

// cacheDuration reads the optional CACHE_TTL variable: missing or
// blank means the default one-hour TTL; anything else must parse as a
// positive duration no longer than maxDataAge, so an entry is always
// fresh for at least its TTL before it may be served stale.
func cacheDuration(name string, getenv func(string) (string, bool)) (time.Duration, string) {
	raw, ok := getenv(name)
	trimmed := strings.TrimSpace(raw)
	if !ok || trimmed == "" {
		return seriesTTL, ""
	}
	value, err := time.ParseDuration(trimmed)
	if err != nil || value <= 0 || value > maxDataAge {
		return 0, name + ` must be a positive duration of at most 24h (was "` + trimmed + `")`
	}
	return value, ""
}

// wasValue describes what a validator saw, for a problem message:
// "not set" when the variable was missing, otherwise the trimmed value
// in quotes.
func wasValue(trimmed string, ok bool) string {
	if !ok {
		return "not set"
	}
	return `"` + trimmed + `"`
}
