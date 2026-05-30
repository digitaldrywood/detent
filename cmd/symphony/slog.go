package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lmittmann/tint"
)

func setupLoggerFromEnv(w io.Writer) *slog.Logger {
	return setupLogger(envValue("SYMPHONY_ENV", "ENV"), envValue("SYMPHONY_LOG_LEVEL", "LOG_LEVEL"), w)
}

func setupLogger(env string, level string, w io.Writer) *slog.Logger {
	logger := slog.New(newLogHandler(env, level, w))
	slog.SetDefault(logger)
	return logger
}

func newLogHandler(env string, level string, w io.Writer) slog.Handler {
	if w == nil {
		w = io.Discard
	}

	logLevel := parseLogLevel(level)
	if isDevelopment(env) {
		return tint.NewHandler(w, &tint.Options{
			Level:      logLevel,
			TimeFormat: time.Kitchen,
			AddSource:  logLevel == slog.LevelDebug,
		})
	}

	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: logLevel,
	})
}

func parseLogLevel(level string) slog.Level {
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

func isDevelopment(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "dev", "development", "local":
		return true
	default:
		return false
	}
}

func envValue(primary string, fallback string) string {
	if value := os.Getenv(primary); value != "" {
		return value
	}
	return os.Getenv(fallback)
}
