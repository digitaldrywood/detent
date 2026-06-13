package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lmittmann/tint"

	"github.com/digitaldrywood/detent/internal/cli"
)

func setupLoggerFromEnv(stdout io.Writer, stderr io.Writer) *slog.Logger {
	env, envSet := envValueWithPresence("ENV", "DETENT_ENV")
	stdoutTTY := cli.WriterIsTTY(stdout)
	return setupLoggerWithOutputs(env, envSet, envValue("LOG_LEVEL", "DETENT_LOG_LEVEL"), stdout, stderr, stdoutTTY, commandOutputJSONSelected(os.Args[1:], stdoutTTY))
}

func setupLoggerFromRuntime(settings cli.RuntimeSettings, stdout io.Writer, stderr io.Writer, stdoutTTY bool) {
	setupLoggerWithOutputs(settings.Env.Value, strings.TrimSpace(settings.Env.Value) != "", settings.LogLevel.Value, stdout, stderr, stdoutTTY, commandOutputJSONSelected(os.Args[1:], stdoutTTY))
}

func setupLogger(env string, level string, w io.Writer) *slog.Logger {
	return setupLoggerForTerminal(env, strings.TrimSpace(env) != "", level, w, false)
}

func setupLoggerWithOutputs(env string, envSet bool, level string, stdout io.Writer, stderr io.Writer, stdoutTTY bool, forceStderr bool) *slog.Logger {
	w := stderr
	if !forceStderr && useTextLogs(env, envSet, stdoutTTY) {
		w = stdout
	}
	return setupLoggerForTerminal(env, envSet, level, w, stdoutTTY)
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

func commandOutputJSONSelected(args []string, stdoutTTY bool) bool {
	formatFlag, flagSet := outputFormatArg(args)
	format, err := cli.ResolveOutputFormat(formatFlag, flagSet, os.Getenv("DETENT_FORMAT"), stdoutTTY)
	return err == nil && format == cli.OutputFormatJSON
}

func outputFormatArg(args []string) (string, bool) {
	for index, arg := range args {
		if arg == "--format" && index+1 < len(args) {
			return args[index+1], true
		}
		if value, ok := strings.CutPrefix(arg, "--format="); ok {
			return value, true
		}
	}
	return "", false
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
