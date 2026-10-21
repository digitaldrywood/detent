package global

import (
	"strings"
	"testing"
	"time"
)

func TestHealthNotificationConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		yaml         string
		wantErr      string
		wantDebounce time.Duration
		wantTimeout  time.Duration
	}{
		{
			name:         "configured webhook",
			yaml:         "notifications:\n  health:\n    debounce_seconds: 30\n    webhook:\n      url: https://alerts.example.test/detent\n      headers:\n        Authorization: Bearer example\n      timeout_ms: 1200\n",
			wantDebounce: 30 * time.Second,
			wantTimeout:  1200 * time.Millisecond,
		},
		{
			name:         "omitted uses documented defaults",
			wantDebounce: time.Duration(DefaultHealthNotificationDebounceSeconds) * time.Second,
			wantTimeout:  time.Duration(DefaultNotificationWebhookTimeoutMS) * time.Millisecond,
		},
		{name: "relative URL", yaml: "notifications:\n  health:\n    webhook:\n      url: /alerts\n", wantErr: "absolute http or https URL"},
		{name: "zero debounce", yaml: "notifications:\n  health:\n    debounce_seconds: 0\n", wantErr: "must be a positive integer"},
		{name: "non-string header", yaml: "notifications:\n  health:\n    webhook:\n      url: https://alerts.example.test\n      headers:\n        Attempts: 3\n", wantErr: "must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte("apiVersion: detent/v1\nkind: GlobalConfig\nglobal:\n  max_concurrent_agents: 1\n  scheduling: weighted\nprojects: []\n" + tt.yaml)
			cfg, err := Parse(raw, "/tmp/global.yaml", WithProjectPathLiterals())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got := cfg.Notifications.Health.Debounce(); got != tt.wantDebounce {
				t.Fatalf("Debounce() = %s, want %s", got, tt.wantDebounce)
			}
			if got := cfg.Notifications.Health.Webhook.Timeout(); got != tt.wantTimeout {
				t.Fatalf("Timeout() = %s, want %s", got, tt.wantTimeout)
			}
		})
	}
}
