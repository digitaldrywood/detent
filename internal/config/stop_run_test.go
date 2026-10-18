package config

import (
	"strings"
	"testing"
)

func TestStopRunTargetStateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		target    string
		active    []string
		observed  []string
		terminal  []string
		wantError string
	}{
		{name: "observed non active state", target: "Paused", active: []string{"Todo", "In Progress"}, observed: []string{"Paused", "Blocked"}, terminal: []string{"Done"}},
		{name: "missing observed state", target: "Paused", active: []string{"Todo"}, observed: []string{"Blocked"}, terminal: []string{"Done"}, wantError: "must be included in tracker.observed_states"},
		{name: "active state", target: "Todo", active: []string{"Todo"}, observed: []string{"Todo", "Blocked"}, terminal: []string{"Done"}, wantError: "must not be an active state"},
		{name: "terminal state", target: "Done", active: []string{"Todo"}, observed: []string{"Done", "Blocked"}, terminal: []string{"Done"}, wantError: "must not be a terminal state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Tracker.Kind = TrackerMemory
			cfg.Tracker.ActiveStates = tt.active
			cfg.Tracker.ObservedStates = tt.observed
			cfg.Tracker.TerminalStates = tt.terminal
			cfg.Agent.StopRun.TargetState = tt.target
			err := cfg.Validate()
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestStopRunTargetStateDefaultsToBlocked(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Agent.StopRun.TargetState = ""
	cfg.normalize()
	if cfg.Agent.StopRun.TargetState != "Blocked" {
		t.Fatalf("TargetState = %q, want Blocked", cfg.Agent.StopRun.TargetState)
	}
}
