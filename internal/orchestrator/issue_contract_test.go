package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestEnforceIssueContract(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	contract := config.DefaultIssueContract()
	orch := &Orchestrator{
		cfg: Config{
			IssueContract:          &contract,
			IssueContractRolloutAt: now.Add(-time.Hour),
			AdmissionTargetState:   "Todo",
		},
	}
	cases := []struct {
		name        string
		description string
		wantAllowed bool
	}{
		{
			name:        "incomplete Todo issue is allowed even with configured contract",
			description: "Fix endpoint without contract headings",
			wantAllowed: true,
		},
		{
			name:        "explicit no-dispatch marker refuses",
			description: "Hold this work\n\n<!-- detent:no-dispatch -->",
			wantAllowed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			created := now
			issue := connector.Issue{
				ID:          "1",
				Identifier:  "prj#1",
				State:       "Todo",
				Description: tc.description,
				CreatedAt:   &created,
			}
			allowed, err := orch.enforceIssueContract(context.Background(), nil, issue, "Todo", now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allowed != tc.wantAllowed {
				t.Fatalf("allowed=%t, want %t", allowed, tc.wantAllowed)
			}
		})
	}
}
