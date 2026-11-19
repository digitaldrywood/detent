package operatorcommands

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
)

func TestSlackMessagePrivacyAndLinks(t *testing.T) {
	t.Parallel()
	const webhook = "https://hooks.slack.com/services/T123/B456/SLACK_SECRET_SENTINEL"
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct{ kind, id, path string }{
		{"work_item", "wi_test", "/example-org/example-project/issues/1?tab=diagnostics"}, {"runner", "runner_test", "/fleet"}, {"project", "prj_test", "/diagnostics"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			finding := healthsignals.Finding{Signal: "runner_heartbeat_gap", Subject: healthsignals.Subject{Kind: test.kind, ID: test.id}, OpenedAt: now.Add(-time.Minute), Summary: "Failed with xoxb-123-SECRET sk-proj-SECRET ghp_SECRET github_pat_SECRET glpat-SECRET AIzaSECRET AKIA1234567890ABCDEF eyJabc.def.ghi " + webhook, NextAction: "token=SECRET Bearer SECRET\n<@here> checks `raw log`."}
			message, err := RenderSlackFinding("https://cloud.detent.test", "/example-org", finding, "opened", now, func() string {
				if test.kind == "work_item" {
					return "/example-org/example-project/issues/1"
				}
				return ""
			}())
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"SECRET", "<@here>", "`raw log`", "xoxb-", "sk-proj-", "ghp_", "github_pat_", "Bearer", "eyJabc"} {
				if strings.Contains(message, secret) {
					t.Fatalf("message leaked %s: %s", secret, message)
				}
			}
			for _, part := range []string{"*runner_heartbeat_gap*", "https://cloud.detent.test" + func() string {
				if test.kind == "work_item" {
					return test.path
				}
				return "/example-org" + test.path
			}(), "Next action:", "Age: 1m0s"} {
				if !strings.Contains(message, part) {
					t.Fatalf("missing %s: %s", part, message)
				}
			}
		})
	}
}
