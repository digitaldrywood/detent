package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
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

func TestCauseBlockedRecoveryPersistsAndBoundsFingerprintAttempts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
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
			currentFingerprint := blockedCauseFingerprint(noProgressLimitReason, orch.blockedCauseSignals(t.Context(), currentIssue, RunModeImplement, metadata.BlockedRecovery.TargetState, DiffStats{}))
			if tt.wantStableFingerprint && currentFingerprint != metadata.BlockedRecovery.CauseFingerprint {
				t.Fatalf("current fingerprint = %q, parked fingerprint = %q", currentFingerprint, metadata.BlockedRecovery.CauseFingerprint)
			}

			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{currentIssue}, parkedAt.Add(time.Minute))

			if tt.wantTransition {
				if len(tracker.updates) != 1 || tracker.updates[0].state != tt.wantTarget {
					t.Fatalf("updates = %#v, want target %q", tracker.updates, tt.wantTarget)
				}
				if _, ok := transitioned[issue.ID]; !ok {
					t.Fatalf("transitioned[%q] missing", issue.ID)
				}
				assertWorkflowActionSignature(t, metrics, issue, workflowActionCauseBlockedRecovery, blockedCauseRecoverySignature(noProgressLimitReason, currentFingerprint))

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

func TestCauseBlockedRecoveryHoldsLegacyFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		predicate string
	}{
		{name: "fingerprint change", predicate: blockedRecoveryPredicateFingerprintChange},
		{name: "once per fingerprint", predicate: blockedRecoveryPredicateOncePerFingerprint},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := dependencyAutoUnblockIssue("issue-legacy-"+strings.ReplaceAll(tt.name, " ", "-"), blockedStatusState)
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			parkOrch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
			parkOrch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{ConfigFingerprint: "config-old", Health: "ready"}}
			metadata := parkOrch.newBlockedRecoveryMetadata(t.Context(), issue, RunModeImplement, noProgressLimitReason, tt.predicate, "Todo", DiffStats{})
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

			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, parkedAt.Add(time.Minute))

			if len(tracker.updates) != 0 || len(transitioned) != 0 {
				t.Fatalf("updates = %#v, transitioned = %#v, want legacy fingerprint held", tracker.updates, transitioned)
			}
			blocked := state.Blocked[issue.ID]
			if blocked.RecoveryAction != "hold" || blocked.RecoveryReason != "legacy_cause_fingerprint" || !blocked.NeedsHumanAttention {
				t.Fatalf("blocked recovery = %#v, want visible legacy-fingerprint hold", blocked)
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
	if !strings.Contains(raw, `"cause_fingerprint_version":2`) {
		t.Fatalf("metadata = %s", raw)
	}
	if strings.Contains(raw, `"resumable":true`) {
		t.Fatalf("metadata retains ambiguous resumable field: %s", raw)
	}
}

func TestRecoverBlockedReadyPullRequestToMerging(t *testing.T) {
	t.Parallel()

	baseSnapshot := runpkg.BlockedRecoverySnapshot{
		HeadSHA:          "ready-head",
		WorkspacePresent: true,
		WorkspaceStatus:  "present",
		Health:           "ready",
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
		{name: "deliverable recovery with ready pull request", cause: deliverableRecoveryNeedsHumanReason + ": pushed branch ready has no recoverable pull request", owner: blockedRecoveryOwnerHuman, snapshot: baseSnapshot, wantMerging: true},
		{
			name:  "operator stop",
			cause: strandedUnpushedWorkReason,
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
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.WorkpadSignal.Status = workpad.StatusBlocked
				issue.WorkpadSignal.HumanAction = "provide production credentials"
			},
			snapshot: baseSnapshot,
		},
		{name: "human-only cause", cause: "credentials_missing", owner: blockedRecoveryOwnerHuman, snapshot: baseSnapshot},
		{
			name:  "draft pull request",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.Draft = true
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "failing pull request",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.CIStatus = "failure"
				issue.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Test", Status: "completed", Conclusion: "failure"}}
			},
			snapshot: baseSnapshot,
		},
		{
			name:  "head mismatched pull request",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			snapshot: runpkg.BlockedRecoverySnapshot{
				HeadSHA:          "workspace-head",
				WorkspacePresent: true,
				WorkspaceStatus:  "present",
				Health:           "ready",
			},
		},
		{
			name:  "dirty workspace",
			cause: strandedUnpushedWorkReason,
			owner: blockedRecoveryOwnerOrchestrator,
			snapshot: runpkg.BlockedRecoverySnapshot{
				HeadSHA:          "ready-head",
				WorkspacePresent: true,
				WorkspaceFiles:   1,
				WorkspaceStatus:  "present",
				Health:           "ready",
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
			cause: strandedUnpushedWorkReason,
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
