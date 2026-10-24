// Package logger builds the application's structured slog logger.
package logger

import (
	"io"
	"log/slog"
)

// New returns a JSON logger (production) or a human-readable text logger
// (local development) writing to w.
func New(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
