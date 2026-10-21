package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestScheduleBackendCredentialProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 22, 14, 9, 51, 0, time.UTC)
	codexScope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	claudeScope := backendcapacity.Scope{BackendID: "claude", BackendKind: "claude_code", Provider: "anthropic"}
	tests := []struct {
		name         string
		outage       BackendOutage
		changedScope backendcapacity.Scope
		want         bool
	}{
		{
			name:         "matching idle outage schedules prompt probe",
			outage:       BackendOutage{Scope: codexScope, NextProbeAt: now.Add(52 * time.Minute), ProbeAttempts: 7},
			changedScope: codexScope,
			want:         true,
		},
		{
			name:         "different backend leaves outage unchanged",
			outage:       BackendOutage{Scope: codexScope, NextProbeAt: now.Add(52 * time.Minute), ProbeAttempts: 7},
			changedScope: claudeScope,
		},
		{
			name:         "probe already in progress is not duplicated",
			outage:       BackendOutage{Scope: codexScope, ProbeIssueID: "issue-probe", ProbeAttempts: 7},
			changedScope: codexScope,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{})
			orch := &Orchestrator{cfg: cfg, now: func() time.Time { return now }}
			state := newState(cfg)
			state.BackendOutages[tt.outage.Scope.Key()] = tt.outage
			state.Retry["issue-codex"] = Retry{
				Issue:         connector.Issue{ID: "issue-codex"},
				DueAt:         now.Add(52 * time.Minute),
				CapacityScope: codexScope,
			}

			if got := orch.scheduleBackendCredentialProbe(&state, tt.changedScope, now); got != tt.want {
				t.Fatalf("scheduleBackendCredentialProbe() = %t, want %t", got, tt.want)
			}
			outage := state.BackendOutages[tt.outage.Scope.Key()]
			if !tt.want {
				if outage != tt.outage {
					t.Fatalf("outage = %#v, want unchanged %#v", outage, tt.outage)
				}
				return
			}
			if !outage.NextProbeAt.Equal(now) || outage.ProbeAttempts != 0 {
				t.Fatalf("outage = %#v, want immediate probe with reset attempts", outage)
			}
			if retry := state.Retry["issue-codex"]; !retry.DueAt.Equal(now) {
				t.Fatalf("retry due = %s, want %s", retry.DueAt, now)
			}
			if !stateEventExists(state, "backend_capacity_credential_changed") {
				t.Fatalf("events = %#v, want credential change event", state.RecentEvents)
			}
		})
	}
}

func TestCredentialTriggeredProbeFailureReentersNormalBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 22, 14, 9, 51, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	cfg := normalizeConfig(Config{})
	orch := &Orchestrator{cfg: cfg, now: func() time.Time { return now }}
	state := newState(cfg)
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:         scope,
		NextProbeAt:   now.Add(52 * time.Minute),
		ProbeAttempts: 7,
	}

	if !orch.scheduleBackendCredentialProbe(&state, scope, now) {
		t.Fatal("credential change did not schedule a probe")
	}
	orch.markBackendCapacityProbe(&state, scope.Key(), "issue-probe", now)
	orch.deferBackendCapacityProbe(&state, Running{CapacityScope: scope, CapacityProbe: true}, now.Add(time.Minute), errors.New("workspace setup failed"))

	outage := state.BackendOutages[scope.Key()]
	wantNextProbe := now.Add(time.Minute).Add(backendCapacityProbeDelayForAttempt(1))
	if outage.ProbeAttempts != 1 || outage.ProbeIssueID != "" || !outage.NextProbeAt.Equal(wantNextProbe) {
		t.Fatalf("outage = %#v, want ordinary first-failure backoff until %s", outage, wantNextProbe)
	}
	request := backendCapacityTestController{scope: scope}
	orch.capacityController = request
	if _, _, paused := orch.backendCapacityDispatch(&state, runpkg.RunRequest{Issue: connector.Issue{ID: "issue-next"}}, wantNextProbe.Add(-time.Second)); !paused {
		t.Fatal("dispatch admitted another probe before normal backoff elapsed")
	}
}
