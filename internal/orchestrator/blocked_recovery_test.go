package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestEvaluateBlockedRecoveryUsesOnlyUnresolvedDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		ref            connector.BlockedRef
		terminalStates []string
		want           BlockedRecoveryReason
	}{
		{
			name: "closed tracker ref is resolved",
			ref: connector.BlockedRef{
				Identifier:   "digitaldrywood/detent#1900",
				State:        "In Progress",
				TrackerState: connector.BlockedRefTrackerStateClosed,
			},
			want: BlockedRecoveryReasonNoRecoverableSignal,
		},
		{
			name: "terminal workflow state is resolved",
			ref: connector.BlockedRef{
				Identifier: "digitaldrywood/detent#1901",
				State:      "Released",
			},
			terminalStates: []string{"Released"},
			want:           BlockedRecoveryReasonNoRecoverableSignal,
		},
		{
			name: "open tracker ref remains unresolved",
			ref: connector.BlockedRef{
				Identifier:   "digitaldrywood/detent#1902",
				State:        "Done",
				TrackerState: connector.BlockedRefTrackerStateOpen,
			},
			want: BlockedRecoveryReasonDependencyBlocker,
		},
		{
			name: "active workflow state remains unresolved",
			ref: connector.BlockedRef{
				Identifier: "digitaldrywood/detent#1903",
				State:      "In Progress",
			},
			want: BlockedRecoveryReasonDependencyBlocker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := connector.Issue{
				State:     blockedStatusState,
				BlockedBy: []connector.BlockedRef{tt.ref},
				PullRequest: &connector.PullRequest{
					State:          "open",
					MergeableState: "clean",
					CIStatus:       "success",
				},
			}
			terminalStates := tt.terminalStates
			if len(terminalStates) == 0 {
				terminalStates = []string{"Done", "Cancelled", "Canceled", "Closed"}
			}
			terminalStates = normalizedStates(terminalStates)
			got := evaluateBlockedRecovery(issue, normalizeBlockedRecoveryConfig(BlockedRecoveryConfig{
				Enabled:      true,
				SourceStates: []string{blockedStatusState},
			}), terminalStates)

			if got.Reason != tt.want {
				t.Fatalf("recovery reason = %q, want %q", got.Reason, tt.want)
			}
		})
	}
}

func TestSetBlockedStatusIssueStoresSpecificCurrentCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cause      string
		wantReason BlockedRecoveryReason
	}{
		{
			name:       "resolved dependency has no generic error",
			wantReason: BlockedRecoveryReasonMissingPullRequest,
		},
		{
			name:       "human attention cause stays actionable",
			cause:      "pull request delivery needs human attention",
			wantReason: BlockedRecoveryReasonHumanBlocker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg}
			state := newState(cfg)
			issue := connector.Issue{
				ID:            "issue-" + strings.ReplaceAll(tt.name, " ", "-"),
				State:         blockedStatusState,
				BlockerReason: tt.cause,
				BlockedBy: []connector.BlockedRef{{
					Identifier:   "digitaldrywood/detent#1900",
					State:        "Done",
					TrackerState: connector.BlockedRefTrackerStateClosed,
				}},
			}

			orch.setBlockedStatusIssue(&state, issue, time.Date(2026, 8, 18, 22, 30, 0, 0, time.UTC))

			blocked := state.Blocked[issue.ID]
			if blocked.Reason != tt.cause || blocked.RecoveryReason != string(tt.wantReason) {
				t.Fatalf("stored cause = %q/%q, want %q/%q", blocked.Reason, blocked.RecoveryReason, tt.cause, tt.wantReason)
			}
			if blocked.Reason == "blocked by project status" {
				t.Fatalf("stored generic project-status error: %#v", blocked)
			}
		})
	}
}

func TestRecoverBlockedIssuesSkipsPersistedStickyBlockedIssue(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"rework_limit", "token_ceiling_circuit_breaker", artifactGateConvergenceReason} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()

			issue := dependencyAutoUnblockIssue("issue-recovery-"+reason, "Blocked")
			prNumber := 418
			issue.PRNumber = &prNumber
			issue.PullRequest = &connector.PullRequest{
				Number:         prNumber,
				State:          "OPEN",
				URL:            "https://github.test/digitaldrywood/detent/pull/418",
				MergeableState: "behind",
				HeadSHA:        "abc123",
			}
			tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{issue}}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch.workflowMetrics = metrics
			if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
				ProjectID:    defaultWorkflowMetricsProjectID,
				IssueID:      issue.ID,
				Identifier:   issue.Identifier,
				IssueURL:     issue.URL,
				PhaseType:    store.WorkflowPhaseTypeLane,
				PhaseName:    "Blocked",
				Reason:       reason,
				Status:       "entered",
				StartedAt:    time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC),
				MetadataJSON: "{}",
			}); err != nil {
				t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
			}
			state := newState(orch.cfg)

			orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{issue}, time.Date(2026, 7, 8, 15, 1, 0, 0, time.UTC))

			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want no blocked recovery transition", tracker.updates)
			}
			if len(tracker.comments) != 0 {
				t.Fatalf("comments = %#v, want no blocked recovery comment", tracker.comments)
			}
		})
	}
}

func TestRecoverBlockedIssuesUsesPersistedSignatureGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		issue            connector.Issue
		persistedIssue   connector.Issue
		runTwice         bool
		wantUpdates      int
		wantComments     int
		wantExhausted    bool
		wantTransitioned bool
	}{
		{
			name:             "first recovery writes signature",
			issue:            blockedRecoverySignatureIssue("issue-first-recovery", "same-head", "same-diff", "same-base", "behind"),
			wantUpdates:      1,
			wantComments:     1,
			wantTransitioned: true,
		},
		{
			name:           "same signature skips and escalates",
			issue:          blockedRecoverySignatureIssue("issue-same-signature", "same-head", "same-diff", "same-base", "behind"),
			persistedIssue: blockedRecoverySignatureIssue("issue-same-signature", "same-head", "same-diff", "same-base", "behind"),
			wantComments:   1,
			wantExhausted:  true,
		},
		{
			name:           "changed head sha does not re-arm recovery",
			issue:          blockedRecoverySignatureIssue("issue-head-stable", "new-head", "same-diff", "same-base", "behind"),
			persistedIssue: blockedRecoverySignatureIssue("issue-head-stable", "old-head", "same-diff", "same-base", "behind"),
			wantComments:   1,
			wantExhausted:  true,
		},
		{
			name:             "changed diff fingerprint re-arms recovery",
			issue:            blockedRecoverySignatureIssue("issue-diff-reset", "new-head", "new-diff", "same-base", "behind"),
			persistedIssue:   blockedRecoverySignatureIssue("issue-diff-reset", "old-head", "old-diff", "same-base", "behind"),
			wantUpdates:      1,
			wantComments:     1,
			wantTransitioned: true,
		},
		{
			name:             "changed base oid re-arms recovery",
			issue:            blockedRecoverySignatureIssue("issue-base-reset", "same-head", "same-diff", "new-base", "behind"),
			persistedIssue:   blockedRecoverySignatureIssue("issue-base-reset", "same-head", "same-diff", "old-base", "behind"),
			wantUpdates:      1,
			wantComments:     1,
			wantTransitioned: true,
		},
		{
			name:           "changed recovery condition does not re-arm recovery",
			issue:          blockedRecoverySignatureIssue("issue-kind-stable", "same-head", "same-diff", "same-base", "behind"),
			persistedIssue: blockedRecoverySignatureIssue("issue-kind-stable", "same-head", "same-diff", "same-base", "dirty"),
			wantComments:   1,
			wantExhausted:  true,
		},
		{
			name:           "exhausted comment is deduped by signature",
			issue:          blockedRecoverySignatureIssue("issue-exhausted-dedupe", "same-head", "same-diff", "same-base", "behind"),
			persistedIssue: blockedRecoverySignatureIssue("issue-exhausted-dedupe", "same-head", "same-diff", "same-base", "behind"),
			runTwice:       true,
			wantComments:   1,
			wantExhausted:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{tt.issue}}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch.workflowMetrics = metrics
			if tt.persistedIssue.ID != "" {
				recordBlockedRecoverySignatureEvent(t, metrics, tt.persistedIssue, time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC))
			}
			state := newState(orch.cfg)
			now := time.Date(2026, 7, 8, 15, 1, 0, 0, time.UTC)
			recordBlockedRecoveryReasonEvent(t, metrics, tt.issue, now.Add(-time.Minute), blockedRecoveryReasonCodeForIssue(tt.issue))

			transitioned := orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{tt.issue}, now)
			if tt.runTwice {
				orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{tt.issue}, now.Add(time.Minute))
			}

			if got := len(tracker.updates); got != tt.wantUpdates {
				t.Fatalf("updates = %#v, want %d update(s)", tracker.updates, tt.wantUpdates)
			}
			if got := len(tracker.comments); got != tt.wantComments {
				t.Fatalf("comments = %#v, want %d comment(s)", tracker.comments, tt.wantComments)
			}
			_, didTransition := transitioned[tt.issue.ID]
			if didTransition != tt.wantTransitioned {
				t.Fatalf("transitioned[%q] = %v, want %v", tt.issue.ID, didTransition, tt.wantTransitioned)
			}
			if tt.wantUpdates > 0 {
				assertBlockedRecoverySignatureMetadata(t, metrics, tt.issue)
			}
			if tt.wantExhausted {
				if len(tracker.comments) == 0 || !strings.Contains(tracker.comments[0].body, "Blocked recovery already moved this issue") {
					t.Fatalf("comments = %#v, want exhausted escalation comment", tracker.comments)
				}
				assertWorkflowActionSignature(t, metrics, tt.issue, workflowActionBlockedRecoveryExhausted, mustBlockedRecoverySignature(t, tt.issue))
			}
		})
	}
}

func TestRecoverBlockedIssuesRequiresConfiguredBlockedReasonAndCurrentCondition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		reasonCode   string
		mutateIssue  func(*connector.Issue)
		mutateConfig func(*BlockedRecoveryConfig)
		wantTarget   string
	}{
		{
			name:       "merge conflict reason still conflicting",
			reasonCode: blockedRecoveryReasonMergeConflict,
			wantTarget: autoPromoteReworkState,
		},
		{
			name:       "stale base reason still behind",
			reasonCode: blockedRecoveryReasonStaleBase,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.MergeableState = "behind"
			},
			wantTarget: autoPromoteReworkState,
		},
		{
			name:       "missing ci reason still missing current head ci",
			reasonCode: blockedRecoveryReasonMissingCurrentHeadCI,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.MergeableState = "clean"
				issue.PullRequest.CIStatus = ""
				issue.PullRequest.CheckRunCount = 0
				issue.PullRequest.StatusContextCount = 0
			},
			wantTarget: autoPromoteReworkState,
		},
		{
			name:       "manual parking reason cannot authorize conflict repair",
			reasonCode: "tracker_state_observed",
		},
		{
			name:       "issue description cannot authorize conflict repair",
			reasonCode: "tracker_state_observed",
			mutateIssue: func(issue *connector.Issue) {
				issue.Description = "This issue explains how to rebase after a merge conflict."
			},
		},
		{
			name:       "reason must match current condition",
			reasonCode: blockedRecoveryReasonStaleBase,
		},
		{
			name:       "cleared condition stays blocked",
			reasonCode: blockedRecoveryReasonMergeConflict,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.MergeableState = "clean"
				issue.PullRequest.CIStatus = "success"
				issue.PullRequest.CheckRunCount = 1
			},
		},
		{
			name:       "disabled recovery stays blocked",
			reasonCode: blockedRecoveryReasonMergeConflict,
			mutateConfig: func(cfg *BlockedRecoveryConfig) {
				cfg.Enabled = false
			},
		},
		{
			name:       "reason outside configured allowlist stays blocked",
			reasonCode: blockedRecoveryReasonMergeConflict,
			mutateConfig: func(cfg *BlockedRecoveryConfig) {
				cfg.ReasonCodes = []string{blockedRecoveryReasonStaleBase}
			},
		},
		{
			name:       "missing diff fingerprint stays blocked",
			reasonCode: blockedRecoveryReasonMergeConflict,
			mutateIssue: func(issue *connector.Issue) {
				issue.PullRequest.DiffFingerprint = ""
			},
		},
		{
			name:       "configured target state is used",
			reasonCode: blockedRecoveryReasonMergeConflict,
			mutateConfig: func(cfg *BlockedRecoveryConfig) {
				cfg.TargetState = "Repair"
			},
			wantTarget: "Repair",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := blockedRecoverySignatureIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), "head", "diff", "base", "dirty")
			if tt.mutateIssue != nil {
				tt.mutateIssue(&issue)
			}
			tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{issue}}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
			if tt.mutateConfig != nil {
				tt.mutateConfig(&orch.cfg.BlockedRecovery)
			}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch.workflowMetrics = metrics
			now := time.Date(2026, 7, 8, 16, 0, 0, 0, time.UTC)
			recordBlockedRecoveryReasonEvent(t, metrics, issue, now.Add(-time.Minute), tt.reasonCode)
			state := newState(orch.cfg)

			transitioned := orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{issue}, now)

			if tt.wantTarget == "" {
				if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
					t.Fatalf("updates = %#v, comments = %#v, want no recovery", tracker.updates, tracker.comments)
				}
				if _, ok := transitioned[issue.ID]; ok {
					t.Fatalf("transitioned[%q] present, want no recovery", issue.ID)
				}
				return
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != tt.wantTarget {
				t.Fatalf("updates = %#v, want target %q", tracker.updates, tt.wantTarget)
			}
			if _, ok := transitioned[issue.ID]; !ok {
				t.Fatalf("transitioned[%q] missing", issue.ID)
			}
		})
	}
}

func TestObservedStructuredBlockedRecoveryReasonAuthorizesRecovery(t *testing.T) {
	t.Parallel()

	after := blockedRecoverySignatureIssue("issue-structured-park", "head", "diff", "base", "dirty")
	after.WorkpadSignal = &workpad.Signal{
		Source:     workpad.SourceStructured,
		Status:     workpad.StatusBlocked,
		ReasonCode: blockedRecoveryReasonMergeConflict,
	}
	before := cloneIssue(after)
	before.State = "In Progress"
	tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{after}}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	state := newState(orch.cfg)
	now := time.Date(2026, 7, 8, 16, 30, 0, 0, time.UTC)

	orch.commentObservedLaneTransition(context.Background(), before, after, now.Add(-time.Minute))
	transitioned := orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{after}, now)

	if len(tracker.updates) != 1 || tracker.updates[0].state != autoPromoteReworkState {
		t.Fatalf("updates = %#v, want one Rework transition", tracker.updates)
	}
	if _, ok := transitioned[after.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", after.ID)
	}
	timeline, err := metrics.IssueWorkflowTimeline(context.Background(), store.IssueIdentity{ProjectID: defaultWorkflowMetricsProjectID, IssueID: after.ID})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	reason, ok := latestEnteredLaneReason(timeline.Events, blockedStatusState)
	if !ok || reason != blockedRecoveryReasonMergeConflict {
		t.Fatalf("latest Blocked reason = %q, %v, want %q", reason, ok, blockedRecoveryReasonMergeConflict)
	}
	for _, event := range timeline.Events {
		if event.PhaseType != store.WorkflowPhaseTypeLane || event.PhaseName != blockedStatusState || event.Status != "entered" {
			continue
		}
		metadata, parsed := provenance.Parse(event.MetadataJSON)
		if !parsed || metadata.Provenance.Origin != provenance.OriginAgent || metadata.Provenance.Initiator != provenance.InitiatorDetentAgentSession {
			t.Fatalf("Blocked transition provenance = %#v, parsed %v, want Detent agent session", metadata.Provenance, parsed)
		}
		return
	}
	t.Fatal("Blocked entry event not found")
}

func TestBlockedRecoverySignatureUsesDiffFingerprintAndBaseOID(t *testing.T) {
	t.Parallel()

	issue := blockedRecoverySignatureIssue("issue-signature-shape", "agent-controlled-head", "stable-diff", "base-oid", "dirty")
	got := mustBlockedRecoverySignature(t, issue)
	want := "pr=418;fingerprint=stable-diff;base=base-oid"
	if got != want {
		t.Fatalf("blockedRecoverySignature() = %q, want %q", got, want)
	}
	for _, excluded := range []string{"kind=", "head="} {
		if strings.Contains(got, excluded) {
			t.Fatalf("blockedRecoverySignature() = %q, must not include %q", got, excluded)
		}
	}
}

func latestEnteredLaneReason(events []store.WorkflowPhaseEvent, lane string) (string, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.PhaseType == store.WorkflowPhaseTypeLane &&
			strings.EqualFold(event.PhaseName, lane) &&
			strings.EqualFold(event.Status, "entered") {
			return event.Reason, true
		}
	}
	return "", false
}

func TestRecoverBlockedIssuesReworkBreakerGuards(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 16, 21, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		reason        AutoPromoteReason
		mutate        func(*connector.Issue)
		manualReblock bool
		consumed      bool
		disabled      bool
		mutateConfig  func(*AutoPromoteConfig)
		prepare       func(*Orchestrator, connector.Issue)
		wantUnpark    bool
	}{
		{
			name:       "same head green and clean",
			reason:     AutoPromoteReasonCINotGreen,
			wantUnpark: true,
		},
		{
			name:       "merge conflicts cleared",
			reason:     AutoPromoteReasonMergeConflicts,
			wantUnpark: true,
		},
		{
			name:   "changed head",
			reason: AutoPromoteReasonCINotGreen,
			mutate: func(issue *connector.Issue) {
				issue.PullRequest.HeadSHA = "new-head"
			},
		},
		{
			name:   "ci still red",
			reason: AutoPromoteReasonCINotGreen,
			mutate: func(issue *connector.Issue) {
				issue.PullRequest.CIStatus = "failure"
			},
		},
		{
			name:   "required check still running",
			reason: AutoPromoteReasonCINotGreen,
			mutate: func(issue *connector.Issue) {
				issue.PullRequest.RunningChecks = []string{"Test"}
			},
		},
		{
			name:   "merge state is not clean",
			reason: AutoPromoteReasonMergeConflicts,
			mutate: func(issue *connector.Issue) {
				issue.PullRequest.MergeableState = "behind"
			},
		},
		{
			name:   "explicit human hold",
			reason: AutoPromoteReasonCINotGreen,
			mutate: func(issue *connector.Issue) {
				issue.BlockerReason = "hold until Friday"
			},
		},
		{
			name:   "new p1 finding",
			reason: AutoPromoteReasonCINotGreen,
			mutate: func(issue *connector.Issue) {
				issue.PullRequest.CodexReviewState = "P1"
			},
		},
		{
			name:   "auto promote optout label",
			reason: AutoPromoteReasonCINotGreen,
			mutate: func(issue *connector.Issue) {
				issue.Labels = append(issue.Labels, "requires-human-review")
			},
			mutateConfig: func(cfg *AutoPromoteConfig) {
				cfg.OptoutLabel = "requires-human-review"
			},
		},
		{
			name:   "required human approval missing",
			reason: AutoPromoteReasonCINotGreen,
			mutateConfig: func(cfg *AutoPromoteConfig) {
				cfg.Gate = gate.Config{
					Kind:          gate.KindHumanReview,
					ApprovalLabel: "human-approved",
				}
			},
		},
		{
			name:   "required automated review missing",
			reason: AutoPromoteReasonCINotGreen,
			mutateConfig: func(cfg *AutoPromoteConfig) {
				cfg.Gate = gate.Config{
					Kind:            gate.KindCommand,
					AutomatedReview: gate.AutomatedReviewRequired,
				}
			},
		},
		{
			name:   "validator result missing",
			reason: AutoPromoteReasonCINotGreen,
			mutateConfig: func(cfg *AutoPromoteConfig) {
				cfg.Gate = gate.Config{
					Kind:            gate.KindCommand,
					AutomatedReview: gate.AutomatedReviewOff,
					Validator:       gate.ValidatorConfig{Enabled: true},
				}
			},
		},
		{
			name:   "passing validator result",
			reason: AutoPromoteReasonCINotGreen,
			mutateConfig: func(cfg *AutoPromoteConfig) {
				cfg.Gate = gate.Config{
					Kind:            gate.KindCommand,
					AutomatedReview: gate.AutomatedReviewOff,
					Validator:       gate.ValidatorConfig{Enabled: true},
				}
			},
			prepare: func(orch *Orchestrator, issue connector.Issue) {
				identity := validatorStageIdentityForIssue(issue)
				orch.validatorResults = map[string]validatorStageResult{
					identity.Key: {
						Result: gate.ValidatorResult{
							Submitted: true,
							Verdict:   gate.ValidatorVerdictPass,
							Score:     1,
						},
					},
				}
			},
			wantUnpark: true,
		},
		{
			name:   "artifact status waiting",
			reason: AutoPromoteReasonCINotGreen,
			mutate: func(issue *connector.Issue) {
				issue.Fields[gate.DefaultArtifactStatusField] = "pending"
			},
			mutateConfig: func(cfg *AutoPromoteConfig) {
				cfg.Gate = gate.Config{Kind: gate.KindArtifact}
			},
		},
		{
			name:   "allowed issue label missing",
			reason: AutoPromoteReasonCINotGreen,
			mutateConfig: func(cfg *AutoPromoteConfig) {
				cfg.AllowedIssueLabels = []string{"approved-for-merge"}
			},
		},
		{
			name:          "manual reblock supersedes breaker park",
			reason:        AutoPromoteReasonCINotGreen,
			manualReblock: true,
		},
		{
			name:     "auto promotion disabled",
			reason:   AutoPromoteReasonCINotGreen,
			disabled: true,
		},
		{
			name:       "automated review finding is human-gated",
			reason:     AutoPromoteReasonP1Findings,
			wantUnpark: false,
		},
		{
			name:       "same head auto-unpark already consumed",
			reason:     AutoPromoteReasonCINotGreen,
			consumed:   true,
			wantUnpark: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := reworkBreakerRecoveryIssue("issue-" + strings.ReplaceAll(tt.name, " ", "-"))
			parkedIssue := cloneIssue(issue)
			if tt.mutate != nil {
				tt.mutate(&issue)
			}
			tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{issue}}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
			orch.cfg.AutoPromote.Enabled = !tt.disabled
			orch.cfg.AutoPromote.Gate = gate.Config{
				Kind:            gate.KindCommand,
				AutomatedReview: gate.AutomatedReviewOff,
			}
			if tt.mutateConfig != nil {
				tt.mutateConfig(&orch.cfg.AutoPromote)
			}
			if tt.prepare != nil {
				tt.prepare(orch, issue)
			}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch.workflowMetrics = metrics
			recordReworkBreakerPark(t, metrics, parkedIssue, base.Add(-time.Hour), tt.reason)
			if tt.consumed {
				recordReworkBreakerAutoUnpark(t, metrics, parkedIssue, base.Add(-30*time.Minute))
				recordReworkBreakerPark(t, metrics, parkedIssue, base.Add(-10*time.Minute), tt.reason)
			}
			if tt.manualReblock {
				recordReworkBreakerManualBlock(t, metrics, parkedIssue, base.Add(-time.Minute))
			}
			state := newState(orch.cfg)

			transitioned := orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{issue}, base)

			if got := len(tracker.updates); got != boolInt(tt.wantUnpark) {
				t.Fatalf("updates = %#v, want unpark %v", tracker.updates, tt.wantUnpark)
			}
			_, didTransition := transitioned[issue.ID]
			if didTransition != tt.wantUnpark {
				t.Fatalf("transitioned[%q] = %v, want %v", issue.ID, didTransition, tt.wantUnpark)
			}
			if tt.wantUnpark {
				if tracker.updates[0].state != autoPromoteMergingState {
					t.Fatalf("update state = %q, want %q", tracker.updates[0].state, autoPromoteMergingState)
				}
				if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "only automatic unpark permitted") {
					t.Fatalf("comments = %#v, want one-shot auto-unpark audit", tracker.comments)
				}
				assertWorkflowActionSignature(t, metrics, issue, workflowActionReworkBreakerAutoUnpark, "pr=1585;head=parked-head")
			}
		})
	}
}

func TestRecoverBlockedIssuesDoesNotRecordRejectedReworkBreakerUnpark(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 21, 0, 0, 0, time.UTC)
	issue := reworkBreakerRecoveryIssue("issue-rejected-unpark")
	tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{issue}}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	orch.connector = reworkBreakerRejectingConnector{dependencyAutoUnblockConnector: tracker}
	orch.cfg.AutoPromote.Enabled = true
	orch.cfg.AutoPromote.Gate = gate.Config{
		Kind:            gate.KindCommand,
		AutomatedReview: gate.AutomatedReviewOff,
	}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	recordReworkBreakerPark(t, metrics, issue, now.Add(-time.Hour), AutoPromoteReasonCINotGreen)
	state := newState(orch.cfg)

	transitioned := orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{issue}, now)

	if got := len(tracker.updates); got != 1 {
		t.Fatalf("updates = %#v, want one rejected transition attempt", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want no success audit after rejected transition", tracker.comments)
	}
	if _, ok := transitioned[issue.ID]; ok {
		t.Fatalf("transitioned[%q] present after rejected transition", issue.ID)
	}
	if reworkBreakerAutoUnparkConsumed(metricsTimeline(t, metrics, issue), "pr=1585;head=parked-head") {
		t.Fatal("rejected transition consumed the one-shot auto-unpark")
	}
}

type reworkBreakerRejectingConnector struct {
	*dependencyAutoUnblockConnector
}

func (c reworkBreakerRejectingConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, dependencyAutoUnblockUpdate{issueID: issueID, state: state})
	return connector.ErrStateUpdateBlocked
}

func metricsTimeline(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
) store.WorkflowTimeline {
	t.Helper()

	timeline, err := metrics.IssueWorkflowTimeline(context.Background(), store.IssueIdentity{
		ProjectID:  defaultWorkflowMetricsProjectID,
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
	})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	return timeline
}

func reworkBreakerRecoveryIssue(id string) connector.Issue {
	issue := dependencyAutoUnblockIssue(id, blockedStatusState)
	prNumber := 1585
	issue.PRNumber = &prNumber
	issue.PullRequest = &connector.PullRequest{
		Number:         prNumber,
		State:          "OPEN",
		URL:            "https://github.test/digitaldrywood/pyroapex/pull/1585",
		HeadSHA:        "parked-head",
		MergeableState: "clean",
		CIStatus:       "success",
		CheckRunCount:  4,
	}
	return issue
}

func recordReworkBreakerPark(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
	reason AutoPromoteReason,
) {
	t.Helper()

	metadata := workflowLaneMetadata{
		ReworkBreaker: &workflowLaneReworkBreakerMetadata{Reason: string(reason)},
	}
	for _, event := range []store.WorkflowPhaseEvent{
		{
			ProjectID:    defaultWorkflowMetricsProjectID,
			IssueID:      issue.ID,
			Identifier:   issue.Identifier,
			IssueURL:     issue.URL,
			PhaseType:    store.WorkflowPhaseTypeLane,
			PhaseName:    autoPromoteReworkState,
			Reason:       string(reason),
			Status:       "entered",
			StartedAt:    at.Add(-time.Minute),
			MetadataJSON: workflowLaneMetadataJSON(issue, workflowLaneMetadata{}),
		},
		{
			ProjectID:    defaultWorkflowMetricsProjectID,
			IssueID:      issue.ID,
			Identifier:   issue.Identifier,
			IssueURL:     issue.URL,
			PhaseType:    store.WorkflowPhaseTypeLane,
			PhaseName:    blockedStatusState,
			Reason:       "rework_limit",
			Status:       "entered",
			StartedAt:    at,
			MetadataJSON: workflowLaneMetadataJSON(issue, metadata),
		},
	} {
		if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), event); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
		}
	}
}

func recordReworkBreakerAutoUnpark(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
) {
	t.Helper()

	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionReworkBreakerAutoUnpark, "pr=1585;head=parked-head")
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    autoPromoteMergingState,
		Reason:       "rework_breaker_auto_unpark",
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: workflowLaneMetadataJSON(issue, metadata),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}

func recordReworkBreakerManualBlock(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
) {
	t.Helper()

	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    blockedStatusState,
		Reason:       "tracker_state_observed",
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: workflowLaneMetadataJSON(issue, workflowLaneMetadata{}),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func blockedRecoverySignatureIssue(
	id string,
	headSHA string,
	diffFingerprint string,
	baseSHA string,
	mergeableState string,
) connector.Issue {
	issue := dependencyAutoUnblockIssue(id, "Blocked")
	prNumber := 418
	issue.PRNumber = &prNumber
	issue.PullRequest = &connector.PullRequest{
		Number:          prNumber,
		State:           "OPEN",
		URL:             "https://github.test/digitaldrywood/detent/pull/418",
		MergeableState:  mergeableState,
		HeadSHA:         headSHA,
		BaseSHA:         baseSHA,
		DiffFingerprint: diffFingerprint,
	}
	return issue
}

func recordBlockedRecoverySignatureEvent(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
) {
	t.Helper()

	signature := mustBlockedRecoverySignature(t, issue)
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionBlockedRecovery, signature)
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    autoPromoteReworkState,
		Reason:       "blocked_recovery",
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: workflowLaneMetadataJSON(issue, metadata),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}

func recordBlockedRecoveryReasonEvent(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
	reasonCode string,
) {
	t.Helper()

	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    issue.State,
		Reason:       reasonCode,
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: workflowLaneMetadataJSON(issue, workflowLaneMetadata{}),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}

func blockedRecoveryReasonCodeForIssue(issue connector.Issue) string {
	if issue.PullRequest != nil && autoPromoteMergeConflicts(issue.PullRequest.MergeableState) {
		return blockedRecoveryReasonMergeConflict
	}
	return blockedRecoveryReasonStaleBase
}

func mustBlockedRecoverySignature(t *testing.T, issue connector.Issue) string {
	t.Helper()

	signature, ok := blockedRecoverySignature(issue)
	if !ok {
		t.Fatalf("blockedRecoverySignature() unavailable for %#v", issue.PullRequest)
	}
	return signature
}

func assertBlockedRecoverySignatureMetadata(t *testing.T, metrics *autoPromoteWorkflowMetricsRecorder, issue connector.Issue) {
	t.Helper()

	assertWorkflowActionSignature(t, metrics, issue, workflowActionBlockedRecovery, mustBlockedRecoverySignature(t, issue))
}

func assertWorkflowActionSignature(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	action string,
	signature string,
) {
	t.Helper()

	for _, event := range metrics.snapshot() {
		if event.IssueID != issue.ID {
			continue
		}
		metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if !ok {
			continue
		}
		if workflowLaneMetadataHasActionSignature(metadata, action, signature) {
			return
		}
	}
	t.Fatalf("missing workflow action signature action=%q signature=%q in events %#v", action, signature, metrics.snapshot())
}
