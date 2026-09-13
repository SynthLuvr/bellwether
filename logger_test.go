package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestDefaultLoggerWritesOneJSONObjectPerLine(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	log := newDefaultLogger(&out)

	log.Log(slog.LevelInfo, "listening",
		logField{Key: "port", Value: 3000},
		logField{Key: "symbol", Value: "MSFT"},
	)

	lines := splitLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want 1: %q", len(lines), out.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	for key, want := range map[string]any{
		"level":  "INFO",
		"msg":    "listening",
		"port":   float64(3000),
		"symbol": "MSFT",
	} {
		if entry[key] != want {
			t.Errorf("%s = %v, want %v", key, entry[key], want)
		}
	}
	// slog's JSON handler marshals the record time as RFC 3339 with
	// nanosecond precision and the process's local offset.
	if _, err := time.Parse(time.RFC3339Nano, entry["time"].(string)); err != nil {
		t.Errorf("time %v is not RFC 3339: %v", entry["time"], err)
	}
}

func TestDefaultLoggerOmitsAbsentFields(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	newDefaultLogger(&out).Log(slog.LevelError, "unhandled error")

	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if len(entry) != 3 {
		t.Errorf("entry has %d keys (%v), want exactly time/level/msg", len(entry), entry)
	}
	for _, key := range []string{"time", "level", "msg"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("entry is missing %s", key)
		}
	}
}

func TestDefaultLoggerLevels(t *testing.T) {
	t.Parallel()
	for level, want := range map[slog.Level]string{
		slog.LevelInfo:  "INFO",
		slog.LevelWarn:  "WARN",
		slog.LevelError: "ERROR",
	} {
		var out bytes.Buffer
		newDefaultLogger(&out).Log(level, "msg")
		var entry struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		if entry.Level != want {
			t.Errorf("Log(%v) level = %q, want %q", level, entry.Level, want)
		}
	}
}

func TestDefaultLoggerNilValueBecomesNull(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	newDefaultLogger(&out).Log(slog.LevelWarn, "msg", logField{Key: "stack", Value: nil})

	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if value, ok := entry["stack"]; !ok || value != nil {
		t.Errorf("stack = %#v (present=%v), want JSON null", value, ok)
	}
}

func splitLines(s string) []string {
	var lines []string
	for line := range bytes.SplitSeq([]byte(strings.TrimSpace(s)), []byte("\n")) {
		if len(line) > 0 {
			lines = append(lines, string(line))
		}
	}
	return lines
}
