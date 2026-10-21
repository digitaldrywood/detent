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
			got, err := c.RevalidatePullRequestAssociation(t.Context(), issue, true)
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
			_, err := c.RevalidatePullRequestAssociation(t.Context(), connector.Issue{ID: tt.id, Identifier: tt.identifier}, true)
			if err == nil {
				t.Fatal("incomplete association evidence accepted")
			}
		})
	}
}

func TestRevalidatePullRequestAssociationWithoutStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		linked     bool
		discover   bool
		state      string
		mergedAt   string
		head       string
		number     int
		httpStatus int
		wantErr    bool
	}{
		{name: "changed cross repository association", linked: true, state: "closed", mergedAt: `"2026-09-05T23:00:00Z"`, head: "fresh-head", number: 2239},
		{name: "cached merged fresh open", linked: true, state: "open", mergedAt: "null", head: "snapshot-head", number: 2239},
		{name: "closed unmerged", linked: true, state: "closed", mergedAt: "null", head: "fresh-head", number: 2239},
		{name: "missing head", linked: true, state: "closed", mergedAt: `"2026-09-05T23:00:00Z"`, number: 2239},
		{name: "wrong PR response", linked: true, state: "closed", mergedAt: `"2026-09-05T23:00:00Z"`, head: "other-head", number: 2240, wantErr: true},
		{name: "not found", linked: true, httpStatus: http.StatusNotFound, wantErr: true},
		{name: "forbidden", linked: true, httpStatus: http.StatusForbidden, wantErr: true},
		{name: "managed branch", state: "closed", mergedAt: `"2026-09-05T23:00:00Z"`, head: "branch-head", number: 2239},
		{name: "discover managed branch", discover: true, state: "closed", mergedAt: `"2026-09-05T23:00:00Z"`, head: "branch-head", number: 2239},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			refs := "[]"
			prRepo := "digitaldrywood/detent"
			if tt.linked {
				prRepo = "example/implementation"
				refs = `[{"number":2239,"state":"MERGED","repository":{"nameWithOwner":"example/implementation"}}]`
			}
			body := fmt.Sprintf(`{"number":%d,"state":%q,"merged_at":%s,"head":{"ref":"detent/2238","sha":%q}}`, tt.number, tt.state, tt.mergedAt, tt.head)
			if tt.httpStatus != 0 {
				body = `{"message":"unavailable"}`
			}
			responses := []graphqlTestResponse{
				{body: fmt.Sprintf(`{"data":{"nodes":[{"__typename":"Issue","id":"I_2238","number":2238,"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":%s}}]}}`, refs)},
			}
			if tt.discover {
				responses = append(responses, graphqlTestResponse{method: http.MethodGet, path: restPullRequestsPath(pullRequestRepo{Owner: "digitaldrywood", Name: "detent"}, 1), body: "[" + body + "]"})
			}
			responses = append(responses, graphqlTestResponse{method: http.MethodGet, path: "/repos/" + prRepo + "/pulls/2239", status: tt.httpStatus, body: body})
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent"})
			oldNumber := 2238
			if !tt.linked {
				oldNumber = 2239
			}
			issue := connector.Issue{ID: "I_2238", Identifier: "digitaldrywood/detent#2238", PRNumber: &oldNumber, PRRepository: "digitaldrywood/detent", PullRequest: &connector.PullRequest{Number: oldNumber, State: "MERGED", HeadSHA: "snapshot-head", CIStatus: "success"}}
			if tt.discover {
				issue.PRNumber, issue.PRRepository, issue.PullRequest = nil, "", nil
			}
			got, err := c.RevalidatePullRequestAssociation(t.Context(), issue, false)
			if tt.wantErr {
				if err == nil && (got.PullRequest == nil || got.PullRequest.HydrationUnavailableReason == "") {
					t.Fatal("unavailable or invalid PR accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got.PRNumber == nil || *got.PRNumber != 2239 || got.PRRepository != prRepo || got.PRVerifiedAt.IsZero() || got.PullRequest == nil || got.PullRequest.HeadSHA != tt.head || got.PullRequest.CIStatus != "" {
					t.Fatalf("fresh scalar association = %+v", got)
				}
				wantState := "CLOSED"
				if tt.mergedAt != "null" {
					wantState = "MERGED"
				} else if tt.state == "open" {
					wantState = "OPEN"
				}
				if got.PullRequest.State != wantState {
					t.Fatalf("state = %q, want %q", got.PullRequest.State, wantState)
				}
			}
			if requests := server.requests(); len(requests) != len(responses) {
				t.Fatalf("requests = %d, want %d without status enrichment", len(requests), len(responses))
			}
		})
	}
}
