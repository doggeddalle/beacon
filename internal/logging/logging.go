// Package logging configures the process-wide structured logger.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup builds a slog.Logger from the given level ("debug"|"info"|"warn"|
// "error") and format ("text"|"json"), installs it as the default logger, and
// returns it along with a Ring that retains recent log lines for the admin UI.
func Setup(level, format string) (*slog.Logger, *Ring) {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	ring := NewRing(handler, 300)
	logger := slog.New(ring)
	slog.SetDefault(logger)
	return logger, ring
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
