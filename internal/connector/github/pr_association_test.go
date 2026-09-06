package github

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRevalidatePullRequestAssociation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		linked     bool
		branch     string
		prRepo     string
		state      string
		wantSource string
	}{
		{name: "diagnostic mention", branch: "detent/detent-digitaldrywood_detent_2198-addbb5fdd715", state: "OPEN"},
		{name: "unrelated merged PR", branch: "detent/detent-digitaldrywood_detent_2198-addbb5fdd715", state: "MERGED"},
		{name: "closing relationship", linked: true, state: "OPEN", wantSource: "github_closing_reference"},
		{name: "manual link arbitrary branch", linked: true, branch: "manual/fix", state: "OPEN", wantSource: "github_closing_reference"},
		{name: "cross repository link", linked: true, prRepo: "example/implementation", state: "MERGED", wantSource: "github_closing_reference"},
		{name: "managed branch", branch: "detent/detent-digitaldrywood_detent_2238-c8b51508b11a", state: "OPEN", wantSource: "detent_branch"},
		{name: "legacy branch", branch: "detent/2238", state: "MERGED", wantSource: "detent_branch"},
		{name: "similar issue number", branch: "detent/22380-fix", state: "OPEN"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			prRepo := tt.prRepo
			if prRepo == "" {
				prRepo = "digitaldrywood/detent"
			}
			references := "[]"
			if tt.linked {
				references = fmt.Sprintf(`[{"number":2239,"state":%q,"repository":{"nameWithOwner":%q}}]`, tt.state, prRepo)
			}
			responses := []graphqlTestResponse{{body: fmt.Sprintf(`{"data":{"nodes":[{"__typename":"Issue","id":"I_2238","number":2238,"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":%s},"timelineItems":{"nodes":[{"__typename":"LabeledEvent","createdAt":"2026-09-05T22:37:12Z","label":{"name":"detent:human-review"}}]}}]}}`, references)}}
			mergedAt := "null"
			if tt.state == "MERGED" {
				mergedAt = `"2026-09-05T23:00:00Z"`
			}
			pr := fmt.Sprintf(`{"number":2239,"state":"open","merged_at":%s,"head":{"ref":%q},"body":"Separate follow-up #2238; not a dependency"}`, mergedAt, tt.branch)
			if !tt.linked {
				responses = append(responses, graphqlTestResponse{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/2239", body: pr})
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent"})
			issue := connector.Issue{ID: "I_2238", Identifier: "digitaldrywood/detent#2238", State: "Human Review", PRNumber: new(2239), PRRepository: prRepo, PullRequest: &connector.PullRequest{Number: 2239, State: tt.state, BranchName: tt.branch}}
			got, err := c.RevalidatePullRequestAssociation(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if got.PRSource != tt.wantSource || got.PRVerifiedAt.IsZero() {
				t.Fatalf("association = %q at %v, want %q with verification time", got.PRSource, got.PRVerifiedAt, tt.wantSource)
			}
			if tt.wantSource == "" {
				if got.PullRequest != nil || got.PRNumber != nil || got.PRRepository != "" {
					t.Fatal("unrelated PR survived revalidation")
				}
			} else if got.PullRequest == nil || got.PullRequest.Number != 2239 || got.PRRepository != prRepo {
				t.Fatalf("valid association lost: %#v", got)
			}
			wantStage := time.Date(2026, 9, 5, 22, 37, 12, 0, time.UTC)
			if got.StageUpdatedAt == nil || !got.StageUpdatedAt.Equal(wantStage) {
				t.Fatalf("stage timestamp = %v", got.StageUpdatedAt)
			}
		})
	}
}

func TestRevalidatePullRequestAssociationRejectsIncompleteEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		body       string
		id         string
		identifier string
	}{
		{name: "wrong node", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_2198"}]}}`, id: "I_2238", identifier: "digitaldrywood/detent#2238"},
		{name: "wrong issue number", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_2238","number":2198,"repository":{"nameWithOwner":"digitaldrywood/detent"}}]}}`, id: "I_2238", identifier: "digitaldrywood/detent#2238"},
		{name: "wrong repository", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_2238","number":2238,"repository":{"nameWithOwner":"example/other"}}]}}`, id: "I_2238", identifier: "digitaldrywood/detent#2238"},
		{name: "lookup error", body: `{"errors":[{"message":"unavailable"}]}`, id: "I_2238", identifier: "digitaldrywood/detent#2238"},
		{name: "missing node ID", identifier: "digitaldrywood/detent#2238"},
		{name: "missing repository", id: "I_2238"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var responses []graphqlTestResponse
			if tt.body != "" {
				responses = append(responses, graphqlTestResponse{body: tt.body})
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent"})
			_, err := c.RevalidatePullRequestAssociation(t.Context(), connector.Issue{ID: tt.id, Identifier: tt.identifier})
			if err == nil {
				t.Fatal("incomplete association evidence accepted")
			}
		})
	}
}
