package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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

func TestSetupLoggerFromEnvUsesDetentVariables(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	t.Setenv("DETENT_ENV", "development")
	t.Setenv("ENV", "production")
	t.Setenv("DETENT_LOG_LEVEL", "debug")
	t.Setenv("LOG_LEVEL", "error")

	logger := setupLoggerFromEnv(&bytes.Buffer{}, &bytes.Buffer{})

	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("expected DETENT_LOG_LEVEL to enable debug records")
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
	logger := setupLoggerWithOutputs("", false, "debug", &stdout, &stderr, true)
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
	logger := setupLoggerWithOutputs("prod", true, "info", &stdout, &stderr, true)
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

func TestNewLogHandlerAllowsNilWriter(t *testing.T) {
	logger := slog.New(newLogHandler("production", "info", nil))
	logger.Info("discarded")
}

func TestEnvValueFallback(t *testing.T) {
	t.Setenv("DETENT_TEST_FALLBACK", "fallback")

	if got := envValue("DETENT_TEST_PRIMARY", "DETENT_TEST_FALLBACK"); got != "fallback" {
		t.Fatalf("envValue fallback = %q, want fallback", got)
	}
}
