package connector

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func TestSecurityAuditSnapshot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                 string
		native               bool
		base, head           string
		limit                int
		wantError, oversized bool
	}{
		{name: "github exact diff", base: "base", head: "head", limit: 100},
		{name: "native exact diff", native: true, base: "base", head: "head", limit: 100},
		{name: "head changed", base: "base", head: "other", limit: 100, wantError: true},
		{name: "base changed", base: "other", head: "head", limit: 100, wantError: true},
		{name: "oversized diff", base: "base", head: "head", limit: 4, oversized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := Issue{ID: "wi_example", Identifier: "prj_example#1", Title: "Review change", Description: "Issue context"}
			number := 1
			if !test.native {
				issue.PullRequest = &PullRequest{Number: number, BaseSHA: "base", HeadSHA: "head"}
			} else {
				number = 0
			}
			diff := ValidationDiff{Repository: "example-org/example-project", PRNumber: number, BaseSHA: test.base, HeadSHA: test.head, Patch: "textual diff"}
			snapshot, err := SecurityAuditSnapshot("prj_example", issue, diff)
			if (err != nil) != test.wantError {
				t.Fatalf("snapshot error = %v", err)
			}
			if test.wantError {
				return
			}
			if snapshot.BaseSHA != test.base || snapshot.HeadSHA != test.head || snapshot.IssueID != issue.ID || snapshot.IssueDescription != issue.Description || snapshot.DiffTruncated || snapshot.Diff != diff.Patch || !snapshot.DiffComplete || snapshot.DiffBytes != len(diff.Patch) {
				t.Fatalf("snapshot = %+v", snapshot)
			}
			_, err = securityaudit.BuildPrompt(snapshot, test.limit)
			if (err != nil) != test.oversized {
				t.Fatalf("prompt error = %v", err)
			}
		})
	}
}
