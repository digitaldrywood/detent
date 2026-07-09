package web

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestOnboardingIssueWorkpadStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		issue           telemetry.Issue
		wantWorkpad     bool
		wantStatusBlock bool
	}{
		{
			name: "valid status block",
			issue: telemetry.Issue{Comments: []telemetry.IssueComment{{
				Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```",
			}}},
			wantWorkpad:     true,
			wantStatusBlock: true,
		},
		{
			name: "workpad without status block",
			issue: telemetry.Issue{Comments: []telemetry.IssueComment{{
				Body: "## Codex Workpad\n\n### Status\nIn Progress",
			}}},
			wantWorkpad:     true,
			wantStatusBlock: false,
		},
		{
			name: "malformed status block",
			issue: telemetry.Issue{Comments: []telemetry.IssueComment{{
				Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: null\n```",
			}}},
			wantWorkpad:     true,
			wantStatusBlock: false,
		},
		{
			name: "status block outside workpad is ignored",
			issue: telemetry.Issue{Comments: []telemetry.IssueComment{{
				Body: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```",
			}}},
			wantWorkpad:     false,
			wantStatusBlock: false,
		},
		{
			name:            "no comments",
			issue:           telemetry.Issue{},
			wantWorkpad:     false,
			wantStatusBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotWorkpad, gotStatusBlock := onboardingIssueWorkpadStatus(tt.issue)
			if gotWorkpad != tt.wantWorkpad || gotStatusBlock != tt.wantStatusBlock {
				t.Fatalf("onboardingIssueWorkpadStatus() = %v, %v; want %v, %v", gotWorkpad, gotStatusBlock, tt.wantWorkpad, tt.wantStatusBlock)
			}
		})
	}
}
