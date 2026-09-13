package main

import (
	"net/http"
	"strconv"
	"syscall"
	"testing"
)

func TestAlphaVantageErrorMessage(t *testing.T) {
	t.Parallel()
	err := &alphaVantageError{status: http.StatusTooManyRequests, message: "API call frequency is 25 per day."}
	if got := err.Error(); got != err.message {
		t.Errorf("Error() = %q, want %q", got, err.message)
	}
}

func TestParseRetryAfterRejectsOutOfRangeDigits(t *testing.T) {
	t.Parallel()
	if _, ok := parseRetryAfter("99999999999999999999"); ok {
		t.Error("parseRetryAfter(huge) ok = true, want false")
	}
	if _, ok := parseRetryAfter(strconv.Itoa(1<<31 - 1)); !ok {
		t.Error("parseRetryAfter(max int32 digits) ok = false, want true")
	}
}

func TestSignalName(t *testing.T) {
	t.Parallel()
	if got := signalName(syscall.SIGTERM); got != "SIGTERM" {
		t.Errorf("signalName(SIGTERM) = %q, want SIGTERM", got)
	}
	if got := signalName(syscall.SIGINT); got != "SIGINT" {
		t.Errorf("signalName(SIGINT) = %q, want SIGINT", got)
	}
	if got := signalName(syscall.SIGHUP); got == "" {
		t.Error("signalName(SIGHUP) is empty, want a description")
	}
}
