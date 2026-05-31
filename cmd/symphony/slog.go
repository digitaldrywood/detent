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
	env, envSet := envValueWithPresence("SYMPHONY_ENV", "ENV")
	return setupLoggerForTerminal(env, envSet, envValue("SYMPHONY_LOG_LEVEL", "LOG_LEVEL"), w, stdoutIsTTY())
}

func setupLogger(env string, level string, w io.Writer) *slog.Logger {
	return setupLoggerForTerminal(env, strings.TrimSpace(env) != "", level, w, false)
}

func setupLoggerForTerminal(env string, envSet bool, level string, w io.Writer, stdoutTTY bool) *slog.Logger {
	logger := slog.New(newLogHandlerForTerminal(env, envSet, level, w, stdoutTTY))
	slog.SetDefault(logger)
	return logger
}

func newLogHandler(env string, level string, w io.Writer) slog.Handler {
	return newLogHandlerForTerminal(env, strings.TrimSpace(env) != "", level, w, false)
}

func newLogHandlerForTerminal(env string, envSet bool, level string, w io.Writer, stdoutTTY bool) slog.Handler {
	if w == nil {
		w = io.Discard
	}

	logLevel := parseLogLevel(level)
	if useTextLogs(env, envSet, stdoutTTY) {
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

func useTextLogs(env string, envSet bool, stdoutTTY bool) bool {
	if isDevelopment(env) {
		return true
	}
	if envSet {
		return false
	}
	return stdoutTTY
}

func stdoutIsTTY() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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
	value, _ := envValueWithPresence(primary, fallback)
	return value
}

func envValueWithPresence(primary string, fallback string) (string, bool) {
	if value, ok := os.LookupEnv(primary); ok && value != "" {
		return value, true
	}
	if value, ok := os.LookupEnv(fallback); ok && value != "" {
		return value, true
	}
	return "", false
}
