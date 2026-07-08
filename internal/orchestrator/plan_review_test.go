package orchestrator

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestPlanReviewContainsReviewSeverityUsesExplicitBadges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		severity string
		want     bool
	}{
		{
			name:     "bracketed p1 anywhere",
			body:     "Automated review found [P1] missing validation.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "line anchored colon",
			body:     "P1: Missing rollback path.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "list prefixed p2 badge",
			body:     "- P2 BADGE naming concern.",
			severity: "P2",
			want:     true,
		},
		{
			name:     "narrative p1 negative",
			body:     "No P1 issues found — approved.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "mid sentence p1 fix",
			body:     "The P1 fix from last week is already present.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "p2 finding prose no longer matches",
			body:     "P2 finding count is zero.",
			severity: "P2",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := containsReviewSeverity(tt.body, tt.severity)
			if got != tt.want {
				t.Fatalf("containsReviewSeverity(%q, %q) = %t, want %t", tt.body, tt.severity, got, tt.want)
			}
		})
	}
}

func TestPlanReviewDecisionLogsReviewStateDisagreement(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	orch := &Orchestrator{
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	issue := connector.Issue{
		ID:         "issue-1072",
		Identifier: "digitaldrywood/detent#1072",
		PullRequest: &connector.PullRequest{
			CodexReviewAPIState:     "APPROVED",
			CodexReviewBodySeverity: "P1",
		},
	}

	orch.logPlanReviewDecision(issue, gate.Decision{
		Action: gate.ActionRework,
		Reason: gate.ReasonP1Findings,
	}, "Rework")

	for _, fragment := range []string{"review_api_state=APPROVED", "review_body_severity=P1"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}
