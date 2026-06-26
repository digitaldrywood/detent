package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/lmittmann/tint"

	"github.com/digitaldrywood/detent/internal/cli"
)

func setupLoggerFromEnv(stdout io.Writer, stderr io.Writer) *slog.Logger {
	env, envSet := envValueWithPresence("ENV", "DETENT_ENV")
	stdoutTTY := cli.WriterIsTTY(stdout)
	addSource := logSourceSettingFromEnv()
	return setupLoggerWithOutputsAndSource(env, envSet, envValue("LOG_LEVEL", "DETENT_LOG_LEVEL"), stdout, stderr, stdoutTTY, commandOutputJSONSelected(os.Args[1:], stdoutTTY), addSource)
}

func setupLoggerFromRuntime(settings cli.RuntimeSettings, stdout io.Writer, stderr io.Writer, stdoutTTY bool) {
	setupLoggerWithOutputsAndSource(settings.Env.Value, strings.TrimSpace(settings.Env.Value) != "", settings.LogLevel.Value, stdout, stderr, stdoutTTY, commandOutputJSONSelected(os.Args[1:], stdoutTTY), logSourceSettingFromEnv())
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
	source := addSource.enabled(logLevel)
	if useTextLogs(env, envSet, stdoutTTY) {
		return tint.NewHandler(w, &tint.Options{
			Level:       logLevel,
			TimeFormat:  time.Kitchen,
			AddSource:   source,
			ReplaceAttr: textLogReplaceAttr,
		})
	}

	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       logLevel,
		AddSource:   source,
		ReplaceAttr: sourceLogReplaceAttr,
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

type logSourceSetting struct {
	value bool
	set   bool
}

func (s logSourceSetting) enabled(level slog.Level) bool {
	if s.set {
		return s.value
	}
	return level == slog.LevelDebug
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

func textLogReplaceAttr(groups []string, attr slog.Attr) slog.Attr {
	attr = sourceLogReplaceAttr(groups, attr)
	if err, ok := attr.Value.Any().(error); ok {
		errAttr := tint.Err(err)
		errAttr.Key = attr.Key
		return errAttr
	}
	return attr
}

func sourceLogReplaceAttr(_ []string, attr slog.Attr) slog.Attr {
	if attr.Key != slog.SourceKey {
		return attr
	}
	source, ok := attr.Value.Any().(*slog.Source)
	if !ok || source == nil {
		return attr
	}
	copied := *source
	copied.File = cleanSourcePath(copied.File)
	return slog.Any(slog.SourceKey, &copied)
}

func cleanSourcePath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return path
	}
	if rel, ok := relativeSourcePath(path, mustGetwdForLogSource()); ok {
		return rel
	}
	if rel, ok := moduleRelativeSourcePath(path); ok {
		return rel
	}
	return filepath.ToSlash(path)
}

func relativeSourcePath(path string, base string) (string, bool) {
	if strings.TrimSpace(base) == "" {
		return "", false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func moduleRelativeSourcePath(path string) (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok || strings.TrimSpace(info.Main.Path) == "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(info.Main.Path, "/"), "/")
	if len(parts) == 0 {
		return "", false
	}
	module := parts[len(parts)-1]
	normalized := filepath.ToSlash(path)
	marker := "/" + module + "/"
	index := strings.LastIndex(normalized, marker)
	if index == -1 {
		return "", false
	}
	return normalized[index+len(marker):], true
}

func mustGetwdForLogSource() string {
	wd, err := os.Getwd()
	if err != nil {
		return string(filepath.Separator)
	}
	return wd
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
