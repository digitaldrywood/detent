package config

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
)

func TestParseWorkflowScheduleOwnership(t *testing.T) {
	t.Parallel()
	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: github
  api_key: token
  github_status_source: label
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
		{name: "heartbeat too late", mutate: func(cfg *scheduleowner.Config) { cfg.HeartbeatSeconds = 280 }, want: "heartbeat_seconds"},
		{name: "clock skew too large", mutate: func(cfg *scheduleowner.Config) { cfg.MaxClockSkewSeconds = 150 }, want: "less than half"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Tracker.Kind = TrackerGitHub
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

func TestSchedulersEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "none", cfg: Default()},
		{name: "webhook only", cfg: func() Config {
			cfg := Default()
			cfg.Intake.Sources = []intake.Source{{Kind: intake.KindWebhook}}
			return cfg
		}()},
		{name: "scheduled intake", cfg: func() Config {
			cfg := Default()
			cfg.Intake.Sources = []intake.Source{{Kind: intake.KindSchedule}}
			return cfg
		}(), want: true},
		{name: "routine", cfg: func() Config {
			cfg := Default()
			cfg.Routines = []Routine{{Name: "audit"}}
			return cfg
		}(), want: true},
		{name: "admission", cfg: func() Config {
			cfg := Default()
			cfg.BacklogAdmission.Enabled = true
			return cfg
		}(), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.SchedulersEnabled(); got != tt.want {
				t.Fatalf("SchedulersEnabled() = %t, want %t", got, tt.want)
			}
		})
	}
}
