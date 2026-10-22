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
		name         string
		cause        string
		blockedState string
		trackerState string
		wantCause    string
		wantReason   BlockedRecoveryReason
	}{
		{
			name:         "resolved dependency records missing cause",
			blockedState: "Done",
			trackerState: connector.BlockedRefTrackerStateClosed,
			wantCause:    "blocked outside Detent; no cause recorded by the tracker",
			wantReason:   BlockedRecoveryReasonMissingPullRequest,
		},
		{
			name:         "unresolved dependency records structured cause",
			blockedState: "In Progress",
			trackerState: connector.BlockedRefTrackerStateOpen,
			wantCause:    blockedReasonDependency,
			wantReason:   BlockedRecoveryReasonDependencyBlocker,
		},
		{
			name:         "human wording alone is diagnostic",
			cause:        "pull request delivery needs human attention",
			blockedState: "Done",
			trackerState: connector.BlockedRefTrackerStateClosed,
			wantCause:    "pull request delivery needs human attention",
			wantReason:   BlockedRecoveryReasonMissingPullRequest,
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
					State:        tt.blockedState,
					TrackerState: tt.trackerState,
				}},
			}

			orch.setBlockedStatusIssue(t.Context(), &state, issue, time.Date(2026, 8, 18, 22, 30, 0, 0, time.UTC))

			blocked := state.Blocked[issue.ID]
			if blocked.Reason != tt.wantCause || blocked.RecoveryReason != string(tt.wantReason) {
				t.Fatalf("stored cause = %q/%q, want %q/%q", blocked.Reason, blocked.RecoveryReason, tt.wantCause, tt.wantReason)
			}
			if blocked.Reason == "blocked by project status" {
				t.Fatalf("stored generic project-status error: %#v", blocked)
			}
		})
	}
}

func TestSetBlockedStatusIssueReplacesUnrecordedCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		refreshed    connector.Issue
		wantCause    string
		wantRecovery BlockedRecoveryReason
	}{
		{
			name: "explicit tracker cause",
			refreshed: connector.Issue{
				BlockerReason: "waiting for human approval",
			},
			wantCause:    "waiting for human approval",
			wantRecovery: BlockedRecoveryReasonMissingPullRequest,
		},
		{
			name: "structured dependency cause",
			refreshed: connector.Issue{
				BlockedBy: []connector.BlockedRef{{
					Identifier:   "digitaldrywood/detent#1902",
					State:        "In Progress",
					TrackerState: connector.BlockedRefTrackerStateOpen,
				}},
			},
			wantCause:    blockedReasonDependency,
			wantRecovery: BlockedRecoveryReasonDependencyBlocker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}})
			orch := &Orchestrator{cfg: cfg}
			state := newState(cfg)
			now := time.Date(2026, 8, 24, 16, 45, 0, 0, time.UTC)
			issue := connector.Issue{ID: "issue-refresh", State: blockedStatusState}
			orch.setBlockedStatusIssue(t.Context(), &state, issue, now)

			refreshed := tt.refreshed
			refreshed.ID = issue.ID
			refreshed.State = blockedStatusState
			orch.setBlockedStatusIssue(t.Context(), &state, refreshed, now.Add(time.Minute))

			blocked := state.Blocked[issue.ID]
			if blocked.Reason != tt.wantCause || blocked.RecoveryReason != string(tt.wantRecovery) {
				t.Fatalf("stored cause = %q/%q, want %q/%q", blocked.Reason, blocked.RecoveryReason, tt.wantCause, tt.wantRecovery)
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

type blockedRecoveryTimelineRecorder struct {
	*autoPromoteWorkflowMetricsRecorder
	reads int
}

func (r *blockedRecoveryTimelineRecorder) IssueWorkflowTimeline(ctx context.Context, identity store.IssueIdentity) (store.WorkflowTimeline, error) {
	r.reads++
	return r.autoPromoteWorkflowMetricsRecorder.IssueWorkflowTimeline(ctx, identity)
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
}

// Validator acceptance refreshes task inputs by identity before consuming a
// cached verdict. This fixture exposes its current task without changing the
// dependency connector used by other tests.
type reworkBreakerTaskConnector struct {
	*dependencyAutoUnblockConnector
}

func (c reworkBreakerTaskConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	var out []connector.Issue
	for _, current := range c.stateIssues {
		for _, id := range ids {
			if current.ID == id {
				out = append(out, cloneIssue(current))
				break
			}
		}
	}
	return out, nil
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
	if issue.PullRequest != nil && connector.PullRequestConflicts(issue.PullRequest.MergeableState) {
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
