package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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

func TestSetupLoggerFromEnvUsesSymphonyVariables(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	t.Setenv("SYMPHONY_ENV", "development")
	t.Setenv("ENV", "production")
	t.Setenv("SYMPHONY_LOG_LEVEL", "debug")
	t.Setenv("LOG_LEVEL", "error")

	logger := setupLoggerFromEnv(&bytes.Buffer{})

	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("expected SYMPHONY_LOG_LEVEL to enable debug records")
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

func TestNewLogHandlerAllowsNilWriter(t *testing.T) {
	logger := slog.New(newLogHandler("production", "info", nil))
	logger.Info("discarded")
}

func TestEnvValueFallback(t *testing.T) {
	t.Setenv("SYMPHONY_TEST_FALLBACK", "fallback")

	if got := envValue("SYMPHONY_TEST_PRIMARY", "SYMPHONY_TEST_FALLBACK"); got != "fallback" {
		t.Fatalf("envValue fallback = %q, want fallback", got)
	}
}
