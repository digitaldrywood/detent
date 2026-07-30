package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestRecoverBlockedIssuesLogsDecisionForEveryBlockedIssue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issue      connector.Issue
		prepare    func(*State, connector.Issue)
		wantAction string
		wantReason string
	}{
		{
			name:       "relation-less park",
			issue:      dependencyAutoUnblockIssue("issue-no-predicate", blockedStatusState),
			wantAction: "hold",
			wantReason: "no_recovery_predicate",
		},
		{
			name: "human action",
			issue: func() connector.Issue {
				issue := dependencyAutoUnblockIssue("issue-human-action", blockedStatusState)
				issue.WorkpadSignal = &workpad.Signal{
					Source:      workpad.SourceStructured,
					Status:      workpad.StatusBlocked,
					HumanAction: "install production credentials",
				}
				return issue
			}(),
			wantAction: "hold",
			wantReason: "human_action",
		},
		{
			name: "dependency",
			issue: func() connector.Issue {
				issue := dependencyAutoUnblockIssue("issue-dependency", blockedStatusState)
				issue.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#59"}}
				return issue
			}(),
			wantAction: "defer",
			wantReason: "dependency_recovery",
		},
		{
			name: "workpad blocker",
			issue: func() connector.Issue {
				issue := dependencyAutoUnblockIssue("issue-workpad-blocker", blockedStatusState)
				issue.WorkpadSignal = &workpad.Signal{
					Source:   workpad.SourceStructured,
					Status:   workpad.StatusBlocked,
					Blockers: []workpad.Blocker{{Ref: "digitaldrywood/detent#59"}},
				}
				return issue
			}(),
			wantAction: "hold",
			wantReason: "workpad_blocker",
		},
		{
			name:  "operator stop",
			issue: dependencyAutoUnblockIssue("issue-operator-stop", blockedStatusState),
			prepare: func(state *State, issue connector.Issue) {
				state.Blocked[issue.ID] = Blocked{
					Issue:      issue,
					Source:     BlockedSourceOperatorStop,
					Reason:     "operator stopped run",
					StopReason: "operator requested stop",
				}
			},
			wantAction: "hold",
			wantReason: "operator_stop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			tracker := &dependencyAutoUnblockConnector{}
			orch := blockedCauseTestOrchestrator(tracker)
			orch.logger = slog.New(slog.NewTextHandler(&logs, nil))
			state := newState(orch.cfg)
			if tt.prepare != nil {
				tt.prepare(&state, tt.issue)
			}

			orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{tt.issue}, time.Date(2026, 7, 29, 18, 0, 0, 0, time.UTC))

			got := logs.String()
			for _, want := range []string{
				"msg=\"blocked recovery decision\"",
				"issue_id=" + tt.issue.ID,
				"action=" + tt.wantAction,
				"reason=" + tt.wantReason,
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("logs = %q, want %q", got, want)
				}
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want none", tracker.updates)
			}
		})
	}
}

func TestCauseBlockedRecoveryPersistsAndBoundsFingerprintAttempts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		predicate      string
		parked         runpkg.BlockedRecoverySnapshot
		current        runpkg.BlockedRecoverySnapshot
		wantTarget     string
		wantTransition bool
	}{
		{
			name:           "changed configuration fingerprint",
			predicate:      blockedRecoveryPredicateFingerprintChange,
			parked:         runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-old", Health: "ready", WorkspaceStatus: "missing"},
			current:        runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-new", Health: "ready", WorkspaceStatus: "missing"},
			wantTarget:     "Todo",
			wantTransition: true,
		},
		{
			name:      "unchanged no-progress cause",
			predicate: blockedRecoveryPredicateFingerprintChange,
			parked:    runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-same", Health: "ready", WorkspaceStatus: "missing"},
			current:   runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-same", Health: "ready", WorkspaceStatus: "missing"},
		},
		{
			name:      "stranded workspace once per unchanged fingerprint",
			predicate: blockedRecoveryPredicateOncePerFingerprint,
			parked: runpkg.BlockedRecoverySnapshot{
				ConfigFingerprint:    "config-same",
				WorkspaceFingerprint: "workspace-same",
				WorkspaceStatus:      "present",
				WorkspacePresent:     true,
				UnpushedCommits:      2,
				Health:               "ready",
			},
			current: runpkg.BlockedRecoverySnapshot{
				ConfigFingerprint:    "config-same",
				WorkspaceFingerprint: "workspace-same",
				WorkspaceStatus:      "present",
				WorkspacePresent:     true,
				UnpushedCommits:      2,
				Health:               "ready",
			},
			wantTarget:     autoPromoteReworkState,
			wantTransition: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := dependencyAutoUnblockIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), blockedStatusState)
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			parkTracker := &dependencyAutoUnblockConnector{}
			parkOrch := blockedCauseTestOrchestrator(parkTracker)
			parkOrch.workflowMetrics = metrics
			parkOrch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: tt.parked}
			parkedAt := time.Date(2026, 7, 29, 18, 30, 0, 0, time.UTC)
			metadata := parkOrch.newBlockedRecoveryMetadata(
				t.Context(),
				issue,
				RunModeImplement,
				noProgressLimitReason,
				tt.predicate,
				"Todo",
				DiffStats{},
			)
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)

			tracker := &dependencyAutoUnblockConnector{}
			orch := blockedCauseTestOrchestrator(tracker)
			orch.workflowMetrics = metrics
			orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: tt.current}
			state := newState(orch.cfg)

			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(time.Minute))

			if tt.wantTransition {
				if len(tracker.updates) != 1 || tracker.updates[0].state != tt.wantTarget {
					t.Fatalf("updates = %#v, want target %q", tracker.updates, tt.wantTarget)
				}
				if _, ok := transitioned[issue.ID]; !ok {
					t.Fatalf("transitioned[%q] missing", issue.ID)
				}
				currentFingerprint := blockedCauseFingerprint(orch.blockedCauseSignals(t.Context(), issue, RunModeImplement, metadata.BlockedRecovery.TargetState, DiffStats{}))
				assertWorkflowActionSignature(t, metrics, issue, workflowActionCauseBlockedRecovery, blockedCauseRecoverySignature(noProgressLimitReason, currentFingerprint))

				if tt.predicate == blockedRecoveryPredicateOncePerFingerprint {
					recordBlockedCausePark(t, metrics, issue, parkedAt.Add(2*time.Minute), metadata)
					restartedTracker := &dependencyAutoUnblockConnector{}
					restarted := blockedCauseTestOrchestrator(restartedTracker)
					restarted.workflowMetrics = metrics
					restarted.recoveryInspector = staticBlockedRecoveryInspector{snapshot: tt.current}
					restarted.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(3*time.Minute))
					if len(restartedTracker.updates) != 0 {
						t.Fatalf("restart updates = %#v, want consumed fingerprint held", restartedTracker.updates)
					}
				}
				return
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want unchanged cause held", tracker.updates)
			}
			if _, ok := transitioned[issue.ID]; ok {
				t.Fatalf("transitioned[%q] present", issue.ID)
			}
		})
	}
}

func blockedCauseTestOrchestrator(tracker *dependencyAutoUnblockConnector) *Orchestrator {
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	orch.cfg.BlockedRecovery.Enabled = false
	return orch
}

func recordBlockedCausePark(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
	metadata workflowLaneMetadata,
) {
	t.Helper()
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    blockedStatusState,
		Reason:       noProgressLimitReason,
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: workflowLaneMetadataJSON(issue, metadata),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}

type staticBlockedRecoveryInspector struct {
	snapshot runpkg.BlockedRecoverySnapshot
}

func (i staticBlockedRecoveryInspector) BlockedRecoverySnapshot(context.Context, runpkg.RunRequest) runpkg.BlockedRecoverySnapshot {
	return i.snapshot
}
