package buildinfo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDetectDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		running   Info
		installed Info
		want      Drift
	}{
		{
			name:      "matching commit",
			running:   Info{Version: "v1.2.2", Commit: "abcdef123456"},
			installed: Info{Version: "v1.2.3", Commit: "abcdef123456"},
			want:      Drift{Comparable: true},
		},
		{
			name:      "matching short commit",
			running:   Info{Version: "v1.2.3", Commit: "abcdef1"},
			installed: Info{Version: "v1.2.3", Commit: "abcdef123456"},
			want:      Drift{Comparable: true},
		},
		{
			name:      "different commit",
			running:   Info{Version: "v1.2.2", Commit: "abcdef123456"},
			installed: Info{Version: "v1.2.3", Commit: "123456abcdef"},
			want:      Drift{Comparable: true, Detected: true},
		},
		{
			name:      "version fallback matches",
			running:   Info{Version: "v1.2.3", Commit: "none"},
			installed: Info{Version: "v1.2.3", Commit: "none"},
			want:      Drift{Comparable: true},
		},
		{
			name:      "version fallback differs",
			running:   Info{Version: "v1.2.2", Commit: "none"},
			installed: Info{Version: "v1.2.3", Commit: "none"},
			want:      Drift{Comparable: true, Detected: true},
		},
		{
			name:      "unknown builds",
			running:   Info{Version: "dev", Commit: "none"},
			installed: Info{Version: "dev", Commit: "none"},
		},
		{
			name:      "missing running build",
			running:   Info{},
			installed: Info{Version: "v1.2.3", Commit: "abcdef123456"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := DetectDrift(tt.running, tt.installed); got != tt.want {
				t.Fatalf("DetectDrift() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReadBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		output   string
		runErr   error
		want     Info
		wantErr  string
		wantPath string
		wantArgs []string
	}{
		{
			name:     "build metadata",
			path:     " /opt/detent/bin/detent ",
			output:   `{"version":"v1.2.3","commit":"abcdef123456","build_date":"2026-08-14T00:00:00Z"}`,
			want:     Info{Version: "v1.2.3", Commit: "abcdef123456", Date: "2026-08-14T00:00:00Z"},
			wantPath: "/opt/detent/bin/detent",
			wantArgs: []string{"--format", "json", "version"},
		},
		{name: "runner failure", path: "/opt/detent", runErr: errors.New("permission denied"), wantErr: "read installed Detent build"},
		{name: "invalid output", path: "/opt/detent", output: "not-json", wantErr: "decode installed Detent build"},
		{name: "missing metadata", path: "/opt/detent", output: `{}`, wantErr: "did not report build metadata"},
		{name: "missing path", wantErr: "binary path is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string
			var gotArgs []string
			got, err := ReadBinary(context.Background(), tt.path, func(_ context.Context, path string, args ...string) (string, error) {
				gotPath = path
				gotArgs = append([]string(nil), args...)
				return tt.output, tt.runErr
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ReadBinary() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadBinary() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ReadBinary() = %#v, want %#v", got, tt.want)
			}
			if gotPath != tt.wantPath || !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("runner call = %q %#v, want %q %#v", gotPath, gotArgs, tt.wantPath, tt.wantArgs)
			}
		})
	}
}
