package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
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
				logger, _, loggerErr = serveLogger(cmd, func(key string) string { return tt.env[key] }, &output)
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
			logger.Error("serve logger probe", "err", fmt.Errorf("serve: %w", &json.UnmarshalTypeError{Value: "array", Type: reflect.TypeFor[string]()}))
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["source"] == nil || record["error_class"] != "*json.UnmarshalTypeError" || record["error"] != "serve: json: cannot unmarshal array into Go value of type string" {
				t.Fatal(output.String())
			}

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

func TestTenantEnvironmentCarriesResolvedLogLevel(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, level, want string
	}{
		{name: "resolved from a flag override", level: "info", want: "LOG_LEVEL=info"},
		{name: "resolved debug", level: "debug", want: "LOG_LEVEL=debug"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			environment := map[string]string{"LOG_LEVEL": "trace", "WORKOS_API_KEY": "sk_test"}
			lookup := func(name string) string {
				if name == "LOG_LEVEL" {
					return test.level
				}
				return environment[name]
			}
			config := cloudFileConfig{Allocation: &cloudAllocationFileConfig{}}
			config.WorkOS.APIKeyEnv = "WORKOS_API_KEY"
			got := tenantEnvironment(config, lookup)
			found := false
			for _, entry := range got {
				if strings.HasPrefix(entry, "LOG_LEVEL=") {
					if entry != test.want {
						t.Fatalf("tenant environment has %q, want %q", entry, test.want)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("tenant environment %v has no LOG_LEVEL", got)
			}
		})
	}
}
