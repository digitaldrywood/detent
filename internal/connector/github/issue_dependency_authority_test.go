package github

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDependencyAuthority(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		body       string
		comment    string
		status     int
		native     string
		wantSource string
	}{
		{name: "native empty retains body", body: "Depends on: #100", status: http.StatusOK, native: `[]`, wantSource: connector.BlockedRefSourceProse},
		{name: "bold body duplicate", body: "**Depends on:** #100\n- Depends on #100 so the helpers exist.", status: http.StatusOK, native: `[]`, wantSource: connector.BlockedRefSourceProse},
		{name: "native empty ignores old workpad", comment: "## Codex Workpad\nBlocked by: #100", status: http.StatusOK, native: `[]`},
		{name: "native wins", body: "Depends on: #100", status: http.StatusOK, native: `[{"node_id":"I_100","number":100,"state":"open"}]`, wantSource: connector.BlockedRefSourceNative},
		{name: "unsupported uses body", body: "Depends on: #100", status: http.StatusNotFound, native: `{"message":"Not Found"}`, wantSource: connector.BlockedRefSourceProse},
		{name: "unsupported ignores old workpad", comment: "## Codex Workpad\nBlocked by: #100", status: http.StatusNotFound, native: `{"message":"Not Found"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1073/dependencies/blocked_by?per_page=100", status: tt.status, body: tt.native}})
			c := newGitHubTestConnector(t, server, Config{})
			node := githubIssueNode{Number: 1073, Body: tt.body, Repository: repository{NameWithOwner: "digitaldrywood/detent"}, Comments: nodeConnection[issueComment]{Nodes: []issueComment{{Body: tt.comment}}}}
			issue := connector.Issue{Identifier: "digitaldrywood/detent#1073", Description: tt.body, Comments: connectorIssueComments(node.Comments.Nodes), BlockedBy: parseBlockedByFromIssueText(node, "digitaldrywood/detent")}
			if err := c.hydrateIssueBlockedByRefs(context.Background(), &issue); err != nil {
				t.Fatal(err)
			}
			wantIgnored := tt.status == http.StatusOK && tt.wantSource == "" && (tt.body != "" || tt.comment != "")
			if got := strings.Join(issue.DependencyNotes, " "); wantIgnored {
				if got != "digitaldrywood/detent#100: prose dependency ignored: native relation absent" {
					t.Fatalf("DependencyNotes = %q", got)
				}
			} else if got != "" {
				t.Fatalf("unexpected DependencyNotes = %q", got)
			}
			if tt.wantSource == "" {
				if len(issue.BlockedBy) != 0 {
					t.Fatalf("BlockedBy = %+v, want no blockers", issue.BlockedBy)
				}
			} else if len(issue.BlockedBy) != 1 || issue.BlockedBy[0].Source != tt.wantSource || issue.BlockedBy[0].Identifier != "digitaldrywood/detent#100" {
				t.Fatalf("BlockedBy = %+v, want #100 source %s", issue.BlockedBy, tt.wantSource)
			}
		})
	}
}

func TestPublicReadDependencyAuthority(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"Todo", "In Progress"} {
		for _, tt := range []struct {
			name string
			read func(*Connector) ([]connector.Issue, error)
		}{
			{name: "bounded candidates", read: func(c *Connector) ([]connector.Issue, error) {
				result, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{state}, Limit: 1})
				return result.Issues, err
			}},
			{name: "legacy candidates", read: func(c *Connector) ([]connector.Issue, error) { return c.FetchCandidateIssues(t.Context()) }},
			{name: "limited board states", read: func(c *Connector) ([]connector.Issue, error) {
				return c.FetchIssuesByStatesLimit(t.Context(), []string{state}, 1)
			}},
			{name: "board states", read: func(c *Connector) ([]connector.Issue, error) {
				return c.FetchIssuesByStates(t.Context(), []string{state})
			}},
		} {
			t.Run(state+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				server := newGraphQLTestServer(t, []graphqlTestResponse{
					{method: http.MethodGet, body: `[{"node_id":"I_101","number":101,"title":"Dependency authority","body":"","comments":1,"state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/101","labels":[{"name":"detent:` + strings.ToLower(strings.ReplaceAll(state, " ", "-")) + `"}]}]`},
					{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/101/dependencies/blocked_by?per_page=100", body: `[]`},
					{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/101/comments?per_page=100", body: `[{"id":1,"body":"Blocked by: #100"}]`},
					{body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_101","closedByPullRequestsReferences":{"nodes":[]}}]}}`},
					{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all", body: `[]`},
				})
				c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent", ActiveStates: []string{state}})
				issues, err := tt.read(c)
				if err != nil {
					t.Fatal(err)
				}
				if len(issues) != 1 {
					t.Fatalf("issues = %+v", issues)
				}
				issue := issues[0]
				if len(issue.BlockedBy) != 0 || issue.DependencySource != connector.BlockedRefSourceNative {
					t.Fatalf("dependencies = %+v, source %q", issue.BlockedBy, issue.DependencySource)
				}
				if got := strings.Join(issue.DependencyNotes, " "); got != "digitaldrywood/detent#100: prose dependency ignored: native relation absent" {
					t.Fatalf("notes = %q", got)
				}
				nativeReads := 0
				for _, request := range server.requests() {
					if strings.Contains(request["path"].(string), "/dependencies/blocked_by") {
						nativeReads++
					}
				}
				if nativeReads != 1 {
					t.Fatalf("native reads = %d, want 1", nativeReads)
				}
			})
		}
	}
}

func TestTodoNativeDependencyRateLimitDoesNotReturnCandidates(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "Depends on: #100"} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, body: `[{"node_id":"I_101","number":101,"body":"` + body + `","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/101","labels":[{"name":"detent:todo"}]}]`},
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/101/dependencies/blocked_by?per_page=100", status: http.StatusForbidden, headers: map[string]string{"Retry-After": "120"}, body: `{"message":"secondary rate limit"}`},
			})
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent", ActiveStates: []string{"Todo"}})
			result, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Todo"}, Limit: 1})
			if !errors.Is(err, ErrRateLimited) || len(result.Issues) != 0 {
				t.Fatalf("candidates = %+v, err = %v", result.Issues, err)
			}
		})
	}
}

func TestTodoNativeDependencyBudgetReserveDoesNotReturnCandidates(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "Depends on: #100"} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, headers: map[string]string{"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "2000", "X-RateLimit-Resource": "core"}, body: `[{"node_id":"I_101","number":101,"body":"` + body + `","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/101","labels":[{"name":"detent:todo"}]}]`},
			})
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent", ActiveStates: []string{"Todo"}, RESTMinRemainingReserve: 1000})
			now := time.Now()
			headers := http.Header{}
			headers.Set("X-RateLimit-Limit", "5000")
			headers.Set("X-RateLimit-Remaining", "900")
			headers.Set("X-RateLimit-Resource", "core")
			headers.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
			c.client.recordRESTRateLimitFromHeaders(t.Context(), "", c.client.restCredentialIdentity("token"), http.MethodGet,
				"/repos/digitaldrywood/detent/issues/101/dependencies/blocked_by?per_page=100", http.StatusOK, headers, nil, now, false)

			result, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Todo"}, Limit: 1})
			if !errors.Is(err, ErrRESTBudgetReserved) || len(result.Issues) != 0 {
				t.Fatalf("candidates = %+v, err = %v", result.Issues, err)
			}
		})
	}
}

func TestNativeDependencyFailurePreservesRefsAndRechecksCapability(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			const path = "/repos/digitaldrywood/detent/issues/101/dependencies/blocked_by?per_page=100"
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: path, body: `[{"node_id":"I_100","number":100,"state":"open"}]`},
				{method: http.MethodGet, path: path, status: status, body: `{"message":"temporary failure"}`},
				{method: http.MethodGet, path: path, body: `[]`},
			})
			c := newGitHubTestConnector(t, server, Config{})
			issue := connector.Issue{Identifier: "digitaldrywood/detent#101"}
			if err := c.hydrateIssueBlockedByRefs(t.Context(), &issue); err != nil {
				t.Fatal(err)
			}
			if err := c.hydrateIssueBlockedByRefs(t.Context(), &issue); err == nil {
				t.Fatal("native failure certified a successful fallback")
			}
			if len(issue.BlockedBy) != 1 || issue.BlockedBy[0].Source != connector.BlockedRefSourceNative || issue.DependencySource == connector.BlockedRefSourceProse {
				t.Fatalf("native failure cleared blockers: %+v", issue)
			}
			if err := c.hydrateIssueBlockedByRefs(t.Context(), &issue); err != nil {
				t.Fatal(err)
			}
			if len(issue.BlockedBy) != 0 || issue.DependencySource != connector.BlockedRefSourceNative {
				t.Fatalf("successful retry did not use empty native list: %+v", issue)
			}
			if got := len(server.requests()); got != 3 {
				t.Fatalf("native reads = %d, want 3", got)
			}
		})
	}
}

func TestDependencyAuthorityIgnoresFencedExamples(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"```text\nDepends on: #100\n```",
		"~~~text\nDepends on: #100\n~~~",
		"````text\n```\nDepends on: #100\n```\n````",
		"```text\nDepends on: #100",
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{{method: http.MethodGet, body: "[]"}})
			c := newGitHubTestConnector(t, server, Config{})
			issue := connector.Issue{Identifier: "digitaldrywood/detent#101", Description: body}
			if err := c.hydrateIssueBlockedByRefs(t.Context(), &issue); err != nil {
				t.Fatal(err)
			}
			if len(issue.BlockedBy) != 0 || len(issue.DependencyNotes) != 0 {
				t.Fatalf("fenced example became dependency: %+v, notes: %v", issue.BlockedBy, issue.DependencyNotes)
			}
		})
	}
}

func TestTodoBodyDependencyLookupFailureDoesNotReturnCandidates(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, body: `[{"node_id":"I_101","number":101,"body":"Depends on: #100","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/101","labels":[{"name":"detent:todo"}]}]`},
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/101/dependencies/blocked_by?per_page=100", body: "[]"},
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/100", status: status, body: `{"message":"blocker lookup failed"}`},
			})
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent", ActiveStates: []string{"Todo"}})
			result, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Todo"}, Limit: 1})
			if err == nil || len(result.Issues) != 0 {
				t.Fatalf("candidates = %+v, err = %v; want no candidates and hydration error", result.Issues, err)
			}
		})
	}
}
