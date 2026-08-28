package cli

import (
	"context"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestCheckDoctorOperationalCompletion(t *testing.T) {
	t.Parallel()

	authorizedIssue := connector.Issue{
		ID:          "issue-62",
		Identifier:  "digitaldrywood/leadpipe#62",
		Description: "```detent-completion\nschema: 1\ncompletion_kind: operational\n```",
	}
	tests := []struct {
		name       string
		issues     []connector.Issue
		prompt     string
		wantStatus doctorStatus
		wantDetail string
	}{
		{
			name:       "authorization without workflow contract warns",
			issues:     []connector.Issue{authorizedIssue},
			prompt:     "Complete work and open a pull request.",
			wantStatus: doctorWarn,
			wantDetail: "digitaldrywood/leadpipe#62",
		},
		{
			name:       "documented contract passes",
			issues:     []connector.Issue{authorizedIssue},
			prompt:     "Pre-authorize no-PR work with a detent-completion block, then declare completion_kind: operational with evidence.",
			wantStatus: doctorOK,
			wantDetail: "documents the operational completion contract",
		},
		{
			name:       "ordinary issues pass",
			issues:     []connector.Issue{{ID: "issue-code", Description: "Implement the code change."}},
			wantStatus: doctorOK,
			wantDetail: "no active or observed issue authorizes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validDoctorWorkflow(t.TempDir())
			cfg.Tracker.Kind = workflowconfig.TrackerGitHub
			check := checkDoctorOperationalCompletion(context.Background(), "leadpipe", cfg, tt.prompt, doctorDeps{
				autoPromoteConnector: func(workflowconfig.Config) (doctorAutoPromoteConnector, error) {
					return &fakeDoctorAutoPromoteConnector{issues: tt.issues}, nil
				},
			})
			if check.Status != tt.wantStatus || !strings.Contains(check.Detail, tt.wantDetail) {
				t.Fatalf("check = %#v, want status %s detail containing %q", check, tt.wantStatus, tt.wantDetail)
			}
		})
	}
}
