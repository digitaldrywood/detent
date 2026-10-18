package main

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/digitaldrywood/detent/internal/logging"

	"github.com/digitaldrywood/detent/internal/cli"
)

func setupLoggerFromEnv(stdout io.Writer, stderr io.Writer) *slog.Logger {
	env, envSet := envValueWithPresence("ENV", "DETENT_ENV")
	stdoutTTY := cli.WriterIsTTY(stdout)
	addSource := logSourceSettingFromEnv()
	return setupLoggerWithOutputsAndSource(env, envSet, envValue("LOG_LEVEL", "DETENT_LOG_LEVEL"), stdout, stderr, stdoutTTY, commandOutputJSONSelected(os.Args[1:], stdoutTTY), addSource)
}

func setupLoggerFromRuntime(settings cli.RuntimeSettings, stdout io.Writer, stderr io.Writer, stdoutTTY bool) *slog.LevelVar {
	return setupRuntimeLoggerWithOutputsAndSource(settings.Env.Value, strings.TrimSpace(settings.Env.Value) != "", settings.LogLevel.Value, stdout, stderr, stdoutTTY, commandOutputJSONSelected(os.Args[1:], stdoutTTY), logSourceSettingFromEnv())
}

func setupRuntimeLoggerWithOutputsAndSource(env string, envSet bool, level string, stdout io.Writer, stderr io.Writer, stdoutTTY bool, forceStderr bool, addSource logSourceSetting) *slog.LevelVar {
	w := stderr
	if !forceStderr && useTextLogs(env, envSet, stdoutTTY) {
		w = stdout
	}
	parsedLevel := parseLogLevel(level)
	levelVar := &slog.LevelVar{}
	levelVar.Set(parsedLevel)
	logger := slog.New(newLogHandlerForTerminalWithLevel(env, envSet, parsedLevel, levelVar, w, stdoutTTY, addSource))
	slog.SetDefault(logger)
	return levelVar
}

func setupLogger(env string, level string, w io.Writer) *slog.Logger {
	return setupLoggerForTerminal(env, strings.TrimSpace(env) != "", level, w, false)
}

func setupLoggerWithOutputs(env string, envSet bool, level string, stdout io.Writer, stderr io.Writer, stdoutTTY bool, forceStderr bool) *slog.Logger {
	return setupLoggerWithOutputsAndSource(env, envSet, level, stdout, stderr, stdoutTTY, forceStderr, logSourceSetting{})
}

func setupLoggerWithOutputsAndSource(env string, envSet bool, level string, stdout io.Writer, stderr io.Writer, stdoutTTY bool, forceStderr bool, addSource logSourceSetting) *slog.Logger {
	w := stderr
	if !forceStderr && useTextLogs(env, envSet, stdoutTTY) {
		w = stdout
	}
	logger := slog.New(newLogHandlerForTerminalWithSource(env, envSet, level, w, stdoutTTY, addSource))
	slog.SetDefault(logger)
	return logger
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
	return newLogHandlerForTerminalWithSource(env, envSet, level, w, stdoutTTY, logSourceSetting{})
}

func newLogHandlerForTerminalWithSource(env string, envSet bool, level string, w io.Writer, stdoutTTY bool, addSource logSourceSetting) slog.Handler {
	if w == nil {
		w = io.Discard
	}

	logLevel := parseLogLevel(level)
	return newLogHandlerForTerminalWithLevel(env, envSet, logLevel, logLevel, w, stdoutTTY, addSource)
}

func newLogHandlerForTerminalWithLevel(env string, envSet bool, parsedLevel slog.Level, level slog.Leveler, w io.Writer, stdoutTTY bool, addSource logSourceSetting) slog.Handler {
	if w == nil {
		w = io.Discard
	}

	return logging.NewHandler(w, level, useTextLogs(env, envSet, stdoutTTY), logging.SourceSetting{Value: addSource.value, Set: addSource.set})
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
	for _, arg := range args {
		if arg == "mcp" {
			return true
		}
	}
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

type logSourceSetting struct {
	value bool
	set   bool
}

func logSourceSettingFromEnv() logSourceSetting {
	value, ok := envValueWithPresence("LOG_ADD_SOURCE", "DETENT_LOG_ADD_SOURCE")
	if !ok {
		return logSourceSetting{}
	}
	return logSourceSetting{value: parseLogBool(value), set: true}
}

func parseLogBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func cleanSourcePath(path string) string { return logging.CleanSourcePath(path) }

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
