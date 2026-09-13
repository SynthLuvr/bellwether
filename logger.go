package main

import (
	"context"
	"io"
	"log/slog"
)

// logField is one structured key/value pair. Values are the JSON-safe
// scalars the app logs: strings, numbers, booleans, and nil.
type logField struct {
	Key   string
	Value any
}

// logger writes one structured entry per call. The default
// implementation emits JSON lines; tests capture entries in memory.
type logger interface {
	Log(level slog.Level, msg string, fields ...logField)
}

// newDefaultLogger writes one JSON object per line in log/slog's
// standard JSON format: {"time", "level", "msg", ...fields}. Log
// routing and filtering are left to the process manager.
//
// SECURITY: never log full upstream URLs — the Alpha Vantage API key
// travels in the outbound URL's query string.
func newDefaultLogger(out io.Writer) logger {
	return &jsonLogger{log: slog.New(slog.NewJSONHandler(out, nil))}
}

// jsonLogger adapts the field-based logger seam onto slog.
type jsonLogger struct {
	log *slog.Logger
}

func (l *jsonLogger) Log(level slog.Level, msg string, fields ...logField) {
	args := make([]any, 0, len(fields)*2)
	for _, field := range fields {
		args = append(args, field.Key, field.Value)
	}
	l.log.Log(context.Background(), level, msg, args...)
}
