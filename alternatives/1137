package github

import (
	"context"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/publication"
)

func TestConnectorProtectsCrossProjectIssueWrites(t *testing.T) {
	t.Parallel()

	const original = "Failure in private/source#42 at /srv/private/source/worktree by @private-user"
	tests := []struct {
		name     string
		info     string
		policy   publication.Policy
		wantBody string
	}{
		{
			name: "public destination redacts",
			info: `{"id":1,"full_name":"public/destination","private":false,"visibility":"public"}`,
			policy: publication.Policy{Sources: []publication.Source{{
				Repository: "private/source",
				Workspaces: []string{"/srv/private/source"},
				Logins:     []string{"private-user"},
			}}},
			wantBody: "Failure in project-A#1 at <workspace> by @contributor-A",
		},
		{
			name: "private destination preserves bytes",
			info: `{"id":1,"full_name":"public/destination","private":true,"visibility":"private"}`,
			policy: publication.Policy{Sources: []publication.Source{{
				Repository: "private/source",
				Workspaces: []string{"/srv/private/source"},
				Logins:     []string{"private-user"},
			}}},
			wantBody: original,
		},
		{
			name: "unknown visibility redacts",
			info: `{}`,
			policy: publication.Policy{Sources: []publication.Source{{
				Repository: "private/source",
				Workspaces: []string{"/srv/private/source"},
				Logins:     []string{"private-user"},
			}}},
			wantBody: "Failure in project-A#1 at <workspace> by @contributor-A",
		},
		{
			name: "same repository preserves references",
			info: `{"id":1,"full_name":"public/destination","private":false,"visibility":"public"}`,
			policy: publication.Policy{Sources: []publication.Source{{
				Repository: "public/destination",
				Workspaces: []string{"/srv/private/source"},
				Logins:     []string{"private-user"},
			}}},
			wantBody: original,
		},
		{
			name: "operator override preserves bytes",
			info: `{"id":1,"full_name":"public/destination","private":false,"visibility":"public"}`,
			policy: publication.Policy{
				Sources: []publication.Source{{
					Repository: "private/source",
					Workspaces: []string{"/srv/private/source"},
					Logins:     []string{"private-user"},
				}},
				AllowPublicCrossProjectDetails: true,
			},
			wantBody: original,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/public/destination", body: test.info},
				{method: http.MethodPost, path: "/repos/public/destination/issues", body: `{"node_id":"I_1","number":1,"title":"Failure","body":"body","state":"open"}`},
				{method: http.MethodPost, path: "/repos/public/destination/issues/1/comments", body: `{"node_id":"IC_1"}`},
			})
			candidate := newGitHubTestConnector(t, server, Config{
				Repository:  "public/destination",
				Publication: test.policy,
			})

			issue, err := candidate.CreateIssue(context.Background(), connector.IssueDraft{Title: "Failure", Body: original})
			if err != nil {
				t.Fatalf("CreateIssue() error = %v", err)
			}
			if err := candidate.CreateComment(context.Background(), issue.ID, original); err != nil {
				t.Fatalf("CreateComment() error = %v", err)
			}

			requests := server.requests()
			if len(requests) != 3 {
				t.Fatalf("request count = %d, want one visibility lookup and two writes", len(requests))
			}
			if got := requests[1]["body"].(map[string]any)["body"]; got != test.wantBody {
				t.Errorf("issue body = %q, want %q", got, test.wantBody)
			}
			if got := requests[2]["body"].(map[string]any)["body"]; got != test.wantBody {
				t.Errorf("comment body = %q, want %q", got, test.wantBody)
			}
		})
	}
}
