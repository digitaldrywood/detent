package cli

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestServeLoggerLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		env       map[string]string
		want      slog.Level
		wantError bool
	}{
		{name: "default", want: slog.LevelInfo},
		{name: "flag", args: []string{"--log-level", "debug"}, want: slog.LevelDebug},
		{name: "env", env: map[string]string{"LOG_LEVEL": "warn"}, want: slog.LevelWarn},
		{name: "prefixed env", env: map[string]string{"DETENT_LOG_LEVEL": "error"}, want: slog.LevelError},
		{name: "flag wins over env", args: []string{"--log-level", "debug"}, env: map[string]string{"LOG_LEVEL": "error"}, want: slog.LevelDebug},
		{name: "env case insensitive", env: map[string]string{"LOG_LEVEL": "DEBUG"}, want: slog.LevelDebug},
		{name: "invalid flag", args: []string{"--log-level", "verbose"}, wantError: true},
		{name: "invalid env", env: map[string]string{"LOG_LEVEL": "trace"}, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			var logger *slog.Logger
			var loggerErr error
			root := &cobra.Command{Use: "detent", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().String("log-level", "", "log level")
			root.AddCommand(&cobra.Command{Use: "serve", RunE: func(cmd *cobra.Command, _ []string) error {
				logger, loggerErr = serveLogger(cmd, func(key string) string { return tt.env[key] }, &output)
				return nil
			}})
			root.SetArgs(append([]string{"serve"}, tt.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if tt.wantError {
				if loggerErr == nil || !strings.Contains(loggerErr.Error(), "log level") {
					t.Fatalf("serveLogger() error = %v, want invalid level", loggerErr)
				}
				return
			}
			if loggerErr != nil {
				t.Fatal(loggerErr)
			}
			if !logger.Enabled(context.Background(), tt.want) || logger.Enabled(context.Background(), tt.want-1) {
				t.Fatalf("logger level does not match %v", tt.want)
			}
			logger.Log(context.Background(), tt.want, "serve logger probe")
			if !strings.HasPrefix(output.String(), "{") || !strings.Contains(output.String(), `"msg":"serve logger probe"`) {
				t.Fatalf("logger did not write JSON to the output: %q", output.String())
			}
		})
	}
}

func TestHubServeHonorsLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		env       string
		want      slog.Level
		wantError bool
	}{
		{name: "default info", want: slog.LevelInfo},
		{name: "env debug", env: "debug", want: slog.LevelDebug},
		{name: "flag overrides env", args: []string{"--log-level", "warn"}, env: "debug", want: slog.LevelWarn},
		{name: "invalid", args: []string{"--log-level", "loud"}, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got *slog.Logger
			run := func(_ context.Context, cfg hubserver.Config) error {
				got = cfg.Logger
				return nil
			}
			lookup := func(name string) string {
				switch name {
				case "DETENT_HUB_ADMIN_TOKEN":
					return "hub-admin-token"
				case "LOG_LEVEL":
					return tt.env
				}
				return ""
			}
			root := &cobra.Command{Use: "detent", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().String("log-level", "", "log level")
			root.AddCommand(newHubCommandWithRun("v-test", lookup, run))
			var stderr bytes.Buffer
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"hub", "serve", "--database", filepath.Join(t.TempDir(), "hub.db"), "--listen", "127.0.0.1:0"}, tt.args...))
			err := root.Execute()
			if tt.wantError {
				if err == nil || got != nil {
					t.Fatalf("Execute() error = %v, runner called %t", err, got != nil)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || !got.Enabled(context.Background(), tt.want) || got.Enabled(context.Background(), tt.want-1) {
				t.Fatalf("hub logger level does not match %v", tt.want)
			}
			got.Log(context.Background(), tt.want, "hub logger probe")
			if !strings.Contains(stderr.String(), `"msg":"hub logger probe"`) {
				t.Fatalf("hub logger did not write JSON to stderr: %q", stderr.String())
			}
		})
	}
}
