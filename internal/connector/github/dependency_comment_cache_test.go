package github

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDependencyCommentEvidenceFreshWorkpad(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	zero := time.Time{}
	for _, tc := range []struct {
		name      string
		revisions []*time.Time
	}{
		{"unchanged", []*time.Time{&now, &now, &now}},
		{"updated", []*time.Time{&now, &later, &later}},
		{"missing revision", []*time.Time{nil, nil}},
		{"zero revision", []*time.Time{&zero, &zero}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			statuses := []string{"blocked", "in_progress", "complete"}
			bodies := make([]string, len(tc.revisions))
			responses := []graphqlTestResponse{}
			for i := range tc.revisions {
				action := "null"
				if i == 0 {
					action = "Restore provider quota."
				}
				bodies[i] = fmt.Sprintf("Depends on: #%d\n\n## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: %s\nblockers: []\nhuman_action: %s\n```", 100+i, statuses[i], action)
				responses = append(responses,
					graphqlTestResponse{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1073/dependencies/blocked_by?per_page=100", body: `[]`},
					graphqlTestResponse{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1073/comments?per_page=100", body: fmt.Sprintf(`[{"id":1,"node_id":"C1","body":%q,"user":{"login":"operator"},"author_association":"OWNER","created_at":%q,"updated_at":%q}]`, bodies[i], now.Format(time.RFC3339), now.Add(time.Duration(i)*time.Minute).Format(time.RFC3339))},
				)
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{})
			for i, revision := range tc.revisions {
				issue := connector.Issue{Identifier: "digitaldrywood/detent#1073", State: "Todo", CommentCount: 1, UpdatedAt: revision}
				if err := c.hydrateIssueBlockedByRefs(t.Context(), &issue); err != nil {
					t.Fatal(err)
				}
				if len(issue.Comments) != 1 || issue.Comments[0].Body != bodies[i] || issue.Comments[0].ID != "C1" || !issue.Comments[0].AuthorAuthorized {
					t.Fatalf("current authorized comments = %+v", issue.Comments)
				}
				signal := issue.WorkpadSignal
				if signal == nil || signal.Invalid != nil || signal.Status != statuses[i] || signal.RecordedAt == nil || !signal.RecordedAt.Equal(now.Add(time.Duration(i)*time.Minute)) || (signal.HumanAction != "") != (i == 0) {
					t.Fatalf("same-comment edit retained previous Workpad authority: %+v", signal)
				}
				if len(issue.BlockedBy) != 0 || issue.DependencySource != connector.BlockedRefSourceNative || len(issue.DependencyNotes) != 1 {
					t.Fatalf("native dependency evidence = %+v", issue)
				}
			}
		})
	}
}
