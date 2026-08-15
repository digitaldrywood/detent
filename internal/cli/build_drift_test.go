package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
)

func TestNewInstalledExecutableBuildReaderCapturesPath(t *testing.T) {
	t.Parallel()

	executableCalls := 0
	var paths []string
	read := newInstalledExecutableBuildReader(func() (string, error) {
		executableCalls++
		return "/opt/detent/bin/detent (deleted)", nil
	}, func(_ context.Context, path string, _ ...string) (string, error) {
		paths = append(paths, path)
		return `{"version":"v1.2.3","commit":"abcdef123456","build_date":"2026-08-14T00:00:00Z"}`, nil
	})

	for range 2 {
		if _, _, err := read(context.Background()); err != nil {
			t.Fatalf("read() error = %v", err)
		}
	}
	if executableCalls != 1 {
		t.Fatalf("executable calls = %d, want 1", executableCalls)
	}
	for _, path := range paths {
		if path != "/opt/detent/bin/detent" {
			t.Fatalf("binary path = %q, want captured pathname without deleted suffix", path)
		}
	}
}

func TestRunRuntimeBuildDriftMonitor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		installed  buildinfo.Info
		readErr    error
		wantLog    string
		wantAbsent string
	}{
		{
			name:       "matching build stays quiet",
			installed:  buildinfo.Info{Version: "v1.2.3", Commit: "abcdef123456"},
			wantAbsent: "restart required",
		},
		{
			name:      "stale running build warns with remediation",
			installed: buildinfo.Info{Version: "v1.2.4", Commit: "123456abcdef"},
			wantLog:   "detent start --restart",
		},
		{
			name:    "read failure is visible",
			readErr: errors.New("permission denied"),
			wantLog: "check installed Detent build drift failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&output, nil))
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			read := func(context.Context) (buildinfo.Info, string, error) {
				calls++
				if calls > 1 {
					cancel()
				}
				return tt.installed, "/opt/detent/bin/detent", tt.readErr
			}
			runRuntimeBuildDriftMonitor(ctx, buildinfo.Info{
				Version: "v1.2.3",
				Commit:  "abcdef123456",
			}, time.Nanosecond, read, logger)

			logs := output.String()
			if tt.wantLog != "" && !strings.Contains(logs, tt.wantLog) {
				t.Fatalf("logs = %q, want containing %q", logs, tt.wantLog)
			}
			if tt.wantAbsent != "" && strings.Contains(logs, tt.wantAbsent) {
				t.Fatalf("logs = %q, want no %q", logs, tt.wantAbsent)
			}
		})
	}
}
