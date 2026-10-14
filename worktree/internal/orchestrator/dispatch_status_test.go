package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestProjectDispatchStatusScenarios(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 18, 0, 0, 0, time.UTC)
	threshold := 2 * time.Hour
	alpha := dispatchTestIssue("alpha", "Todo")
	beta := dispatchTestIssue("beta", "Todo")
	fingerprint := dispatchCandidateFingerprint(dispatchCandidateIdentities([]connector.Issue{alpha, beta}))

	tests := []struct {
		name          string
		previous      store.ProjectDispatchStatus
		candidates    []connector.Issue
		decisions     []dispatchPlanDecision
		outcomes      map[string]dispatchIssueOutcome
		wantCount     int
		wantEligible  int
		wantSkipped   int
		wantReason    string
		wantCode      string
		wantStall     bool
		wantClass     observability.Class
		wantAttention bool
	}{
		{
			name: "no candidates is healthy",
			previous: store.ProjectDispatchStatus{
				CandidateCount:       2,
				CandidateFingerprint: fingerprint,
				SkippedCount:         2,
				WaitReason:           dispatchSkipGitHubRESTCapacity,
				AllSkippedSince:      dispatchStatusTimePointer(now.Add(-3 * time.Hour)),
			},
		},
		{
			name:         "candidate selected is healthy",
			candidates:   []connector.Issue{alpha},
			decisions:    []dispatchPlanDecision{{Issue: alpha, Selected: true}},
			outcomes:     dispatchStatusOutcomes(alpha, dispatchIssueOutcome{dispatched: true}),
			wantCount:    1,
			wantEligible: 1,
		},
		{
			name: "uniform skips under threshold are healthy",
			previous: store.ProjectDispatchStatus{
				CandidateCount:       2,
				CandidateFingerprint: fingerprint,
				SkippedCount:         2,
				WaitReason:           dispatchSkipGitHubRESTCapacity,
				AllSkippedSince:      dispatchStatusTimePointer(now.Add(-30 * time.Minute)),
			},
			candidates: []connector.Issue{alpha, beta},
			decisions: []dispatchPlanDecision{
				{Issue: alpha, SkipReason: dispatchSkipGitHubRESTCapacity},
				{Issue: beta, SkipReason: dispatchSkipGitHubRESTCapacity},
			},
			wantCount:   2,
			wantSkipped: 2,
			wantReason:  dispatchSkipGitHubRESTCapacity,
			wantCode:    dispatchSkipGitHubRESTCapacity,
		},
		{
			name: "uniform skips over threshold are stalled",
			previous: store.ProjectDispatchStatus{
				CandidateCount:       2,
				CandidateFingerprint: fingerprint,
				SkippedCount:         2,
				WaitReason:           dispatchSkipGitHubRESTCapacity,
				AllSkippedSince:      dispatchStatusTimePointer(now.Add(-3 * time.Hour)),
			},
			candidates: []connector.Issue{alpha, beta},
			decisions: []dispatchPlanDecision{
				{Issue: alpha, SkipReason: dispatchSkipGitHubRESTCapacity},
				{Issue: beta, SkipReason: dispatchSkipGitHubRESTCapacity},
			},
			wantCount:   2,
			wantSkipped: 2,
			wantReason:  dispatchSkipGitHubRESTCapacity,
			wantCode:    dispatchSkipGitHubRESTCapacity,
			wantStall:   true,
			wantClass:   observability.ClassDiagnostic,
		},
		{
			name: "total authorization exclusion is a fault",
			previous: store.ProjectDispatchStatus{
				CandidateCount:       2,
				CandidateFingerprint: fingerprint,
				SkippedCount:         2,
				WaitReason:           schedulerDecisionWaitReason(dispatchSkipAuthorizationSelector),
				WaitReasonCode:       dispatchSkipAuthorizationSelector,
				AllSkippedSince:      dispatchStatusTimePointer(now.Add(-3 * time.Hour)),
			},
			candidates: []connector.Issue{alpha, beta},
			decisions: []dispatchPlanDecision{
				{Issue: alpha, SkipReason: dispatchSkipAuthorizationSelector, SkipDetail: "alpha does not match selector"},
				{Issue: beta, SkipReason: dispatchSkipAuthorizationSelector, SkipDetail: "beta does not match selector"},
			},
			wantCount:     2,
			wantSkipped:   2,
			wantReason:    schedulerDecisionWaitReason(dispatchSkipAuthorizationSelector),
			wantCode:      dispatchSkipAuthorizationSelector,
			wantStall:     true,
			wantClass:     observability.ClassFault,
			wantAttention: true,
		},
		{
			name:       "failure breaker counts only otherwise eligible candidates",
			candidates: []connector.Issue{alpha, beta},
			decisions: []dispatchPlanDecision{
				{Issue: alpha, SkipReason: dispatchSkipProjectFailureBreaker},
				{Issue: beta, SkipReason: dispatchSkipBlockedByDependency},
			},
			wantCount:    2,
			wantEligible: 1,
			wantSkipped:  2,
		},
		{
			name:        "capacity rejection is not an eligible candidate",
			candidates:  []connector.Issue{alpha},
			decisions:   []dispatchPlanDecision{{Issue: alpha, Selected: true}},
			outcomes:    dispatchStatusOutcomes(alpha, dispatchIssueOutcome{reason: dispatchIssueFailureBackendCapacityPaused}),
			wantCount:   1,
			wantSkipped: 1,
			wantReason:  schedulerDecisionWaitReason(dispatchIssueFailureBackendCapacityPaused),
			wantCode:    dispatchIssueFailureBackendCapacityPaused,
		},
		{
			name:        "pressure capacity rejection preserves duration detail",
			candidates:  []connector.Issue{alpha},
			decisions:   []dispatchPlanDecision{{Issue: alpha, Selected: true}},
			outcomes:    dispatchStatusOutcomes(alpha, dispatchIssueOutcome{reason: dispatchIssueFailureIOPressure, waitReason: "I/O pressure has limited admission to 1 concurrent agent for 5m0s"}),
			wantCount:   1,
			wantSkipped: 1,
			wantReason:  "I/O pressure has limited admission to 1 concurrent agent for 5m0s",
			wantCode:    dispatchIssueFailureIOPressure,
		},
		{
			name:        "failed selection does not advance dispatch",
			candidates:  []connector.Issue{alpha},
			decisions:   []dispatchPlanDecision{{Issue: alpha, Selected: true}},
			outcomes:    dispatchStatusOutcomes(alpha, dispatchIssueOutcome{reason: dispatchIssueFailureClaimFailed}),
			wantCount:   1,
			wantSkipped: 1,
			wantReason:  dispatchIssueFailureClaimFailed,
			wantCode:    dispatchIssueFailureClaimFailed,
		},
		{
			name:       "already running candidate is healthy idle",
			candidates: []connector.Issue{alpha},
			decisions:  []dispatchPlanDecision{{Issue: alpha, SkipReason: dispatchSkipAlreadyRunning}},
		},
		{
			name:       "already claimed candidate is healthy idle",
			candidates: []connector.Issue{alpha},
			decisions:  []dispatchPlanDecision{{Issue: alpha, SkipReason: dispatchSkipAlreadyClaimed}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status := projectDispatchStatusFromCycle(tt.previous, "detent", tt.candidates, tt.decisions, tt.outcomes, now)
			got := dispatchStatusSnapshot(status, threshold, now)
			if got.CandidateCount != tt.wantCount || got.EligibleCandidateCount != tt.wantEligible || got.SkippedCount != tt.wantSkipped || got.WaitReason != tt.wantReason || got.WaitReasonCode != tt.wantCode || got.Stalled != tt.wantStall || got.Class != tt.wantClass {
				t.Fatalf("dispatch status = %#v, want count=%d eligible=%d skipped=%d reason=%q code=%q stalled=%t class=%q", got, tt.wantCount, tt.wantEligible, tt.wantSkipped, tt.wantReason, tt.wantCode, tt.wantStall, tt.wantClass)
			}
			if got.NeedsHumanAttention != tt.wantAttention {
				t.Fatalf("NeedsHumanAttention = %t, want %t", got.NeedsHumanAttention, tt.wantAttention)
			}
			if tt.name == "candidate selected is healthy" && (got.LastSelectedAt == nil || !got.LastSelectedAt.Equal(now)) {
				t.Fatalf("LastSelectedAt = %#v, want %s", got.LastSelectedAt, now)
			}
		})
	}
}

func TestProjectDispatchStatusMixedProjects(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 18, 0, 0, 0, time.UTC)
	candidate := dispatchTestIssue("candidate", "Todo")
	fingerprint := dispatchCandidateFingerprint(dispatchCandidateIdentities([]connector.Issue{candidate}))
	projects := []struct {
		id       string
		previous store.ProjectDispatchStatus
		decision dispatchPlanDecision
		stalled  bool
	}{
		{
			id: "stalled",
			previous: store.ProjectDispatchStatus{
				CandidateFingerprint: fingerprint,
				WaitReason:           dispatchSkipGitHubRESTCapacity,
				AllSkippedSince:      dispatchStatusTimePointer(now.Add(-3 * time.Hour)),
			},
			decision: dispatchPlanDecision{Issue: candidate, SkipReason: dispatchSkipGitHubRESTCapacity},
			stalled:  true,
		},
		{
			id:       "moving",
			decision: dispatchPlanDecision{Issue: candidate, Selected: true},
		},
	}

	for _, project := range projects {
		outcomes := map[string]dispatchIssueOutcome(nil)
		if project.id == "moving" {
			outcomes = dispatchStatusOutcomes(candidate, dispatchIssueOutcome{dispatched: true})
		}
		status := projectDispatchStatusFromCycle(project.previous, project.id, []connector.Issue{candidate}, []dispatchPlanDecision{project.decision}, outcomes, now)
		got := dispatchStatusSnapshot(status, 2*time.Hour, now)
		if got.Stalled != project.stalled {
			t.Fatalf("project %s stalled = %t, want %t", project.id, got.Stalled, project.stalled)
		}
	}
}

func TestDispatchStatusSnapshotUsesLastSelectionForStarvationAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 18, 41, 0, 0, time.UTC)
	lastSelectedAt := now.Add(-48 * time.Hour)
	allSkippedSince := now.Add(-30 * time.Minute)
	candidate := dispatchTestIssue("todo", "Todo")
	candidate.StageUpdatedAt = dispatchStatusTimePointer(now.Add(-72 * time.Hour))
	previous := store.ProjectDispatchStatus{
		ProjectID:       "pyroapex",
		CandidateCount:  1,
		SkippedCount:    1,
		WaitReason:      "global capacity full",
		WaitReasonCode:  "global_capacity_full",
		AllSkippedSince: &allSkippedSince,
		LastSelectedAt:  &lastSelectedAt,
	}
	status := projectDispatchStatusFromCycle(
		previous,
		"pyroapex",
		[]connector.Issue{candidate},
		[]dispatchPlanDecision{{Issue: candidate, SkipReason: "reserved_for_higher_priority_state"}},
		nil,
		now,
	)

	got := dispatchStatusSnapshot(status, 2*time.Hour, now)
	if status.AllSkippedSince == nil || !status.AllSkippedSince.Equal(lastSelectedAt) {
		t.Fatalf("AllSkippedSince = %#v, want durable last selection %s", status.AllSkippedSince, lastSelectedAt)
	}
	if !got.Stalled {
		t.Fatalf("Stalled = false, want true after %s without a selection", now.Sub(lastSelectedAt))
	}
	if got.StallDurationSeconds != int64(48*time.Hour/time.Second) {
		t.Fatalf("StallDurationSeconds = %d, want %d", got.StallDurationSeconds, int64(48*time.Hour/time.Second))
	}
}

func TestDispatchStatusSnapshotDoesNotCountIdleTimeAsStarvation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 18, 41, 0, 0, time.UTC)
	lastSelectedAt := now.Add(-48 * time.Hour)
	candidate := dispatchTestIssue("todo", "Todo")
	status := projectDispatchStatusFromCycle(
		store.ProjectDispatchStatus{ProjectID: "pyroapex", LastSelectedAt: &lastSelectedAt},
		"pyroapex",
		[]connector.Issue{candidate},
		[]dispatchPlanDecision{{Issue: candidate, SkipReason: "reserved_for_higher_priority_state"}},
		nil,
		now,
	)

	got := dispatchStatusSnapshot(status, 2*time.Hour, now)
	if got.Stalled {
		t.Fatal("Stalled = true, want false for newly waiting work after an idle interval")
	}
	if status.AllSkippedSince == nil || !status.AllSkippedSince.Equal(now) {
		t.Fatalf("AllSkippedSince = %#v, want current refusal time %s", status.AllSkippedSince, now)
	}

	status = projectDispatchStatusFromCycle(
		status,
		"pyroapex",
		[]connector.Issue{candidate},
		[]dispatchPlanDecision{{Issue: candidate, SkipReason: "reserved_for_higher_priority_state"}},
		nil,
		now.Add(time.Minute),
	)
	if status.AllSkippedSince == nil || !status.AllSkippedSince.Equal(now) {
		t.Fatalf("second AllSkippedSince = %#v, want refusal window start %s", status.AllSkippedSince, now)
	}
}

func dispatchStatusOutcomes(issue connector.Issue, outcome dispatchIssueOutcome) map[string]dispatchIssueOutcome {
	return map[string]dispatchIssueOutcome{workflowIssueIdentityKey(issue): outcome}
}

func dispatchStatusTimePointer(value time.Time) *time.Time {
	return &value
}
