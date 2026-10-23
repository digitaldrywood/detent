package orchestrator

import (
	"context"
	"strings"
	"testing"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/workpad"
)

// Exercise both observations synchronously: the run loop may publish either
// snapshot before a reader observes the park.

func TestBreakerRecoveryMetadataRemembersPreviousLane(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cause string
	}{
		{name: "instant failure", cause: instantFailureCircuitBreakerCause},
		{name: "repeated failure", cause: repeatedFailureCircuitBreakerCause},
		{name: "token ceiling", cause: "token_ceiling_circuit_breaker"},
		{name: "terminal attempt retry", cause: terminalAttemptRetryLimitCause},
		{name: "workspace preparation retry", cause: workspacePreparationRetryLimitCause},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := dependencyAutoUnblockIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), "In Progress")
			orch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
			metadata := orch.newBlockedRecoveryMetadata(
				t.Context(),
				issue,
				RunModeImplement,
				tt.cause,
				blockedRecoveryPredicateFingerprintChange,
				"Todo",
				DiffStats{},
			)

			if metadata.BlockedRecovery.Predicate != blockedRecoveryPredicateBreakerCooldown {
				t.Fatalf("predicate = %q, want breaker_cooldown_elapsed", metadata.BlockedRecovery.Predicate)
			}
			if metadata.BlockedRecovery.TargetState != issue.State {
				t.Fatalf("target state = %q, want previous lane %q", metadata.BlockedRecovery.TargetState, issue.State)
			}
		})
	}
}

func TestBreakerParkCauseIncludesRetryCycleLimits(t *testing.T) {
	t.Parallel()

	for _, cause := range []string{instantFailureCircuitBreakerCause, terminalAttemptRetryLimitCause, workspacePreparationRetryLimitCause} {
		if !breakerParkCause(cause) {
			t.Fatalf("breakerParkCause(%q) = false, want true", cause)
		}
	}
}

func TestBlockedCauseFingerprintTracksNormalizedLabels(t *testing.T) {
	t.Parallel()

	original := blockedCauseSignals{Labels: blockedCauseLabels([]string{"bug", "detent:in-progress"}, "detent:")}
	reordered := blockedCauseSignals{Labels: blockedCauseLabels([]string{" DETENT:BLOCKED ", "Bug", "bug"}, "detent:")}
	withOverride := blockedCauseSignals{Labels: blockedCauseLabels([]string{"bug", "detent:blocked", "agent:max-tokens"}, "detent:")}

	if blockedCauseFingerprint(noProgressLimitReason, original) != blockedCauseFingerprint(noProgressLimitReason, reordered) {
		t.Fatal("status label transition changed the cause fingerprint")
	}
	if blockedCauseFingerprint(noProgressLimitReason, original) == blockedCauseFingerprint(noProgressLimitReason, withOverride) {
		t.Fatal("recovery-affecting label did not change the cause fingerprint")
	}
	if blockedCauseFingerprint(noProgressLimitReason, original) == blockedCauseFingerprint(spendProgressReason, original) {
		t.Fatal("failure cause did not change the cause fingerprint")
	}
}

func TestBlockedCauseFingerprintIgnoresUnrelatedBaseForBreakerParks(t *testing.T) {
	t.Parallel()

	parked := blockedCauseSignals{ConfigFingerprint: "config-same", BaseFingerprint: "base-before", Health: "ready"}
	current := parked
	current.BaseFingerprint = "base-after-unrelated-merge"

	for _, cause := range []string{
		noProgressLimitReason,
		dispatchLoopDetectedReason,
		spendProgressReason,
		repeatedFailureCircuitBreakerCause,
		"token_ceiling_circuit_breaker",
	} {
		if blockedCauseFingerprint(cause, parked) != blockedCauseFingerprint(cause, current) {
			t.Fatalf("breaker cause %q changed after unrelated base movement", cause)
		}
	}
	if blockedCauseFingerprint(strandedUnpushedWorkReason, parked) == blockedCauseFingerprint(strandedUnpushedWorkReason, current) {
		t.Fatal("non-breaker recovery fingerprint ignored base movement")
	}
}

func TestBlockedCauseFingerprintIgnoresAttemptMutatedSignals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current blockedCauseSignals
	}{
		{
			name: "workspace churn",
			current: blockedCauseSignals{
				ConfigFingerprint:    "config-same",
				WorkspaceFingerprint: "workspace-after-failure",
				WorkspaceStatus:      "dirty",
				WorkspacePresent:     true,
				WorkspaceFiles:       1,
				UnpushedCommits:      1,
				Health:               "ready",
			},
		},
		{
			name: "workpad churn",
			current: blockedCauseSignals{
				ConfigFingerprint: "config-same",
				Health:            "ready",
				Workpad: &workpad.Signal{
					Source: workpad.SourceStructured,
					Status: workpad.StatusInProgress,
					Fields: map[string]string{"attempt": "5"},
				},
			},
		},
	}

	parked := blockedCauseSignals{
		ConfigFingerprint:    "config-same",
		WorkspaceFingerprint: "workspace-before-failure",
		WorkspaceStatus:      "clean",
		WorkspacePresent:     true,
		Health:               "ready",
		Workpad: &workpad.Signal{
			Source: workpad.SourceStructured,
			Status: workpad.StatusInProgress,
			Fields: map[string]string{"attempt": "4"},
		},
	}
	want := blockedCauseFingerprint("instant_failure_circuit_breaker", parked)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := blockedCauseFingerprint("instant_failure_circuit_breaker", tt.current); got != want {
				t.Fatalf("fingerprint = %q, want %q", got, want)
			}
		})
	}
}

func TestBlockedRecoveryMetadataSeparatesIntentFromReachability(t *testing.T) {
	t.Parallel()

	issue := dependencyAutoUnblockIssue("issue-resume-intent", blockedStatusState)
	orch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{
		WorkspacePresent: true,
		WorkspaceFiles:   1,
		WorkspaceStatus:  "present",
		Health:           "ready",
	}}
	metadata := orch.newBlockedRecoveryMetadata(
		t.Context(),
		issue,
		RunModeImplement,
		noProgressLimitReason,
		blockedRecoveryPredicateFingerprintChange,
		"Todo",
		DiffStats{},
	)
	raw := workflowLaneMetadataJSON(issue, metadata)
	if !strings.Contains(raw, `"intent_resumable":true`) {
		t.Fatalf("metadata = %s", raw)
	}
	if !strings.Contains(raw, `"cause_fingerprint_version":3`) {
		t.Fatalf("metadata = %s", raw)
	}
	if strings.Contains(raw, `"resumable":true`) {
		t.Fatalf("metadata retains ambiguous resumable field: %s", raw)
	}
}

func blockedCauseTestOrchestrator(tracker *dependencyAutoUnblockConnector) *Orchestrator {
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	orch.cfg.BlockedRecovery.Enabled = false
	orch.cfg.StatusLabelPrefix = "detent:"
	return orch
}

type staticBlockedRecoveryInspector struct {
	snapshot runpkg.BlockedRecoverySnapshot
}

func (i staticBlockedRecoveryInspector) BlockedRecoverySnapshot(context.Context, runpkg.RunRequest) runpkg.BlockedRecoverySnapshot {
	return i.snapshot
}
