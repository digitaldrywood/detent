package config

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/scheduleowner"
)

func TestParseWorkflowScheduleOwnership(t *testing.T) {
	t.Parallel()
	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: hub_native
  api_key: token
  repository: example/issues
schedule_ownership:
  enabled: true
  backend: github_ref
  key: example/production
  repository: example/coordination
  branch: scheduler-state
  lease_seconds: 240
  heartbeat_seconds: 60
  retry_seconds: 10
  max_clock_skew_seconds: 10
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	got := workflow.Config.ScheduleOwnership
	if !got.Enabled || got.Backend != scheduleowner.BackendGitHubRef || got.Key != "example/production" ||
		got.Repository != "example/coordination" || got.Branch != "scheduler-state" || got.LeaseSeconds != 240 ||
		got.HeartbeatSeconds != 60 || got.RetrySeconds != 10 || got.MaxClockSkewSeconds != 10 {
		t.Fatalf("ScheduleOwnership = %#v", got)
	}
}

func TestScheduleOwnershipValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*scheduleowner.Config)
		want   string
	}{
		{name: "missing key", mutate: func(cfg *scheduleowner.Config) { cfg.Key = "" }, want: "key is required"},
		{name: "invalid repository", mutate: func(cfg *scheduleowner.Config) { cfg.Repository = "repo" }, want: "owner/name"},
		{name: "heartbeat too late", mutate: func(cfg *scheduleowner.Config) { cfg.HeartbeatSeconds = 880 }, want: "heartbeat_seconds"},
		{name: "clock skew too large", mutate: func(cfg *scheduleowner.Config) { cfg.MaxClockSkewSeconds = 450 }, want: "less than half"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Tracker.Kind = TrackerHubNative
			cfg.Tracker.APIKey = "token"
			cfg.Tracker.Repository = "example/issues"
			cfg.ScheduleOwnership.Enabled = true
			cfg.ScheduleOwnership.Key = "example/production"
			cfg.ScheduleOwnership.Repository = "example/coordination"
			tt.mutate(&cfg.ScheduleOwnership)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNativeScheduleOwnershipRequiresExplicitGitHubRepository(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		wantError  bool
	}{
		{name: "inherited tracker repository", wantError: true},
		{name: "explicit coordination repository", repository: "example/coordination"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Tracker.Kind = TrackerHubNative
			cfg.Tracker.Repository = "example/project"
			cfg.ScheduleOwnership.Enabled = true
			cfg.ScheduleOwnership.Key = "example/production"
			cfg.ScheduleOwnership.Repository = tt.repository
			err := cfg.Validate()
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "explicit schedule_ownership.repository") {
					t.Fatalf("Validate() error = %v, want explicit GitHub coordination repository", err)
				}
			} else if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestMappedNativeScheduleOwnershipDoesNotInheritGitHubRepository(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		wantError  bool
	}{
		{name: "implicit GitHub repository", wantError: true},
		{name: "empty repository", repository: "  repository: ''\n", wantError: true},
		{name: "blank repository", repository: "  repository: '  '\n", wantError: true},
		{name: "null repository", repository: "  repository: null\n", wantError: true},
		{name: "explicit coordination repository", repository: "  repository: example/coordination\n"},
		{name: "explicit tracker repository", repository: "  repository: example/project\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte("---\ntracker:\n  kind: hub_native\n  repository: example/project\nschedule_ownership:\n  enabled: true\n  key: example/production\n" + tt.repository + "---\nPrompt\n"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := workflow.Config.ForNativeTracker()
			err = cfg.Validate()
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "explicit schedule_ownership.repository") {
					t.Fatalf("mapped Validate() error = %v, want explicit repository", err)
				}
			} else if err != nil {
				t.Fatalf("mapped Validate() error = %v", err)
			}
		})
	}
}
