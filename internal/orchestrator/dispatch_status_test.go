package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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
		name        string
		previous    store.ProjectDispatchStatus
		candidates  []connector.Issue
		decisions   []dispatchPlanDecision
		outcomes    map[string]dispatchIssueOutcome
		wantCount   int
		wantSkipped int
		wantReason  string
		wantStall   bool
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
			name:       "candidate selected is healthy",
			candidates: []connector.Issue{alpha},
			decisions:  []dispatchPlanDecision{{Issue: alpha, Selected: true}},
			outcomes:   dispatchStatusOutcomes(alpha, dispatchIssueOutcome{dispatched: true}),
			wantCount:  1,
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
			wantStall:   true,
		},
		{
			name:        "failed selection does not advance dispatch",
			candidates:  []connector.Issue{alpha},
			decisions:   []dispatchPlanDecision{{Issue: alpha, Selected: true}},
			outcomes:    dispatchStatusOutcomes(alpha, dispatchIssueOutcome{reason: dispatchIssueFailureClaimFailed}),
			wantCount:   1,
			wantSkipped: 1,
			wantReason:  dispatchIssueFailureClaimFailed,
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
			if got.CandidateCount != tt.wantCount || got.SkippedCount != tt.wantSkipped || got.WaitReason != tt.wantReason || got.Stalled != tt.wantStall {
				t.Fatalf("dispatch status = %#v, want count=%d skipped=%d reason=%q stalled=%t", got, tt.wantCount, tt.wantSkipped, tt.wantReason, tt.wantStall)
			}
			if got.NeedsHumanAttention != tt.wantStall {
				t.Fatalf("NeedsHumanAttention = %t, want %t", got.NeedsHumanAttention, tt.wantStall)
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

func dispatchStatusOutcomes(issue connector.Issue, outcome dispatchIssueOutcome) map[string]dispatchIssueOutcome {
	return map[string]dispatchIssueOutcome{workflowIssueIdentityKey(issue): outcome}
}

func dispatchStatusTimePointer(value time.Time) *time.Time {
	return &value
}
