package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		level string
		want  slog.Level
	}{
		{name: "debug", level: "debug", want: slog.LevelDebug},
		{name: "info", level: "info", want: slog.LevelInfo},
		{name: "warn", level: "warn", want: slog.LevelWarn},
		{name: "error", level: "error", want: slog.LevelError},
		{name: "default", level: "", want: slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := parseLogLevel(tt.level); got != tt.want {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestSetupLoggerSetsDefault(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	logger := setupLogger("development", "debug", &bytes.Buffer{})

	if slog.Default() != logger {
		t.Fatal("setupLogger did not set the default slog logger")
	}
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug logger does not enable debug records")
	}
}

func TestSetupLoggerFromEnvUsesUnprefixedVariables(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	t.Setenv("ENV", "development")
	t.Setenv("DETENT_ENV", "production")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("DETENT_LOG_LEVEL", "error")

	logger := setupLoggerFromEnv(&bytes.Buffer{}, &bytes.Buffer{})

	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("expected LOG_LEVEL to enable debug records")
	}
}

func TestSetupLoggerFromEnvFallsBackToDeprecatedPrefixedVariables(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	t.Setenv("ENV", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("DETENT_ENV", "development")
	t.Setenv("DETENT_LOG_LEVEL", "debug")

	logger := setupLoggerFromEnv(&bytes.Buffer{}, &bytes.Buffer{})

	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("expected DETENT_LOG_LEVEL fallback to enable debug records")
	}
}

func TestProductionLoggerWritesJSON(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	var out bytes.Buffer
	logger := setupLogger("production", "info", &out)
	logger.Info("ready", "component", "test")

	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("log output is not JSON: %v\n%s", err, out.String())
	}
	if record["msg"] != "ready" {
		t.Fatalf("msg = %v, want ready", record["msg"])
	}
	if record["component"] != "test" {
		t.Fatalf("component = %v, want test", record["component"])
	}
}

func TestProductionDebugLoggerIncludesSourceByDefault(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	var out bytes.Buffer
	logger := setupLogger("production", "debug", &out)
	logger.Debug("ready")

	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("log output is not JSON: %v\n%s", err, out.String())
	}
	source, ok := record["source"].(map[string]any)
	if !ok {
		t.Fatalf("source = %#v, want object", record["source"])
	}
	file, ok := source["file"].(string)
	if !ok || !strings.HasSuffix(file, "slog_test.go") {
		t.Fatalf("source.file = %#v, want shortened slog_test.go path", source["file"])
	}
}

func TestLogAddSourceEnablesInfoJSONSource(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	t.Setenv("ENV", "production")
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("LOG_ADD_SOURCE", "true")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	logger := setupLoggerFromEnv(&stdout, &stderr)
	logger.Info("ready")

	var record map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &record); err != nil {
		t.Fatalf("stderr log output is not JSON: %v\n%s", err, stderr.String())
	}
	if _, ok := record["source"].(map[string]any); !ok {
		t.Fatalf("source = %#v, want source object when LOG_ADD_SOURCE=true", record["source"])
	}
}

func TestLogAddSourceFalseDisablesDebugJSONSource(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	t.Setenv("ENV", "production")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_ADD_SOURCE", "false")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	logger := setupLoggerFromEnv(&stdout, &stderr)
	logger.Debug("ready")

	var record map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &record); err != nil {
		t.Fatalf("stderr log output is not JSON: %v\n%s", err, stderr.String())
	}
	if _, ok := record["source"]; ok {
		t.Fatalf("source = %#v, want omitted when LOG_ADD_SOURCE=false", record["source"])
	}
}

func TestCleanSourcePathUsesWorkspaceRelativePath(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	path := filepath.Join(wd, "slog.go")

	if got := cleanSourcePath(path); got != "slog.go" {
		t.Fatalf("cleanSourcePath() = %q, want slog.go", got)
	}
}

func TestUseTextLogs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		env       string
		envSet    bool
		stdoutTTY bool
		want      bool
	}{
		{name: "dev env uses text without tty", env: "dev", envSet: true, stdoutTTY: false, want: true},
		{name: "unset env uses text with tty", env: "", envSet: false, stdoutTTY: true, want: true},
		{name: "unset env uses json without tty", env: "", envSet: false, stdoutTTY: false, want: false},
		{name: "prod env uses json with tty", env: "prod", envSet: true, stdoutTTY: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := useTextLogs(tt.env, tt.envSet, tt.stdoutTTY); got != tt.want {
				t.Fatalf("useTextLogs(%q, %v, %v) = %v, want %v", tt.env, tt.envSet, tt.stdoutTTY, got, tt.want)
			}
		})
	}
}

func TestInteractiveDefaultLoggerWritesText(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(newLogHandlerForTerminal("", false, "debug", &out, true))
	logger.Debug("ready", "component", "test")

	if json.Valid(out.Bytes()) {
		t.Fatalf("log output is JSON, want text:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ready") {
		t.Fatalf("log output missing message:\n%s", out.String())
	}
}

func TestSetupLoggerWithOutputsRoutesTextToStdout(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	logger := setupLoggerWithOutputs("", false, "debug", &stdout, &stderr, true, false)
	logger.Debug("ready")

	if json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout log output is JSON, want text:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Fatalf("stdout log output missing message:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestSetupLoggerWithOutputsRoutesJSONToStderr(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	logger := setupLoggerWithOutputs("prod", true, "info", &stdout, &stderr, true, false)
	logger.Info("ready")

	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	var record map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &record); err != nil {
		t.Fatalf("stderr log output is not JSON: %v\n%s", err, stderr.String())
	}
	if record["msg"] != "ready" {
		t.Fatalf("msg = %v, want ready", record["msg"])
	}
}

func TestSetupLoggerWithOutputsRoutesTextToStderrForJSONCommandOutput(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	logger := setupLoggerWithOutputs("", false, "debug", &stdout, &stderr, true, true)
	logger.Debug("ready")

	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if json.Valid(stderr.Bytes()) {
		t.Fatalf("stderr log output is JSON, want text:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "ready") {
		t.Fatalf("stderr log output missing message:\n%s", stderr.String())
	}
}

func TestCommandOutputJSONSelected(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		args      []string
		stdoutTTY bool
		want      bool
	}{
		{name: "tty defaults to pretty", stdoutTTY: true},
		{name: "pipe defaults to json", want: true},
		{name: "env json overrides tty", env: "json", stdoutTTY: true, want: true},
		{name: "env pretty overrides pipe", env: "pretty"},
		{name: "flag json overrides env pretty", env: "pretty", args: []string{"version", "--format", "json"}, stdoutTTY: true, want: true},
		{name: "flag pretty overrides env json", env: "json", args: []string{"--format=pretty", "version"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DETENT_FORMAT", tt.env)

			if got := commandOutputJSONSelected(tt.args, tt.stdoutTTY); got != tt.want {
				t.Fatalf("commandOutputJSONSelected(%v, %v) = %v, want %v", tt.args, tt.stdoutTTY, got, tt.want)
			}
		})
	}
}

func TestNewLogHandlerAllowsNilWriter(t *testing.T) {
	logger := slog.New(newLogHandler("production", "info", nil))
	logger.Info("discarded")
}

func TestEnvValueFallback(t *testing.T) {
	t.Setenv("TEST_FALLBACK", "fallback")

	if got := envValue("TEST_PRIMARY", "TEST_FALLBACK"); got != "fallback" {
		t.Fatalf("envValue fallback = %q, want fallback", got)
	}
}
