package main

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func validTestEnv() map[string]string {
	return map[string]string{
		"SYMBOL": "MSFT",
		"NDAYS":  "5",
		"APIKEY": "demo-key",
	}
}

func envGetter(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestLoadConfigReadsAllVariables(t *testing.T) {
	t.Parallel()
	env := validTestEnv()
	env["PORT"] = "8080"

	cfg, err := loadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	want := config{symbol: "MSFT", ndays: 5, apiKey: "demo-key", port: 8080, cacheTTL: time.Hour}
	if cfg != want {
		t.Errorf("loadConfig() = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigTrimsValues(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(envGetter(map[string]string{
		"SYMBOL": " MSFT ",
		"NDAYS":  " 5 ",
		"APIKEY": " k ",
	}))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	want := config{symbol: "MSFT", ndays: 5, apiKey: "k", port: 3000, cacheTTL: time.Hour}
	if cfg != want {
		t.Errorf("loadConfig() = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigFallsBackToProcessEnv(t *testing.T) {
	// Mutates the real environment; cannot run in parallel.
	keys := []string{"SYMBOL", "NDAYS", "APIKEY", "PORT"}
	saved := map[string]string{}
	for _, key := range keys {
		saved[key] = os.Getenv(key)
		_ = os.Unsetenv(key)
	}
	t.Cleanup(func() {
		for key, value := range saved {
			if value == "" {
				_ = os.Unsetenv(key)
			} else {
				_ = os.Setenv(key, value)
			}
		}
	})

	_, err := loadConfig(nil)
	if _, ok := errors.AsType[*configError](err); !ok {
		t.Fatalf("loadConfig(nil) error = %v, want configError", err)
	}
}

func TestLoadConfigDefaultsPort(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(envGetter(validTestEnv()))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.port != 3000 {
		t.Errorf("port = %d, want 3000", cfg.port)
	}
}

func TestLoadConfigBlankPortMeansDefault(t *testing.T) {
	t.Parallel()
	for _, port := range []string{"  ", ""} {
		env := validTestEnv()
		env["PORT"] = port
		cfg, err := loadConfig(envGetter(env))
		if err != nil {
			t.Fatalf("loadConfig(PORT=%q) error = %v", port, err)
		}
		if cfg.port != 3000 {
			t.Errorf("loadConfig(PORT=%q).port = %d, want 3000", port, cfg.port)
		}
	}
}

func TestLoadConfigRejectsBadPort(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(`a port between 1 and 65535`)
	for _, port := range []string{"http", "0", "70000", "2.5", "-1", "8080.0"} {
		env := validTestEnv()
		env["PORT"] = port
		_, err := loadConfig(envGetter(env))
		if err == nil {
			t.Errorf("loadConfig(PORT=%q) succeeded, want error", port)
			continue
		}
		if !pattern.MatchString(err.Error()) {
			t.Errorf("loadConfig(PORT=%q) error = %q, want match %q", port, err, pattern)
		}
	}
}

func TestLoadConfigReadsCacheTTL(t *testing.T) {
	t.Parallel()
	env := validTestEnv()
	env["CACHE_TTL"] = " 2h "

	cfg, err := loadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.cacheTTL != 2*time.Hour {
		t.Errorf("cacheTTL = %v, want 2h", cfg.cacheTTL)
	}
}

func TestLoadConfigDefaultsCacheTTL(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(envGetter(validTestEnv()))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.cacheTTL != time.Hour {
		t.Errorf("cacheTTL = %v, want 1h", cfg.cacheTTL)
	}
}

func TestLoadConfigBlankCacheTTLMeansDefault(t *testing.T) {
	t.Parallel()
	for _, ttl := range []string{"  ", ""} {
		env := validTestEnv()
		env["CACHE_TTL"] = ttl
		cfg, err := loadConfig(envGetter(env))
		if err != nil {
			t.Fatalf("loadConfig(CACHE_TTL=%q) error = %v", ttl, err)
		}
		if cfg.cacheTTL != time.Hour {
			t.Errorf("loadConfig(CACHE_TTL=%q).cacheTTL = %v, want 1h", ttl, cfg.cacheTTL)
		}
	}
}

func TestLoadConfigRejectsBadCacheTTL(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(`a positive duration of at most 24h`)
	for _, ttl := range []string{"2 hours", "0s", "-1h", "25h", "1.5"} {
		env := validTestEnv()
		env["CACHE_TTL"] = ttl
		_, err := loadConfig(envGetter(env))
		if err == nil {
			t.Errorf("loadConfig(CACHE_TTL=%q) succeeded, want error", ttl)
			continue
		}
		if !pattern.MatchString(err.Error()) {
			t.Errorf("loadConfig(CACHE_TTL=%q) error = %q, want match %q", ttl, err, pattern)
		}
	}
}

func TestLoadConfigRejectsMissingSymbol(t *testing.T) {
	t.Parallel()
	env := validTestEnv()
	delete(env, "SYMBOL")

	_, err := loadConfig(envGetter(env))
	if _, ok := errors.AsType[*configError](err); !ok {
		t.Errorf("loadConfig() error = %v, want configError", err)
	}
}

func TestLoadConfigRejectsBlankSymbol(t *testing.T) {
	t.Parallel()
	env := validTestEnv()
	env["SYMBOL"] = "  "

	_, err := loadConfig(envGetter(env))
	if err == nil || !strings.Contains(err.Error(), "SYMBOL") {
		t.Errorf("error = %v, want SYMBOL problem", err)
	}
}

func TestLoadConfigRejectsMissingAPIKey(t *testing.T) {
	t.Parallel()
	env := validTestEnv()
	delete(env, "APIKEY")

	_, err := loadConfig(envGetter(env))
	if err == nil || !strings.Contains(err.Error(), "APIKEY") {
		t.Errorf("error = %v, want APIKEY problem", err)
	}
}

func TestLoadConfigRejectsMissingNDays(t *testing.T) {
	t.Parallel()
	env := validTestEnv()
	delete(env, "NDAYS")

	_, err := loadConfig(envGetter(env))
	if err == nil || !strings.Contains(err.Error(), "NDAYS") {
		t.Errorf("error = %v, want NDAYS problem", err)
	}
}

func TestLoadConfigRejectsBadNDays(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(`positive integer`)
	for _, ndays := range []string{"week", "2.5", "0", "-3", "+5", "NaN", "Infinity", "5.0", "1e2"} {
		env := validTestEnv()
		env["NDAYS"] = ndays
		_, err := loadConfig(envGetter(env))
		if err == nil {
			t.Errorf("loadConfig(NDAYS=%q) succeeded, want error", ndays)
			continue
		}
		if !pattern.MatchString(err.Error()) {
			t.Errorf("loadConfig(NDAYS=%q) error = %q, want match %q", ndays, err, pattern)
		}
	}
}

func TestLoadConfigReportsEveryInvalidVariable(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(`SYMBOL[\s\S]*NDAYS[\s\S]*APIKEY`)

	_, err := loadConfig(envGetter(map[string]string{
		"NDAYS":  "week",
		"SYMBOL": " ",
		// APIKEY missing entirely.
	}))
	if err == nil {
		t.Fatal("loadConfig() succeeded, want error")
	}
	if !pattern.MatchString(err.Error()) {
		t.Errorf("error = %q, want %s in order", err, pattern)
	}
}

func TestLoadConfigErrorType(t *testing.T) {
	t.Parallel()
	var configErr *configError
	_, err := loadConfig(envGetter(nil))
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %v, want *configError", err)
	}
	want := []string{
		`SYMBOL must be a non-empty string (was not set)`,
		`NDAYS must be a positive integer (was not set)`,
		`APIKEY must be a non-empty string (was not set)`,
	}
	if !reflect.DeepEqual(configErr.problems, want) {
		t.Errorf("problems = %#v, want %#v", configErr.problems, want)
	}
}
