package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
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

func TestRecoverBlockedIssuesSurfacesInvalidWorkpadHold(t *testing.T) {
	t.Parallel()

	invalid := dependencyAutoUnblockIssue("issue-invalid-workpad", blockedStatusState)
	invalid.WorkpadSignal = &workpad.Signal{
		Source:  workpad.SourceStructured,
		Status:  workpad.StatusBlocked,
		Invalid: &workpad.Invalid{Message: "status must be in_progress, blocked, or complete"},
	}
	deferred := dependencyAutoUnblockIssue("issue-deferred", blockedStatusState)
	deferred.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#999"}}
	root := cloneIssue(invalid)
	root.ID = "issue-root"
	root.Identifier = "digitaldrywood/detent#6"
	middle := dependencyAutoUnblockIssue("issue-middle", blockedStatusState)
	middle.Identifier = "digitaldrywood/detent#17"
	middle.BlockedBy = []connector.BlockedRef{{ID: root.ID, Identifier: root.Identifier}}
	leaf := dependencyAutoUnblockIssue("issue-leaf", blockedStatusState)
	leaf.Identifier = "digitaldrywood/detent#18"
	leaf.BlockedBy = []connector.BlockedRef{{ID: middle.ID, Identifier: middle.Identifier}}

	tests := []struct {
		name             string
		issues           []connector.Issue
		resolvedBlockers []connector.Issue
		assert           func(*testing.T, State)
	}{
		{
			name:   "invalid workpad hold needs human attention",
			issues: []connector.Issue{invalid},
			assert: func(t *testing.T, state State) {
				entry := state.Blocked[invalid.ID]
				if entry.RecoveryAction != "hold" || entry.RecoveryReason != "invalid_workpad_signal" || !entry.NeedsHumanAttention {
					t.Fatalf("blocked recovery = %#v", entry)
				}
				if entry.RecoveryReachability != "held" || !entry.RecoveryIntentResumable || entry.Recovery == nil || entry.Recovery.Resumable {
					t.Fatalf("recovery reachability = %#v", entry)
				}
				if !strings.Contains(entry.RecoveryRemedy, "fresh-work lane") || !strings.Contains(entry.RecoveryRemedy, "no pull request") {
					t.Fatalf("RecoveryRemedy = %q", entry.RecoveryRemedy)
				}
				row := state.Snapshot(time.Date(2026, 8, 12, 12, 1, 0, 0, time.UTC)).Blocked[0]
				if row.RecoveryAction != "hold" || row.RecoveryReachability != "held" || !row.NeedsHumanAttention || row.RecoveryRemedy != entry.RecoveryRemedy {
					t.Fatalf("snapshot blocked recovery = %#v", row)
				}
			},
		},
		{
			name:             "dependency defer stays machine-resolvable",
			issues:           []connector.Issue{deferred},
			resolvedBlockers: []connector.Issue{dependencyAutoUnblockIssue("issue-999", "In Progress")},
			assert: func(t *testing.T, state State) {
				entry := state.Blocked[deferred.ID]
				if entry.RecoveryAction != "defer" || entry.RecoveryReason != "dependency_recovery" || entry.NeedsHumanAttention {
					t.Fatalf("blocked recovery = %#v", entry)
				}
				if entry.RecoveryReachability != "deferred" || !entry.RecoveryIntentResumable {
					t.Fatalf("recovery reachability = %#v", entry)
				}
			},
		},
		{
			name:             "dependency chain attributes held root",
			issues:           []connector.Issue{leaf, middle, root},
			resolvedBlockers: []connector.Issue{middle, root},
			assert: func(t *testing.T, state State) {
				for _, issueID := range []string{leaf.ID, middle.ID} {
					entry := state.Blocked[issueID]
					if entry.NeedsHumanAttention || entry.RecoveryAction != "defer" || entry.RecoveryRoot == nil {
						t.Fatalf("Blocked[%q] = %#v", issueID, entry)
					}
					if entry.RecoveryRoot.IssueIdentifier != root.Identifier || entry.RecoveryRoot.Reason != "invalid_workpad_signal" {
						t.Fatalf("Blocked[%q].RecoveryRoot = %#v", issueID, entry.RecoveryRoot)
					}
				}
				rows := state.Snapshot(time.Date(2026, 8, 12, 12, 1, 0, 0, time.UTC)).Blocked
				for _, row := range rows {
					if row.ID == leaf.ID && (row.RecoveryRoot == nil || row.RecoveryRoot.IssueIdentifier != root.Identifier) {
						t.Fatalf("leaf snapshot root = %#v", row.RecoveryRoot)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &dependencyAutoUnblockConnector{blockers: tt.resolvedBlockers}
			orch := blockedCauseTestOrchestrator(tracker)
			state := newState(orch.cfg)
			for _, issue := range tt.issues {
				state.Blocked[issue.ID] = Blocked{
					Issue:    issue,
					Source:   BlockedSourceProjectStatus,
					Recovery: &workflowLaneBlockedRecoveryMetadata{Resumable: true},
				}
			}

			orch.recoverBlockedIssues(t.Context(), &state, tt.issues, time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
			tt.assert(t, state)
		})
	}
}

func TestCauseBlockedRecoveryAutoRecoversObsoleteArtifactSpendPark(t *testing.T) {
	t.Parallel()

	issue := dependencyAutoUnblockIssue("wi-artifact-legacy-spend", blockedStatusState)
	issue.Deliverable = &connector.Deliverable{Kind: "artifact"}
	parkedAt := time.Date(2026, 8, 31, 18, 30, 0, 0, time.UTC)
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	parkOrch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	parkOrch.cfg.DeliverableKind = "artifact"
	parkOrch.workflowMetrics = metrics
	metadata := parkOrch.newBlockedRecoveryMetadata(
		t.Context(),
		issue,
		RunModeImplement,
		spendProgressReason,
		blockedRecoveryPredicateFingerprintChange,
		autoPromoteReworkState,
		DiffStats{},
	)
	recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)

	tracker := &dependencyAutoUnblockConnector{}
	orch := blockedCauseTestOrchestrator(tracker)
	orch.cfg.DeliverableKind = "artifact"
	orch.workflowMetrics = metrics
	orch.workAttempts = &implementProgressAttemptStore{history: []store.WorkAttempt{{
		TerminalState: store.WorkAttemptTerminalNoProgress,
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			spendProgressMetadataKey: spendProgressRecord{
				BlockReason: spendProgressReason,
				Case:        spendProgressCaseNoPR,
			},
		}),
	}}}
	state := newState(orch.cfg)
	state.Blocked[issue.ID] = Blocked{
		Issue:     issue,
		Source:    BlockedSourceProjectStatus,
		BlockedAt: parkedAt,
		Recovery:  metadata.BlockedRecovery,
	}

	transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(time.Second))

	if len(tracker.updates) != 1 || tracker.updates[0].state != autoPromoteReworkState {
		t.Fatalf("updates = %#v, want immediate Rework recovery", tracker.updates)
	}
	if _, ok := transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if _, ok := state.Blocked[issue.ID]; ok {
		t.Fatalf("Blocked[%q] remains after recovery", issue.ID)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "checked PR evidence that the project cannot produce") {
		t.Fatalf("comments = %#v, want obsolete PR evidence explanation", tracker.comments)
	}
}

func TestCauseBlockedRecoveryPersistsAndBoundsFingerprintAttempts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		cause                 string
		predicate             string
		parked                runpkg.BlockedRecoverySnapshot
		current               runpkg.BlockedRecoverySnapshot
		parkedLabels          []string
		currentLabels         []string
		parkedWorkpad         *workpad.Signal
		currentWorkpad        *workpad.Signal
		wantTarget            string
		wantTransition        bool
		wantStableFingerprint bool
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
			name:                  "unchanged no-progress cause",
			predicate:             blockedRecoveryPredicateFingerprintChange,
			parked:                runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-same", Health: "ready", WorkspaceStatus: "missing"},
			current:               runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-same", Health: "ready", WorkspaceStatus: "missing"},
			wantStableFingerprint: true,
		},
		{
			name:      "attempt workspace churn leaves failure cause unchanged",
			predicate: blockedRecoveryPredicateFingerprintChange,
			parked: runpkg.BlockedRecoverySnapshot{
				ConfigFingerprint:    "config-same",
				WorkspaceFingerprint: "workspace-before-failure",
				WorkspaceStatus:      "clean",
				WorkspacePresent:     true,
				Health:               "ready",
			},
			current: runpkg.BlockedRecoverySnapshot{
				ConfigFingerprint:    "config-same",
				WorkspaceFingerprint: "workspace-after-failure",
				WorkspaceStatus:      "dirty",
				WorkspacePresent:     true,
				WorkspaceFiles:       1,
				Health:               "ready",
			},
			parkedLabels:  []string{"bug", "detent:in-progress"},
			currentLabels: []string{"bug", "detent:blocked"},
			parkedWorkpad: &workpad.Signal{
				Source: workpad.SourceStructured,
				Status: workpad.StatusInProgress,
				Fields: map[string]string{"attempt": "4"},
			},
			currentWorkpad: &workpad.Signal{
				Source: workpad.SourceStructured,
				Status: workpad.StatusInProgress,
				Fields: map[string]string{"attempt": "5"},
			},
			wantStableFingerprint: true,
		},
		{
			name:      "stranded workspace once per unchanged fingerprint",
			cause:     strandedUnpushedWorkReason,
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
			issue.Labels = append([]string(nil), tt.parkedLabels...)
			issue.WorkpadSignal = tt.parkedWorkpad
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			parkTracker := &dependencyAutoUnblockConnector{}
			parkOrch := blockedCauseTestOrchestrator(parkTracker)
			parkOrch.workflowMetrics = metrics
			parkOrch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: tt.parked}
			parkedAt := time.Date(2026, 7, 29, 18, 30, 0, 0, time.UTC)
			cause := tt.cause
			if cause == "" {
				cause = noProgressLimitReason
			}
			metadata := parkOrch.newBlockedRecoveryMetadata(
				t.Context(),
				issue,
				RunModeImplement,
				cause,
				tt.predicate,
				"Todo",
				DiffStats{},
			)
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)
			currentIssue := cloneIssue(issue)
			if tt.currentLabels != nil {
				currentIssue.Labels = append([]string(nil), tt.currentLabels...)
			}
			if tt.currentWorkpad != nil {
				currentIssue.WorkpadSignal = tt.currentWorkpad
			}

			tracker := &dependencyAutoUnblockConnector{}
			orch := blockedCauseTestOrchestrator(tracker)
			orch.workflowMetrics = metrics
			orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: tt.current}
			state := newState(orch.cfg)
			state.Blocked[issue.ID] = Blocked{
				Issue:     currentIssue,
				Source:    BlockedSourceProjectStatus,
				BlockedAt: parkedAt,
				Recovery:  metadata.BlockedRecovery,
			}
			currentFingerprint := blockedCauseFingerprint(cause, orch.blockedCauseSignals(t.Context(), currentIssue, RunModeImplement, metadata.BlockedRecovery.TargetState, DiffStats{}))
			if tt.wantStableFingerprint && currentFingerprint != metadata.BlockedRecovery.CauseFingerprint {
				t.Fatalf("current fingerprint = %q, parked fingerprint = %q", currentFingerprint, metadata.BlockedRecovery.CauseFingerprint)
			}

			recoveryAt := parkedAt.Add(time.Minute)
			if breakerParkCause(cause) {
				if tt.wantTransition {
					orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{currentIssue}, parkedAt.Add(defaultBreakerParkCooldown-time.Nanosecond))
					if len(tracker.updates) != 0 {
						t.Fatalf("updates before cooldown = %#v, want breaker held", tracker.updates)
					}
					blocked := state.Blocked[issue.ID]
					if blocked.RecoveryAction != "defer" || blocked.RecoveryReason != "breaker_cooldown_active" || blocked.NeedsHumanAttention {
						t.Fatalf("pre-cooldown recovery = %#v, want machine-deferred cooldown", blocked)
					}
				}
				recoveryAt = parkedAt.Add(defaultBreakerParkCooldown)
			}
			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{currentIssue}, recoveryAt)

			if tt.wantTransition {
				if len(tracker.updates) != 1 || tracker.updates[0].state != tt.wantTarget {
					t.Fatalf("updates = %#v, want target %q", tracker.updates, tt.wantTarget)
				}
				if _, ok := transitioned[issue.ID]; !ok {
					t.Fatalf("transitioned[%q] missing", issue.ID)
				}
				assertWorkflowActionSignature(t, metrics, issue, workflowActionCauseBlockedRecovery, blockedCauseRecoverySignature(cause, currentFingerprint))

				if tt.predicate == blockedRecoveryPredicateOncePerFingerprint {
					recordBlockedCausePark(t, metrics, issue, parkedAt.Add(2*time.Minute), metadata)
					restartedTracker := &dependencyAutoUnblockConnector{}
					restarted := blockedCauseTestOrchestrator(restartedTracker)
					restarted.workflowMetrics = metrics
					restarted.recoveryInspector = staticBlockedRecoveryInspector{snapshot: tt.current}
					restarted.recoverBlockedIssues(t.Context(), &state, []connector.Issue{currentIssue}, parkedAt.Add(3*time.Minute))
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
			if tt.wantStableFingerprint {
				blocked := state.Blocked[issue.ID]
				if blocked.RecoveryAction != "hold" || blocked.RecoveryReason != "cause_unchanged" || !blocked.NeedsHumanAttention {
					t.Fatalf("blocked recovery = %#v, want visible cause-unchanged hold", blocked)
				}
			}
		})
	}
}

func TestCauseBlockedRecoveryCooldownSurvivesRestart(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	issue := dependencyAutoUnblockIssue("issue-restarted-breaker-cooldown", blockedStatusState)
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	parkOrch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	parkOrch.workflowMetrics = metrics
	parkOrch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{
		ConfigFingerprint: "config-before",
		Health:            "ready",
	}}
	metadata := parkOrch.newBlockedRecoveryMetadata(
		t.Context(),
		issue,
		RunModeImplement,
		dispatchLoopDetectedReason,
		blockedRecoveryPredicateFingerprintChange,
		"Todo",
		DiffStats{},
	)
	recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)

	tracker := &dependencyAutoUnblockConnector{}
	restarted := blockedCauseTestOrchestrator(tracker)
	restarted.workflowMetrics = metrics
	restarted.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{
		ConfigFingerprint: "config-after",
		Health:            "ready",
	}}
	state := newState(restarted.cfg)

	restarted.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(defaultBreakerParkCooldown-time.Nanosecond))
	if len(tracker.updates) != 0 {
		t.Fatalf("pre-cooldown restart updates = %#v, want park held", tracker.updates)
	}
	restarted.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(defaultBreakerParkCooldown))
	if len(tracker.updates) != 1 || tracker.updates[0].state != "Todo" {
		t.Fatalf("post-cooldown restart updates = %#v, want Todo", tracker.updates)
	}
}

func TestBreakerParkCauseIncludesRetryCycleLimits(t *testing.T) {
	t.Parallel()

	for _, cause := range []string{terminalAttemptRetryLimitCause, workspacePreparationRetryLimitCause} {
		if !breakerParkCause(cause) {
			t.Fatalf("breakerParkCause(%q) = false, want true", cause)
		}
	}
}

func TestRepeatedFailureParkRecoveryUsesRecordedRESTBudgetFailures(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 8, 15, 23, 50, 0, 0, time.UTC)
	observedAt := parkedAt.Add(time.Hour)
	resetAt := observedAt.Add(time.Hour)
	budgetError := "run agent turn: worker github REST budget reached reserved headroom: remaining=940 reserve=1250 reset_at=2026-08-16T01:00:00Z"
	tests := []struct {
		name             string
		errorMessage     string
		remaining        int64
		consumed         bool
		workpadStatus    string
		wantTransition   bool
		wantAction       string
		wantReason       string
		wantSurfaceParts []string
		recoveryAfter    time.Duration
	}{
		{name: "budget recovers", errorMessage: budgetError, remaining: 4927, wantTransition: true},
		{name: "current park recovers with invalid workpad", errorMessage: budgetError, remaining: 4927, workpadStatus: workpad.StatusBlocked, wantTransition: true},
		{
			name:             "budget remains exhausted",
			errorMessage:     budgetError,
			remaining:        940,
			wantAction:       "defer",
			wantReason:       githubRESTBudgetWaitingReason,
			wantSurfaceParts: []string{"transient GitHub REST budget", "remaining=940/5000", "reserve=1250"},
		},
		{
			name:          "non-transient failure stays parked",
			errorMessage:  "run agent turn: runner transport closed",
			remaining:     4927,
			wantAction:    "hold",
			wantReason:    "cause_unchanged",
			recoveryAfter: defaultBreakerParkCooldown,
		},
		{
			name:             "same exhaustion episode does not rearm twice",
			errorMessage:     budgetError,
			remaining:        4927,
			consumed:         true,
			wantAction:       "hold",
			wantReason:       githubRESTBudgetRearmConsumedReason,
			wantSurfaceParts: []string{"capacity recovered", "automatic re-arm already used", "remaining=4927/5000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := dependencyAutoUnblockIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), blockedStatusState)
			if tt.workpadStatus != "" {
				issue.WorkpadSignal = &workpad.Signal{
					Source:  workpad.SourceStructured,
					Status:  tt.workpadStatus,
					Invalid: &workpad.Invalid{Message: "status must be in_progress, blocked, or complete"},
				}
			}
			tracker := &dependencyAutoUnblockConnector{}
			orch := blockedCauseTestOrchestrator(tracker)
			metadata := orch.newBlockedRecoveryMetadata(
				t.Context(),
				issue,
				RunModeImplement,
				"repeated_failure_circuit_breaker",
				blockedRecoveryPredicateFingerprintChange,
				"Todo",
				DiffStats{},
			)
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			if tt.consumed {
				signature := githubRESTBudgetRecoverySignature(githubRESTBudgetEvidence{
					Consumer:           telemetry.RESTConsumerSharedPool,
					CredentialIdentity: "github-rest:worker",
					ResetAt:            resetAt,
				}, 1250)
				consumedMetadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionGitHubRESTBudgetRecovery, signature)
				if _, err := metrics.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{
					ProjectID:    defaultWorkflowMetricsProjectID,
					IssueID:      issue.ID,
					Identifier:   issue.Identifier,
					IssueURL:     issue.URL,
					PhaseType:    store.WorkflowPhaseTypeLane,
					PhaseName:    "In Progress",
					Reason:       workflowActionGitHubRESTBudgetRecovery,
					Status:       "entered",
					StartedAt:    parkedAt.Add(-time.Minute),
					MetadataJSON: workflowLaneMetadataJSON(issue, consumedMetadata),
				}); err != nil {
					t.Fatalf("RecordWorkflowPhaseEvent() consumed recovery error = %v", err)
				}
			}
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)
			attempts := &implementProgressAttemptStore{}
			for attempt := repeatedFailureThreshold; attempt >= 1; attempt-- {
				attempts.history = append(attempts.history, store.WorkAttempt{
					ID:            int64(attempt),
					ProjectID:     defaultWorkflowMetricsProjectID,
					IssueID:       issue.ID,
					Identifier:    issue.Identifier,
					Lane:          "In Progress",
					AttemptNumber: attempt,
					Status:        store.WorkAttemptStatusTerminal,
					TerminalState: store.WorkAttemptTerminalFailure,
					ErrorClass:    workAttemptErrorRunner,
					ErrorMessage:  tt.errorMessage,
					CompletedAt:   parkedAt.Add(-time.Duration(repeatedFailureThreshold-attempt) * time.Minute),
				})
			}

			orch.workflowMetrics = metrics
			orch.workAttempts = attempts
			state := newState(orch.cfg)
			state.RateLimits = &telemetry.RateLimits{
				GitHubREST: &telemetry.RateLimitBucket{
					Remaining:  4999,
					Limit:      5000,
					ResetAt:    &resetAt,
					ObservedAt: &observedAt,
				},
				GitHubRESTBudgets: []telemetry.RESTBudget{{
					Consumer:            telemetry.RESTConsumerSharedPool,
					CredentialIdentity:  "github-rest:worker",
					EndpointFamily:      "shared credential pool",
					Resource:            "core",
					Remaining:           tt.remaining,
					Limit:               5000,
					MinRemainingReserve: 1250,
					ResetAt:             &resetAt,
					ObservedAt:          &observedAt,
				}},
			}
			state.Blocked[issue.ID] = Blocked{
				Issue:     issue,
				Source:    BlockedSourceProjectStatus,
				BlockedAt: parkedAt,
				Recovery:  metadata.BlockedRecovery,
			}

			recoveryAt := observedAt
			if tt.recoveryAfter > 0 {
				recoveryAt = parkedAt.Add(tt.recoveryAfter)
			}
			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, recoveryAt)

			if tt.wantTransition {
				if len(tracker.updates) != 1 || tracker.updates[0].state != "In Progress" {
					t.Fatalf("updates = %#v, want recovery to prior lane In Progress", tracker.updates)
				}
				if _, ok := transitioned[issue.ID]; !ok {
					t.Fatalf("transitioned[%q] missing", issue.ID)
				}
				return
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want park held", tracker.updates)
			}
			if _, ok := transitioned[issue.ID]; ok {
				t.Fatalf("transitioned[%q] present for held park", issue.ID)
			}
			orch.trackBlockedStatusIssues(t.Context(), &state, []connector.Issue{issue}, observedAt.Add(time.Minute))
			blocked := state.Blocked[issue.ID]
			if blocked.RecoveryAction != tt.wantAction || blocked.RecoveryReason != tt.wantReason {
				t.Fatalf("blocked recovery = %q/%q, want %q/%q", blocked.RecoveryAction, blocked.RecoveryReason, tt.wantAction, tt.wantReason)
			}
			for _, part := range tt.wantSurfaceParts {
				if !strings.Contains(blocked.Reason, part) {
					t.Fatalf("blocked reason = %q, want %q", blocked.Reason, part)
				}
			}
		})
	}
}

func TestBlockedCauseRecoveryUsesCurrentCauseAfterDependencyResolution(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		seedStale bool
	}{
		{name: "first observation"},
		{name: "supersedes stale dependency classification", seedStale: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parkedAt := time.Date(2026, 8, 18, 21, 47, 10, 0, time.UTC)
			issue := dependencyAutoUnblockIssue("issue-current-blocked-cause-"+strings.ReplaceAll(tt.name, " ", "-"), blockedStatusState)
			issue.BlockedBy = []connector.BlockedRef{{
				Identifier:   "gopherguides/corp#72",
				State:        "Done",
				TrackerState: connector.BlockedRefTrackerStateClosed,
				Source:       connector.BlockedRefSourceNative,
			}}
			blocker := dependencyAutoUnblockIssue("issue-resolved-blocker", "Done")
			blocker.Identifier = "gopherguides/corp#72"
			blocker.Closed = true
			cause := "no_commits_to_deliver: branch detent/gopher-corp-example has no local commits ahead"
			remedy := "return the issue to Todo when implementation work is ready to resume"
			metadata := workflowLaneMetadata{BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{
				Owner:          blockedRecoveryOwnerHuman,
				Cause:          cause,
				Predicate:      blockedRecoveryPredicateManaged,
				TargetState:    autoPromoteReworkState,
				RunMode:        RunModeImplement,
				HoldReason:     noCommitsToDeliverReason,
				OperatorRemedy: remedy,
			}}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)
			tracker := &dependencyAutoUnblockConnector{blockers: []connector.Issue{blocker}}
			orch := blockedCauseTestOrchestrator(tracker)
			orch.workflowMetrics = metrics
			orch.cfg.DependencyAutoUnblock = normalizeDependencyAutoUnblockConfig(DependencyAutoUnblockConfig{
				Enabled:      true,
				SourceStates: []string{blockedStatusState},
				TargetState:  "Todo",
				Readiness:    DependencyReadinessTerminalOrMerged,
			})
			state := newState(orch.cfg)
			if tt.seedStale {
				state.Blocked[issue.ID] = Blocked{
					Issue:          issue,
					Reason:         "blocked by project status",
					RecoveryReason: string(BlockedRecoveryReasonDependencyBlocker),
					BlockedAt:      parkedAt,
					Source:         BlockedSourceProjectStatus,
				}
			}

			if transitioned := orch.autoUnblockDependencyIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(time.Minute)); len(transitioned) != 0 {
				t.Fatalf("dependency auto-unblock transitioned cause-owned park: %#v", transitioned)
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("dependency auto-unblock updates = %#v, want cause-owned park held", tracker.updates)
			}

			orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(time.Minute))
			orch.trackBlockedStatusIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(2*time.Minute))

			blocked := state.Blocked[issue.ID]
			if blocked.RecoveryAction != "hold" || blocked.RecoveryReason != noCommitsToDeliverReason {
				t.Fatalf("blocked recovery = %q/%q, want hold/%s", blocked.RecoveryAction, blocked.RecoveryReason, noCommitsToDeliverReason)
			}
			if blocked.Reason != cause {
				t.Fatalf("blocked reason = %q, want current cause %q", blocked.Reason, cause)
			}
			if blocked.RecoveryRemedy != remedy || !blocked.NeedsHumanAttention {
				t.Fatalf("blocked recovery remedy = %q, needs human = %t", blocked.RecoveryRemedy, blocked.NeedsHumanAttention)
			}
		})
	}
}

func TestBlockedCauseRecoveryNamesUnresolvedDependency(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 22, 0, 0, 0, time.UTC)
	issue := dependencyAutoUnblockIssue("issue-unresolved-blocker", blockedStatusState)
	issue.BlockedBy = []connector.BlockedRef{{Identifier: "gopherguides/corp#526", State: "In Progress", Source: connector.BlockedRefSourceNative}}
	blocker := dependencyAutoUnblockIssue("issue-live-blocker", "In Progress")
	blocker.Identifier = "gopherguides/corp#526"
	tracker := &dependencyAutoUnblockConnector{blockers: []connector.Issue{blocker}}
	orch := blockedCauseTestOrchestrator(tracker)
	state := newState(orch.cfg)

	orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now)
	orch.trackBlockedStatusIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Minute))

	blocked := state.Blocked[issue.ID]
	if blocked.RecoveryAction != "defer" || blocked.RecoveryReason != "dependency_recovery" {
		t.Fatalf("blocked recovery = %q/%q, want defer/dependency_recovery", blocked.RecoveryAction, blocked.RecoveryReason)
	}
	if want := "waiting on gopherguides/corp#526 (In Progress)"; blocked.Reason != want {
		t.Fatalf("blocked reason = %q, want %q", blocked.Reason, want)
	}
}

func TestRepeatedFailureLegacyRESTBudgetParkRecoversAfterTrackerTransitionDelay(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 8, 15, 23, 29, 55, 0, time.UTC)
	recoveredAt := parkedAt.Add(17 * time.Hour)
	resetAt := recoveredAt.Add(time.Hour)
	observedAt := recoveredAt.Add(time.Second)
	tests := []struct {
		name             string
		identifier       string
		stageUpdateDelay time.Duration
		remaining        int64
		workpadStatus    string
		humanAction      string
		blockedBy        []connector.BlockedRef
		resolvedBlockers []connector.Issue
		parkOwner        string
		blockedSource    BlockedSource
		wantTransition   bool
		wantReason       string
	}{
		{name: "corp 519 invalid blocked workpad", identifier: "gopherguides/corp#519", stageUpdateDelay: 7 * time.Second, remaining: 1012, workpadStatus: workpad.StatusBlocked, wantTransition: true},
		{name: "corp 520 label update lag", identifier: "gopherguides/corp#520", stageUpdateDelay: 4 * time.Second, remaining: 945, wantTransition: true},
		{name: "corp 521 invalid in progress workpad", identifier: "gopherguides/corp#521", stageUpdateDelay: 5 * time.Second, remaining: 944, workpadStatus: workpad.StatusInProgress, wantTransition: true},
		{name: "later manual reblock", identifier: "gopherguides/corp#522", stageUpdateDelay: 15 * time.Second, remaining: 944},
		{
			name:             "invalid workpad with human action",
			identifier:       "gopherguides/corp#523",
			stageUpdateDelay: 5 * time.Second,
			remaining:        944,
			workpadStatus:    workpad.StatusBlocked,
			humanAction:      "confirm the recovery",
			wantReason:       "human_action",
		},
		{
			name:             "invalid workpad with unresolved dependency",
			identifier:       "gopherguides/corp#524",
			stageUpdateDelay: 5 * time.Second,
			remaining:        944,
			workpadStatus:    workpad.StatusBlocked,
			blockedBy:        []connector.BlockedRef{{Identifier: "gopherguides/corp#500"}},
			resolvedBlockers: []connector.Issue{{ID: "issue-500", Identifier: "gopherguides/corp#500", State: "In Progress"}},
			wantReason:       "invalid_workpad_signal",
		},
		{
			name:             "invalid workpad with operator stop",
			identifier:       "gopherguides/corp#525",
			stageUpdateDelay: 5 * time.Second,
			remaining:        944,
			workpadStatus:    workpad.StatusInProgress,
			blockedSource:    BlockedSourceOperatorStop,
			wantReason:       "operator_stop",
		},
		{
			name:             "invalid workpad with human owned park",
			identifier:       "gopherguides/corp#526",
			stageUpdateDelay: 5 * time.Second,
			remaining:        944,
			workpadStatus:    workpad.StatusBlocked,
			parkOwner:        blockedRecoveryOwnerHuman,
			wantReason:       "invalid_workpad_signal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := dependencyAutoUnblockIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), blockedStatusState)
			issue.Identifier = tt.identifier
			if tt.workpadStatus != "" {
				issue.WorkpadSignal = &workpad.Signal{
					Source:      workpad.SourceStructured,
					Status:      tt.workpadStatus,
					HumanAction: tt.humanAction,
					Invalid:     &workpad.Invalid{Message: "status must be in_progress, blocked, or complete"},
				}
			}
			issue.BlockedBy = tt.blockedBy
			stageUpdatedAt := parkedAt.Add(tt.stageUpdateDelay)
			issue.StageUpdatedAt = &stageUpdatedAt
			tracker := &dependencyAutoUnblockConnector{blockers: tt.resolvedBlockers}
			orch := blockedCauseTestOrchestrator(tracker)
			orch.cfg.Project.ID = "gopher-corp"
			metadata := orch.newBlockedRecoveryMetadata(
				t.Context(),
				issue,
				RunModeImplement,
				repeatedFailureCircuitBreakerCause,
				blockedRecoveryPredicateFingerprintChange,
				"Todo",
				DiffStats{},
			)
			if tt.parkOwner != "" {
				metadata.BlockedRecovery.Owner = tt.parkOwner
			}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)
			attempts := &implementProgressAttemptStore{}
			for attempt := repeatedFailureThreshold; attempt >= 1; attempt-- {
				attempts.history = append(attempts.history, store.WorkAttempt{
					ID:            int64(attempt),
					ProjectID:     "gopher-corp",
					IssueID:       issue.ID,
					Identifier:    issue.Identifier,
					Lane:          "Todo",
					AttemptNumber: attempt,
					Status:        store.WorkAttemptStatusTerminal,
					TerminalState: store.WorkAttemptTerminalFailure,
					ErrorClass:    workAttemptErrorRunner,
					ErrorMessage: fmt.Sprintf(
						"run agent turn: worker github REST budget reached reserved headroom: remaining=%d reserve=1250 reset_at=2026-08-15T23:35:48Z",
						tt.remaining,
					),
					CompletedAt: parkedAt.Add(-time.Duration(repeatedFailureThreshold-attempt) * time.Minute),
				})
			}

			orch.workflowMetrics = metrics
			orch.workAttempts = attempts
			prober := &staticGitHubRESTBudgetProber{budget: telemetry.RESTBudget{
				Consumer:            telemetry.RESTConsumerSharedPool,
				CredentialIdentity:  "github-rest:worker",
				Remaining:           4791,
				Limit:               5000,
				MinRemainingReserve: 1250,
				ResetAt:             &resetAt,
				ObservedAt:          &observedAt,
			}}
			orch.githubRESTBudgetProber = prober
			state := newState(orch.cfg)
			blockedSource := tt.blockedSource
			if blockedSource == "" {
				blockedSource = BlockedSourceProjectStatus
			}
			state.Blocked[issue.ID] = Blocked{
				Issue:     issue,
				Source:    blockedSource,
				BlockedAt: recoveredAt,
			}

			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, recoveredAt)

			_, didTransition := transitioned[issue.ID]
			if didTransition != tt.wantTransition {
				t.Fatalf("transitioned = %v, want %v; blocked = %#v; history calls = %d", didTransition, tt.wantTransition, state.Blocked[issue.ID], attempts.historyCalls)
			}
			if len(tracker.updates) != boolInt(tt.wantTransition) {
				t.Fatalf("updates = %#v, want transition %v", tracker.updates, tt.wantTransition)
			}
			if tt.wantTransition && tracker.updates[0].state != "Todo" {
				t.Fatalf("update state = %q, want Todo", tracker.updates[0].state)
			}
			if attempts.historyCalls != boolInt(tt.wantTransition) {
				t.Fatalf("history calls = %d, want %d", attempts.historyCalls, boolInt(tt.wantTransition))
			}
			if prober.calls != boolInt(tt.wantTransition) {
				t.Fatalf("probe calls = %d, want %d", prober.calls, boolInt(tt.wantTransition))
			}
			if tt.wantReason != "" && state.Blocked[issue.ID].RecoveryReason != tt.wantReason {
				t.Fatalf("recovery reason = %q, want %q", state.Blocked[issue.ID].RecoveryReason, tt.wantReason)
			}
			if tt.wantTransition {
				query := attempts.queries[0]
				if query.ProjectID != "gopher-corp" || query.IssueID != issue.ID || query.Identifier != issue.Identifier || query.Limit != repeatedFailureThreshold {
					t.Fatalf("history query = %#v, want current project and issue identity", query)
				}
			}
		})
	}
}

func TestRepeatedFailureRESTBudgetParkProbesAfterReset(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 8, 15, 23, 50, 0, 0, time.UTC)
	exhaustedResetAt := parkedAt.Add(10 * time.Minute)
	recoveryAt := exhaustedResetAt.Add(time.Minute)
	nextResetAt := recoveryAt.Add(time.Hour)
	oldObservedAt := parkedAt
	newObservedAt := recoveryAt
	budgetError := "run agent turn: worker github REST budget reached reserved headroom: consumer=worker credential_identity=github-rest:worker remaining=940 reserve=1250 reset_at=" + exhaustedResetAt.Format(time.RFC3339)
	tests := []struct {
		name           string
		probeBudget    telemetry.RESTBudget
		repetitions    int
		wantTransition bool
		wantProbeCalls int
	}{
		{
			name: "matching worker credential recovers",
			probeBudget: telemetry.RESTBudget{
				Consumer:            telemetry.RESTConsumerWorker,
				CredentialIdentity:  "github-rest:worker",
				EndpointFamily:      "worker credential",
				Resource:            "core",
				Remaining:           4927,
				Limit:               5000,
				MinRemainingReserve: 1250,
				ResetAt:             &nextResetAt,
				ObservedAt:          &newObservedAt,
			},
			wantTransition: true,
			wantProbeCalls: 1,
		},
		{
			name: "different credential cannot recover park",
			probeBudget: telemetry.RESTBudget{
				Consumer:            telemetry.RESTConsumerWorker,
				CredentialIdentity:  "github-rest:tracker",
				EndpointFamily:      "worker credential",
				Resource:            "core",
				Remaining:           4927,
				Limit:               5000,
				MinRemainingReserve: 1250,
				ResetAt:             &nextResetAt,
				ObservedAt:          &newObservedAt,
			},
			repetitions:    2,
			wantProbeCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := dependencyAutoUnblockIssue("issue-probe-"+strings.ReplaceAll(tt.name, " ", "-"), blockedStatusState)
			tracker := &dependencyAutoUnblockConnector{}
			orch := blockedCauseTestOrchestrator(tracker)
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			metadata := orch.newBlockedRecoveryMetadata(
				t.Context(),
				issue,
				RunModeImplement,
				repeatedFailureCircuitBreakerCause,
				blockedRecoveryPredicateGitHubRESTBudget,
				"In Progress",
				DiffStats{},
			)
			evidence, ok := githubRESTBudgetEvidenceFromMessage(budgetError)
			if !ok {
				t.Fatal("budget error did not parse")
			}
			evidence.TargetState = "In Progress"
			applyGitHubRESTBudgetEvidence(metadata.BlockedRecovery, evidence)
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)
			prober := &staticGitHubRESTBudgetProber{budget: tt.probeBudget}
			orch.workflowMetrics = metrics
			orch.githubRESTBudgetProber = prober

			state := newState(orch.cfg)
			state.RateLimits = &telemetry.RateLimits{
				GitHubREST: &telemetry.RateLimitBucket{
					Remaining:  4999,
					Limit:      5000,
					ResetAt:    &nextResetAt,
					ObservedAt: &newObservedAt,
				},
				GitHubRESTBudgets: []telemetry.RESTBudget{{
					Consumer:            telemetry.RESTConsumerWorker,
					CredentialIdentity:  "github-rest:worker",
					EndpointFamily:      "worker credential",
					Resource:            "core",
					Remaining:           940,
					Limit:               5000,
					MinRemainingReserve: 1250,
					ResetAt:             &exhaustedResetAt,
					ObservedAt:          &oldObservedAt,
				}},
			}
			state.Blocked[issue.ID] = Blocked{
				Issue:     issue,
				Source:    BlockedSourceProjectStatus,
				BlockedAt: parkedAt,
				Recovery:  metadata.BlockedRecovery,
			}

			var transitioned map[string]struct{}
			for range max(1, tt.repetitions) {
				transitioned = orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, recoveryAt)
			}

			if prober.calls != tt.wantProbeCalls {
				t.Fatalf("probe calls = %d, want %d", prober.calls, tt.wantProbeCalls)
			}
			_, didTransition := transitioned[issue.ID]
			if didTransition != tt.wantTransition {
				t.Fatalf("transitioned = %v, want %v", didTransition, tt.wantTransition)
			}
		})
	}
}

type staticGitHubRESTBudgetProber struct {
	budget telemetry.RESTBudget
	calls  int
}

func (p *staticGitHubRESTBudgetProber) ProbeGitHubRESTBudget(context.Context, connector.Issue) (telemetry.RESTBudget, bool, error) {
	p.calls++
	return p.budget, true, nil
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

func TestCauseBlockedRecoveryRebaselinesLegacyFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		cause          string
		predicate      string
		wantTransition bool
	}{
		{
			name:      "fingerprint change resumes after a later cause change",
			cause:     noProgressLimitReason,
			predicate: blockedRecoveryPredicateFingerprintChange,
		},
		{
			name:           "once per fingerprint resumes immediately",
			cause:          strandedUnpushedWorkReason,
			predicate:      blockedRecoveryPredicateOncePerFingerprint,
			wantTransition: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := dependencyAutoUnblockIssue("issue-legacy-"+strings.ReplaceAll(tt.name, " ", "-"), blockedStatusState)
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			parkOrch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
			parkOrch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-old", Health: "ready"}}
			metadata := parkOrch.newBlockedRecoveryMetadata(t.Context(), issue, RunModeImplement, tt.cause, tt.predicate, "Todo", DiffStats{})
			metadata.BlockedRecovery.CauseFingerprint = "legacy-fingerprint"
			metadata.BlockedRecovery.CauseFingerprintVersion = 0
			parkedAt := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)

			tracker := &dependencyAutoUnblockConnector{}
			orch := blockedCauseTestOrchestrator(tracker)
			orch.workflowMetrics = metrics
			orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-new", Health: "ready"}}
			state := newState(orch.cfg)
			state.Blocked[issue.ID] = Blocked{
				Issue:     issue,
				Source:    BlockedSourceProjectStatus,
				BlockedAt: parkedAt,
				Recovery:  metadata.BlockedRecovery,
			}

			recoveryAt := parkedAt.Add(defaultBreakerParkCooldown)
			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, recoveryAt)

			wantUpdates := 0
			if tt.wantTransition {
				wantUpdates = 1
			}
			if got := len(tracker.updates); got != wantUpdates {
				t.Fatalf("updates = %#v, want transition %t", tracker.updates, tt.wantTransition)
			}
			if _, ok := transitioned[issue.ID]; ok != tt.wantTransition {
				t.Fatalf("transitioned[%q] present = %t, want %t", issue.ID, ok, tt.wantTransition)
			}
			currentFingerprint := blockedCauseFingerprint(tt.cause, orch.blockedCauseSignals(t.Context(), issue, RunModeImplement, metadata.BlockedRecovery.TargetState, DiffStats{}))
			if !workflowTimelineHasCurrentFingerprint(t, metrics, issue, currentFingerprint) {
				t.Fatalf("workflow timeline does not contain current fingerprint %q", currentFingerprint)
			}
			if got := blockedLaneEntryCount(metrics.snapshot(), issue.ID); got != 1 {
				t.Fatalf("Blocked lane entries = %d, want legacy park updated in place", got)
			}
			if tt.wantTransition {
				return
			}
			blocked := state.Blocked[issue.ID]
			if blocked.RecoveryAction != "hold" || blocked.RecoveryReason != "cause_unchanged" || !blocked.NeedsHumanAttention || blocked.Recovery == nil {
				t.Fatalf("blocked recovery = %#v, want current-schema cause hold", blocked)
			}
			if blocked.Recovery.CauseFingerprintVersion != blockedCauseFingerprintVersion || blocked.Recovery.CauseFingerprint != currentFingerprint {
				t.Fatalf("recovery metadata = %#v, want version %d fingerprint %q", blocked.Recovery, blockedCauseFingerprintVersion, currentFingerprint)
			}

			restartedTracker := &dependencyAutoUnblockConnector{}
			restarted := blockedCauseTestOrchestrator(restartedTracker)
			restarted.workflowMetrics = metrics
			restarted.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-newer", Health: "ready"}}
			restartedState := newState(restarted.cfg)
			restarted.recoverBlockedIssues(t.Context(), &restartedState, []connector.Issue{issue}, recoveryAt.Add(time.Minute))
			if len(restartedTracker.updates) != 1 || restartedTracker.updates[0].state != "Todo" {
				t.Fatalf("restart updates = %#v, want Todo after current-schema cause change", restartedTracker.updates)
			}
		})
	}
}

func TestCauseBlockedRecoveryLegacyFingerprintUnsafeRemedy(t *testing.T) {
	t.Parallel()

	issue := dependencyAutoUnblockIssue("issue-legacy-unsafe", blockedStatusState)
	parkedAt := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	legacy := &workflowLaneBlockedRecoveryMetadata{
		Owner:                   blockedRecoveryOwnerOrchestrator,
		Cause:                   noProgressLimitReason,
		Predicate:               blockedRecoveryPredicateFingerprintChange,
		CauseFingerprint:        "legacy-fingerprint",
		CauseFingerprintVersion: 0,
		TargetState:             "Todo",
		RunMode:                 RunModeImplement,
	}
	orch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-new", Health: "ready"}}
	state := newState(orch.cfg)
	state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus, BlockedAt: parkedAt, Recovery: legacy}

	orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(defaultBreakerParkCooldown))

	blocked := state.Blocked[issue.ID]
	if blocked.RecoveryAction != "hold" || blocked.RecoveryReason != "legacy_cause_fingerprint" || !blocked.NeedsHumanAttention {
		t.Fatalf("blocked recovery = %#v, want unsafe legacy hold", blocked)
	}
	wantRemedy := "Park predates the current recovery schema; move the issue to Todo or Rework to re-evaluate it."
	if blocked.RecoveryRemedy != wantRemedy {
		t.Fatalf("recovery remedy = %q, want %q", blocked.RecoveryRemedy, wantRemedy)
	}
}

func TestCauseBlockedRecoveryUpgradeSweepRebaselinesLegacyBacklog(t *testing.T) {
	t.Parallel()

	projects := []struct {
		name  string
		count int
	}{
		{name: "digitaldrywood-video", count: 6},
		{name: "ghostreel", count: 2},
		{name: "creswoodcornersarmory", count: 1},
	}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	issues := make([]connector.Issue, 0, 9)
	state := newState(blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{}).cfg)
	parkedAt := time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC)
	for _, project := range projects {
		for index := range project.count {
			issue := dependencyAutoUnblockIssue(fmt.Sprintf("%s-legacy-%d", project.name, index+1), blockedStatusState)
			metadata := workflowLaneMetadata{BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{
				Owner:                   blockedRecoveryOwnerOrchestrator,
				Cause:                   noProgressLimitReason,
				Predicate:               blockedRecoveryPredicateFingerprintChange,
				CauseFingerprint:        "legacy-fingerprint",
				CauseFingerprintVersion: index % blockedCauseFingerprintVersion,
				TargetState:             "Todo",
				RunMode:                 RunModeImplement,
			}}
			recordBlockedCausePark(t, metrics, issue, parkedAt, metadata)
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus, BlockedAt: parkedAt, Recovery: metadata.BlockedRecovery}
			issues = append(issues, issue)
		}
	}

	orch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	orch.workflowMetrics = metrics
	orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-current", Health: "ready"}}
	orch.recoverBlockedIssues(t.Context(), &state, issues, parkedAt.Add(time.Minute))

	if len(state.Blocked) != 9 {
		t.Fatalf("blocked backlog size = %d, want 9 live parks", len(state.Blocked))
	}
	for _, issue := range issues {
		blocked := state.Blocked[issue.ID]
		if blocked.RecoveryReason == "legacy_cause_fingerprint" || blocked.Recovery == nil || blocked.Recovery.CauseFingerprintVersion != blockedCauseFingerprintVersion {
			t.Fatalf("Blocked[%q] = %#v, want current-schema recovery", issue.ID, blocked)
		}
	}
	eventCount := len(metrics.snapshot())
	if eventCount != len(issues) {
		t.Fatalf("workflow events after upgrade sweep = %d, want %d parks updated in place", eventCount, len(issues))
	}
	restarted := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	restarted.workflowMetrics = metrics
	restarted.recoveryInspector = orch.recoveryInspector
	restartedState := newState(restarted.cfg)
	restarted.recoverBlockedIssues(t.Context(), &restartedState, issues, parkedAt.Add(2*time.Minute))
	if got := len(metrics.snapshot()); got != eventCount {
		t.Fatalf("workflow events after restart = %d, want one-pass count %d", got, eventCount)
	}
	for _, issue := range issues {
		blocked := restartedState.Blocked[issue.ID]
		if blocked.RecoveryReason == "legacy_cause_fingerprint" || blocked.Recovery == nil || blocked.Recovery.CauseFingerprintVersion != blockedCauseFingerprintVersion {
			t.Fatalf("restarted Blocked[%q] = %#v, want durable current-schema recovery", issue.ID, blocked)
		}
	}
}

func blockedLaneEntryCount(events []store.WorkflowPhaseEvent, issueID string) int {
	count := 0
	for _, event := range events {
		if event.IssueID == issueID && event.PhaseType == store.WorkflowPhaseTypeLane &&
			normalizeState(event.PhaseName) == normalizeState(blockedStatusState) &&
			strings.EqualFold(strings.TrimSpace(event.Status), "entered") {
			count++
		}
	}
	return count
}

func workflowTimelineHasCurrentFingerprint(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	fingerprint string,
) bool {
	t.Helper()
	timeline, err := metrics.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: defaultWorkflowMetricsProjectID, IssueID: issue.ID})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	for _, event := range timeline.Events {
		metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if ok && metadata.BlockedRecovery != nil &&
			metadata.BlockedRecovery.CauseFingerprintVersion == blockedCauseFingerprintVersion &&
			metadata.BlockedRecovery.CauseFingerprint == fingerprint {
			return true
		}
	}
	return false
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

func TestRecoverBlockedReadyPullRequestToMerging(t *testing.T) {
	t.Parallel()

	baseSnapshot := runpkg.BlockedRecoverySnapshot{
		HeadSHA:                        "ready-head",
		WorkspacePresent:               true,
		WorkspaceStatus:                "present",
		PullRequestComparisonAvailable: true,
		Health:                         "ready",
	}
	tests := []struct {
		name         string
		cause        string
		owner        string
		mutateIssue  func(*connector.Issue)
		mutateConfig func(*Config)
		mutateState  func(*State, connector.Issue)
		snapshot     runpkg.BlockedRecoverySnapshot
		repetitions  int
		wantMerging  bool
	}{
		{name: "recoverable stranded work with ready pull request", cause: strandedUnpushedWorkReason, owner: blockedRecoveryOwnerOrchestrator, snapshot: baseSnapshot, wantMerging: true},
		{name: "recoverable dispatch loop with ready pull request", cause: dispatchLoopDetectedReason, owner: blockedRecoveryOwnerOrchestrator, snapshot: baseSnapshot, wantMerging: true},
		{
			name:  "invalid workpad repeated failure with ready linked pull request",
			cause: repeatedFailureCircuitBreakerCause,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.WorkpadSignal = &workpad.Signal{
					Source:  workpad.SourceStructured,
					Status:  workpad.StatusBlocked,
					Invalid: &workpad.Invalid{Message: "status must be in_progress, blocked, or complete"},
				}
			},
			snapshot:    baseSnapshot,
			wantMerging: true,
		},
		{name: "deliverable recovery with ready pull request", cause: deliverableRecoveryNeedsHumanReason + ": pushed branch ready has no recoverable pull request", owner: blockedRecoveryOwnerHuman, snapshot: baseSnapshot, wantMerging: true},
		{
			name:  "operator stop",
			cause: repeatedFailureCircuitBreakerCause,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateState: func(state *State, issue connector.Issue) {
				blocked := state.Blocked[issue.ID]
				blocked.Source = BlockedSourceOperatorStop
				state.Blocked[issue.ID] = blocked
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "human action",
			cause: repeatedFailureCircuitBreakerCause,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.WorkpadSignal.Status = workpad.StatusBlocked
				issue.WorkpadSignal.HumanAction = "provide production credentials"
			},
			snapshot: baseSnapshot,
		},
		{name: "human-owned repeated failure", cause: repeatedFailureCircuitBreakerCause, owner: blockedRecoveryOwnerHuman, snapshot: baseSnapshot},
		{
			name:  "draft pull request",
			cause: repeatedFailureCircuitBreakerCause,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.Draft = true
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "failing pull request",
			cause: repeatedFailureCircuitBreakerCause,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.CIStatus = "failure"
				issue.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Test", Status: "completed", Conclusion: "failure"}}
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "head mismatch without risk evidence",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			snapshot: runpkg.BlockedRecoverySnapshot{
				HeadSHA:          "workspace-head",
				WorkspacePresent: true,
				WorkspaceStatus:  "present",
				Health:           "ready",
			},
			wantMerging: true,
		},
		{
			name:  "untracked-only workspace with matching pull request head",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			snapshot: runpkg.BlockedRecoverySnapshot{
				HeadSHA:                        "ready-head",
				WorkspacePresent:               true,
				WorkspaceFiles:                 1,
				UnpushedCommits:                1,
				WorkspaceStatus:                "present",
				PullRequestComparisonAvailable: true,
				Health:                         "ready",
			},
			wantMerging: true,
		},
		{
			name:  "tracked workspace path",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			snapshot: runpkg.BlockedRecoverySnapshot{
				HeadSHA:                        "ready-head",
				WorkspacePresent:               true,
				WorkspaceFiles:                 1,
				TrackedPaths:                   []string{"tracked.go"},
				WorkspaceStatus:                "present",
				PullRequestComparisonAvailable: true,
				Health:                         "ready",
			},
		},
		{
			name:  "commit absent from pull request",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			snapshot: runpkg.BlockedRecoverySnapshot{
				HeadSHA:                        "workspace-head",
				WorkspacePresent:               true,
				UnpushedCommits:                1,
				CommitsNotInPullRequest:        []string{"abc123 fix: preserve work"},
				WorkspaceStatus:                "present",
				PullRequestComparisonAvailable: true,
				Health:                         "ready",
			},
		},
		{
			name:  "hydration unavailable",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.HydrationUnavailableReason = connector.PullRequestHydrationReasonRESTBudgetReserved
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "dependency not ready",
			cause: repeatedFailureCircuitBreakerCause,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.BlockedBy = []connector.BlockedRef{{ID: "blocker", Identifier: "digitaldrywood/detent#1700", State: "In Progress", Source: connector.BlockedRefSourceNative}}
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "approval revoked",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateConfig: func(cfg *Config) {
				cfg.AutoPromote.Gate.Kind = gate.KindHumanReview
				cfg.AutoPromote.Gate.ApprovalLabel = "human-approved"
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "ci trigger revoked",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.Labels = []string{}
			},
			mutateConfig: func(cfg *Config) {
				cfg.AutoPromote.Gate.CITriggerLabel = "run-ci"
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "auto promote disabled",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateConfig: func(cfg *Config) {
				cfg.AutoPromote.Enabled = false
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "merge fast path disabled",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateConfig: func(cfg *Config) {
				cfg.MergeFastPathEnabled = false
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "merging lane inactive",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateConfig: func(cfg *Config) {
				cfg.ActiveStates = []string{"Todo", "In Progress", "Rework"}
			},
			snapshot: baseSnapshot,
		},
		{name: "repeated reconciliation is idempotent", cause: strandedUnpushedWorkReason, owner: blockedRecoveryOwnerOrchestrator, snapshot: baseSnapshot, repetitions: 2, wantMerging: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := blockedReadyPullRequestIssue()
			if tt.mutateIssue != nil {
				tt.mutateIssue(&issue)
			}
			tracker := &dependencyAutoUnblockConnector{}
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents:  1,
				ActiveStates:         []string{"Todo", "In Progress", "Rework", autoPromoteMergingState},
				TerminalStates:       []string{"Done", "Cancelled"},
				MergeFastPathEnabled: true,
				AutoPromote: AutoPromoteConfig{
					Enabled: true,
					Gate:    gate.Config{Kind: gate.KindCommand, AutomatedReview: gate.AutomatedReviewOff},
				},
			})
			if tt.mutateConfig != nil {
				tt.mutateConfig(&cfg)
				cfg = normalizeConfig(cfg)
			}
			orch := &Orchestrator{
				cfg:               cfg,
				connector:         tracker,
				logger:            slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
				recoveryInspector: staticBlockedRecoveryInspector{snapshot: tt.snapshot},
			}
			state := newState(cfg)
			parkedAt := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
			state.Blocked[issue.ID] = Blocked{
				Issue:     issue,
				Reason:    tt.cause,
				BlockedAt: parkedAt,
				Source:    BlockedSourceProjectStatus,
				Recovery: &workflowLaneBlockedRecoveryMetadata{
					Owner:       tt.owner,
					Cause:       tt.cause,
					Predicate:   blockedRecoveryPredicateOncePerFingerprint,
					TargetState: autoPromoteReworkState,
					RunMode:     RunModeImplement,
				},
			}
			if tt.mutateState != nil {
				tt.mutateState(&state, issue)
			}

			repetitions := max(1, tt.repetitions)
			for range repetitions {
				orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(time.Minute))
			}

			if tt.wantMerging {
				if len(tracker.updates) != 1 || tracker.updates[0] != (dependencyAutoUnblockUpdate{issueID: issue.ID, state: autoPromoteMergingState}) {
					t.Fatalf("updates = %#v, want one Merging transition", tracker.updates)
				}
				if _, ok := state.Blocked[issue.ID]; ok {
					t.Fatalf("Blocked[%q] present after reconciliation", issue.ID)
				}
				return
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want none", tracker.updates)
			}
		})
	}
}

func TestRecoverBlockedReadyPullRequestExactHeadLookup(t *testing.T) {
	t.Parallel()

	const (
		branch     = "detent/exact-head-recovery"
		repository = "digitaldrywood/detent"
	)
	readyPullRequest := *blockedReadyPullRequestIssue().PullRequest
	readyPullRequest.BranchName = branch
	tests := []struct {
		name              string
		cause             string
		linked            bool
		lookupPullRequest connector.PullRequest
		lookupFound       bool
		lookupErr         error
		invalidWorkpad    bool
		wantLookupCalls   int
		wantHydrateCalls  int
		wantWaitCalls     int
		wantAction        string
		wantReason        string
		wantMerging       bool
	}{
		{
			name:              "unlinked exact-head open pull request reconciles",
			lookupPullRequest: readyPullRequest,
			lookupFound:       true,
			wantLookupCalls:   1,
			wantHydrateCalls:  1,
			wantMerging:       true,
		},
		{
			name:              "invalid workpad repeated failure with unlinked exact-head pull request reconciles",
			cause:             repeatedFailureCircuitBreakerCause,
			lookupPullRequest: readyPullRequest,
			lookupFound:       true,
			invalidWorkpad:    true,
			wantLookupCalls:   1,
			wantHydrateCalls:  1,
			wantMerging:       true,
		},
		{
			name:            "unlinked with no pull request holds accurately",
			cause:           repeatedFailureCircuitBreakerCause,
			invalidWorkpad:  true,
			wantLookupCalls: 1,
			wantAction:      "hold",
			wantReason:      blockedReadyPullRequestLookupNoneReason,
		},
		{
			name:            "lookup unavailable defers after bounded retries",
			cause:           repeatedFailureCircuitBreakerCause,
			lookupErr:       connector.ErrResourceExhausted,
			invalidWorkpad:  true,
			wantLookupCalls: blockedReadyPullRequestLookupAttempts,
			wantWaitCalls:   blockedReadyPullRequestLookupAttempts - 1,
			wantAction:      "defer",
			wantReason:      blockedReadyPullRequestLookupUnavailableReason,
		},
		{
			name:        "linked pull request fast path remains unchanged",
			linked:      true,
			wantMerging: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cause := tt.cause
			if cause == "" {
				cause = strandedUnpushedWorkReason
			}

			issue := blockedReadyPullRequestIssue()
			if !tt.linked {
				issue.PRNumber = nil
				issue.PullRequest = nil
			}
			if tt.invalidWorkpad {
				issue.WorkpadSignal = &workpad.Signal{
					Source: workpad.SourceStructured,
					Status: workpad.StatusBlocked,
					Invalid: &workpad.Invalid{
						Message: "status must be in_progress, blocked, or complete",
					},
				}
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: human-review\nblockers: []\nhuman_action: null\n```",
				}}
			}
			tracker := &blockedReadyPullRequestLookupConnector{
				dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{},
				pullRequest:                    tt.lookupPullRequest,
				found:                          tt.lookupFound,
				err:                            tt.lookupErr,
			}
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents:  1,
				ActiveStates:         []string{"Todo", "In Progress", "Rework", autoPromoteMergingState},
				TerminalStates:       []string{"Done", "Cancelled"},
				MergeFastPathEnabled: true,
				AutoPromote: AutoPromoteConfig{
					Enabled: true,
					Gate:    gate.Config{Kind: gate.KindCommand, AutomatedReview: gate.AutomatedReviewOff},
				},
			})
			var logs bytes.Buffer
			waitCalls := 0
			orch := &Orchestrator{
				cfg:               cfg,
				connector:         tracker,
				logger:            slog.New(slog.NewTextHandler(&logs, nil)),
				recoveryInspector: staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{HeadSHA: readyPullRequest.HeadSHA, WorkspacePresent: true, WorkspaceStatus: "present", Health: "ready"}},
				deliverableRecoveryWait: func(context.Context, time.Duration) bool {
					waitCalls++
					return true
				},
			}
			state := newState(cfg)
			parkedAt := time.Date(2026, 8, 14, 13, 15, 0, 0, time.UTC)
			blockedIssue := cloneIssue(issue)
			blockedIssue.BranchName = branch
			state.Blocked[issue.ID] = Blocked{
				Issue:     blockedIssue,
				Reason:    cause,
				BlockedAt: parkedAt,
				Source:    BlockedSourceProjectStatus,
				Recovery: &workflowLaneBlockedRecoveryMetadata{
					Owner:       blockedRecoveryOwnerOrchestrator,
					Cause:       cause,
					Predicate:   blockedRecoveryPredicateOncePerFingerprint,
					TargetState: autoPromoteReworkState,
					RunMode:     RunModeImplement,
				},
			}

			orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(time.Minute))

			if tracker.lookupCalls != tt.wantLookupCalls {
				t.Fatalf("lookup calls = %d, want %d", tracker.lookupCalls, tt.wantLookupCalls)
			}
			if tracker.hydrateCalls != tt.wantHydrateCalls {
				t.Fatalf("hydrate calls = %d, want %d", tracker.hydrateCalls, tt.wantHydrateCalls)
			}
			if waitCalls != tt.wantWaitCalls {
				t.Fatalf("retry waits = %d, want %d", waitCalls, tt.wantWaitCalls)
			}
			if tracker.lookupCalls > 0 && (tracker.repository != repository || tracker.branch != branch || tracker.headSHA != readyPullRequest.HeadSHA) {
				t.Fatalf("lookup = (%q, %q, %q), want (%q, %q, %q)", tracker.repository, tracker.branch, tracker.headSHA, repository, branch, readyPullRequest.HeadSHA)
			}
			if tracker.lookupCalls > 0 {
				wantOutcome := tt.wantReason
				if tt.lookupFound {
					wantOutcome = blockedReadyPullRequestLookupFoundReason
				}
				if !strings.Contains(logs.String(), "reason="+wantOutcome) {
					t.Fatalf("logs = %q, want lookup outcome %q", logs.String(), wantOutcome)
				}
			}
			if tt.wantMerging {
				if len(tracker.updates) != 1 || tracker.updates[0] != (dependencyAutoUnblockUpdate{issueID: issue.ID, state: autoPromoteMergingState}) {
					t.Fatalf("updates = %#v, want one Merging transition", tracker.updates)
				}
				return
			}
			blocked := state.Blocked[issue.ID]
			if blocked.RecoveryAction != tt.wantAction || blocked.RecoveryReason != tt.wantReason {
				t.Fatalf("blocked recovery = %#v, want action %q reason %q", blocked, tt.wantAction, tt.wantReason)
			}
			if tt.wantReason == blockedReadyPullRequestLookupNoneReason && !strings.Contains(blocked.RecoveryRemedy, "No PR found") {
				t.Fatalf("recovery remedy = %q, want accurate no-PR outcome", blocked.RecoveryRemedy)
			}
		})
	}
}

func blockedReadyPullRequestIssue() connector.Issue {
	prNumber := 1776
	issue := dependencyAutoUnblockIssue("issue-ready-pr", blockedStatusState)
	issue.PRNumber = &prNumber
	issue.PRRepository = "digitaldrywood/detent"
	issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusComplete}
	issue.PullRequest = &connector.PullRequest{
		Number:          prNumber,
		URL:             "https://github.com/digitaldrywood/detent/pull/1776",
		State:           "OPEN",
		MergeableState:  "clean",
		HeadSHA:         "ready-head",
		BaseSHA:         "ready-base",
		DiffFingerprint: "ready-diff",
		CIStatus:        "success",
		CheckRunCount:   1,
	}
	return issue
}

type blockedReadyPullRequestLookupConnector struct {
	*dependencyAutoUnblockConnector
	pullRequest  connector.PullRequest
	found        bool
	err          error
	repository   string
	branch       string
	headSHA      string
	lookupCalls  int
	hydrateCalls int
}

func (c *blockedReadyPullRequestLookupConnector) LookupPullRequestByHead(_ context.Context, repository string, branch string, headSHA string) (connector.PullRequest, bool, error) {
	c.lookupCalls++
	c.repository = repository
	c.branch = branch
	c.headSHA = headSHA
	return c.pullRequest, c.found, c.err
}

func (c *blockedReadyPullRequestLookupConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.hydrateCalls++
	return issue, nil
}

func blockedCauseTestOrchestrator(tracker *dependencyAutoUnblockConnector) *Orchestrator {
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	orch.cfg.BlockedRecovery.Enabled = false
	orch.cfg.StatusLabelPrefix = "detent:"
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
