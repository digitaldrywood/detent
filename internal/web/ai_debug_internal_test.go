package web

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestAIDebugAttemptsExtractRecordedProgressEvidence(t *testing.T) {
	t.Parallel()

	attempts := aiDebugAttempts([]store.WorkAttempt{{
		ID:                 11,
		WorkerMetadataJSON: `{"completion_progress":{"workspace_diffstat":{"files_changed":2,"added_lines":14,"removed_lines":3,"unpushed_commits":4},"current_head_sha":"head-new"},"pr_head_sha":"head-old","work_product_pushed":true}`,
		MetricsJSON:        `{}`,
	}})
	if len(attempts) != 1 {
		t.Fatalf("aiDebugAttempts() len = %d, want 1", len(attempts))
	}
	if got := attempts[0].WorkspaceDiffstat["files_changed"]; got != float64(2) {
		t.Fatalf("workspace diffstat files_changed = %#v, want 2", got)
	}
	if attempts[0].UnpushedCommitCount == nil || *attempts[0].UnpushedCommitCount != 4 {
		t.Fatalf("unpushed commit count = %v, want 4", attempts[0].UnpushedCommitCount)
	}
	if attempts[0].WorkProductPushed == nil || !*attempts[0].WorkProductPushed {
		t.Fatalf("work product pushed = %v, want true", attempts[0].WorkProductPushed)
	}
}

func TestAIDebugProjectionSummariesUseCanonicalValues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "fleet slots sum pool usage",
			run: func(t *testing.T) {
				evidence := aiDebugFleetEvidence(telemetry.Snapshot{AgentPools: []telemetry.AgentPool{{Name: "primary", Used: 2}, {Name: "burst", Used: 3}}, Running: make([]telemetry.Running, 9)})
				if evidence.RunningCount != 5 {
					t.Fatalf("global slots in use = %d, want 5", evidence.RunningCount)
				}
			},
		},
		{
			name: "scheduler decisions count wait reasons",
			run: func(t *testing.T) {
				counts := aiDebugSchedulerWaitCounts([]store.SchedulerDecision{{WaitReason: "global_capacity_full"}, {WaitReason: "global_capacity_full"}, {}})
				if counts["global_capacity_full"] != 2 || counts["none recorded"] != 1 {
					t.Fatalf("wait counts = %#v", counts)
				}
			},
		},
		{
			name: "project gate uses latest attempt",
			run: func(t *testing.T) {
				snapshot := telemetry.Snapshot{WorkAttempts: []telemetry.WorkAttempt{
					{AttemptID: 1, ProjectID: "detent", StartedAt: now.Add(-time.Hour), CIState: "failure"},
					{AttemptID: 2, ProjectID: "detent", StartedAt: now, CIState: "success"},
				}}
				if got := aiDebugProjectGateResult(snapshot, "detent"); got != "success" {
					t.Fatalf("project gate result = %q, want success", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestAIDebugParkEvidenceUsesCurrentScopedBreakerState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		snapshot      telemetry.Snapshot
		issue         telemetry.Issue
		wantParked    bool
		wantKind      string
		wantParkCount int64
	}{
		{
			name:          "historical parks do not imply current park",
			issue:         telemetry.Issue{ProjectID: "alpha", ID: "1", ParkSummary: telemetry.ParkSummary{ParkCount: 3}},
			wantParkCount: 3,
		},
		{
			name: "empty aliases do not match",
			snapshot: telemetry.Snapshot{DispatchLoops: []telemetry.DispatchLoop{{
				ProjectID:  "alpha",
				Identifier: "other#1",
				Tripped:    true,
			}}},
			issue: telemetry.Issue{ProjectID: "alpha"},
		},
		{
			name: "failure breaker from another project is ignored",
			snapshot: telemetry.Snapshot{FailureBreakers: []telemetry.FailureBreaker{{
				ProjectID: "beta",
				Items:     []telemetry.FailureBreakerItem{{IssueID: "1", Parked: true}},
			}}},
			issue: telemetry.Issue{ProjectID: "alpha", ID: "1"},
		},
		{
			name: "matching dispatch loop reports current park",
			snapshot: telemetry.Snapshot{DispatchLoops: []telemetry.DispatchLoop{{
				ProjectID: "alpha",
				IssueURL:  "https://example.test/issues/1",
				Tripped:   true,
			}}},
			issue:      telemetry.Issue{ProjectID: "alpha", URL: "https://example.test/issues/1"},
			wantParked: true,
			wantKind:   "dispatch_loop",
		},
		{
			name: "matching failure breaker reports current park",
			snapshot: telemetry.Snapshot{FailureBreakers: []telemetry.FailureBreaker{{
				ProjectID: "alpha",
				Items:     []telemetry.FailureBreakerItem{{Identifier: "alpha#1", Parked: true}},
			}}},
			issue:      telemetry.Issue{ProjectID: "alpha", Identifier: "alpha#1"},
			wantParked: true,
			wantKind:   "failure_breaker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evidence := aiDebugParkEvidence(tt.snapshot, tt.issue, 3)
			if evidence.Parked != tt.wantParked {
				t.Errorf("Parked = %t, want %t", evidence.Parked, tt.wantParked)
			}
			if evidence.BreakerKind != tt.wantKind {
				t.Errorf("BreakerKind = %q, want %q", evidence.BreakerKind, tt.wantKind)
			}
			if evidence.ParkCount != tt.wantParkCount {
				t.Errorf("ParkCount = %d, want %d", evidence.ParkCount, tt.wantParkCount)
			}
		})
	}
}

func TestAIDebugLaneOriginUsesPromptVocabulary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		origin provenance.Origin
		want   string
	}{
		{origin: provenance.OriginDetent, want: "detent"},
		{origin: provenance.OriginRoutine, want: "detent"},
		{origin: provenance.OriginAgent, want: "agent"},
		{origin: provenance.OriginHuman, want: "human"},
		{origin: provenance.OriginAutomation, want: "indeterminate"},
		{origin: provenance.OriginUnknown, want: "indeterminate"},
	}
	for _, tt := range tests {
		if got := aiDebugLaneOrigin(tt.origin); got != tt.want {
			t.Errorf("aiDebugLaneOrigin(%q) = %q, want %q", tt.origin, got, tt.want)
		}
	}
}

func TestAIDebugDeliveryPrefersCurrentHeadEvidence(t *testing.T) {
	t.Parallel()

	evidence := aiDebugDelivery(telemetry.Issue{PullRequest: &telemetry.PullRequest{CIStatus: "pending", CheckRunCount: 4, StatusContextCount: 2}}, []store.WorkAttempt{
		{WorkerMetadataJSON: `{"completion_progress":{"current_head_sha":"head-new"},"pr_head_sha":"head-old"}`},
		{WorkerMetadataJSON: `{"completion_progress":{"current_head_sha":"head-prior"}}`},
	})
	if evidence.HeadMovedAcrossAttempts != "true" {
		t.Fatalf("head moved = %q, want true", evidence.HeadMovedAcrossAttempts)
	}
	if evidence.CIStatus != "pending" || evidence.CheckRunCount != 4 || evidence.StatusContextCount != 2 {
		t.Fatalf("CI summary = %#v", evidence)
	}
}
