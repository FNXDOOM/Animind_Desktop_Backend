// Package logging configures the process-wide structured logger: structured JSON in production
// and human-readable text in development.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Init builds the process logger from LOG_LEVEL and LOG_FORMAT and installs it as the slog
// default, so third-party code using slog.Default() inherits the same format.
func Init(env, level, format string) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{
		Level: parseLevel(level),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Timestamps in UTC, so cron and token-expiry logs are unambiguous.
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	}

	var handler slog.Handler
	if strings.EqualFold(format, "text") || (format == "" && env != "production") {
		handler = slog.NewTextHandler(os.Stdout, handlerOpts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, handlerOpts)
	}

	logger := slog.New(handler).With(slog.String("app", "animind-backend"))
	slog.SetDefault(logger)
	return logger
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
