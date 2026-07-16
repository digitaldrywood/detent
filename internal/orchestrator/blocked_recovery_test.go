package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestRecoverBlockedIssuesSkipsPersistedStickyBlockedIssue(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"rework_limit", "token_ceiling_circuit_breaker"} {
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
			issue:            blockedRecoverySignatureIssue("issue-first-recovery", "same-head"),
			wantUpdates:      1,
			wantComments:     1,
			wantTransitioned: true,
		},
		{
			name:           "same signature skips and escalates",
			issue:          blockedRecoverySignatureIssue("issue-same-signature", "same-head"),
			persistedIssue: blockedRecoverySignatureIssue("issue-same-signature", "same-head"),
			wantComments:   1,
			wantExhausted:  true,
		},
		{
			name:             "changed head sha re-arms recovery",
			issue:            blockedRecoverySignatureIssue("issue-head-reset", "new-head"),
			persistedIssue:   blockedRecoverySignatureIssue("issue-head-reset", "old-head"),
			wantUpdates:      1,
			wantComments:     1,
			wantTransitioned: true,
		},
		{
			name:           "exhausted comment is deduped by signature",
			issue:          blockedRecoverySignatureIssue("issue-exhausted-dedupe", "same-head"),
			persistedIssue: blockedRecoverySignatureIssue("issue-exhausted-dedupe", "same-head"),
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
				assertWorkflowActionSignature(t, metrics, tt.issue, workflowActionBlockedRecoveryExhausted, blockedRecoverySignature(tt.issue, EvaluateBlockedRecovery(tt.issue)))
			}
		})
	}
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

func blockedRecoverySignatureIssue(id string, headSHA string) connector.Issue {
	issue := dependencyAutoUnblockIssue(id, "Blocked")
	prNumber := 418
	issue.PRNumber = &prNumber
	issue.PullRequest = &connector.PullRequest{
		Number:         prNumber,
		State:          "OPEN",
		URL:            "https://github.test/digitaldrywood/detent/pull/418",
		MergeableState: "behind",
		HeadSHA:        headSHA,
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

	decision := EvaluateBlockedRecovery(issue)
	signature := blockedRecoverySignature(issue, decision)
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

func assertBlockedRecoverySignatureMetadata(t *testing.T, metrics *autoPromoteWorkflowMetricsRecorder, issue connector.Issue) {
	t.Helper()

	signature := blockedRecoverySignature(issue, EvaluateBlockedRecovery(issue))
	assertWorkflowActionSignature(t, metrics, issue, workflowActionBlockedRecovery, signature)
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
