package github

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDependencyCommentEvidenceCache(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	zero := time.Time{}
	for _, tc := range []struct {
		name      string
		revisions []*time.Time
		wantReads int
	}{
		{"unchanged", []*time.Time{&now, &now}, 1},
		{"updated", []*time.Time{&now, &later, &later}, 2},
		{"missing revision", []*time.Time{nil, nil}, 2},
		{"zero revision", []*time.Time{&zero, &zero}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			responses := []graphqlTestResponse{}
			for i, revision := range tc.revisions {
				responses = append(responses, graphqlTestResponse{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1073/dependencies/blocked_by?per_page=100", body: `[]`})
				if i == 0 || revision == nil || revision.IsZero() || tc.revisions[i-1] == nil || !revision.Equal(*tc.revisions[i-1]) {
					responses = append(responses, graphqlTestResponse{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1073/comments?per_page=100", body: fmt.Sprintf(`[{"id":1,"body":"Depends on: #%d"}]`, 100+i)})
				}
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{})
			expectedComment := ""
			for i, revision := range tc.revisions {
				issue := connector.Issue{Identifier: "digitaldrywood/detent#1073", CommentCount: 1, UpdatedAt: revision}
				if i == 0 || revision == nil || revision.IsZero() || tc.revisions[i-1] == nil || !revision.Equal(*tc.revisions[i-1]) {
					expectedComment = fmt.Sprintf("Depends on: #%d", 100+i)
				}
				if err := c.hydrateIssueBlockedByRefs(t.Context(), &issue); err != nil {
					t.Fatal(err)
				}
				if len(issue.Comments) != 1 || issue.Comments[0].Body != expectedComment {
					t.Fatalf("comments = %+v; want %q", issue.Comments, expectedComment)
				}
				if len(issue.BlockedBy) != 0 || len(issue.DependencyNotes) != 1 {
					t.Fatalf("dependency evidence = %+v", issue)
				}
				issue.Comments[0].Body = "caller mutation"
			}
			reads := 0
			for _, request := range server.requests() {
				if request["path"] == "/repos/digitaldrywood/detent/issues/1073/comments?per_page=100" {
					reads++
				}
			}
			if reads != tc.wantReads {
				t.Fatalf("comment reads = %d; want %d", reads, tc.wantReads)
			}
		})
	}
}

func TestDependencyCommentEvidenceCacheDoesNotCacheFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	path := "/repos/digitaldrywood/detent/issues/1073/comments?per_page=100"
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: path, status: http.StatusBadRequest, body: `{"message":"failed fetch"}`},
		{method: http.MethodGet, path: path, body: `[{"id":1,"body":"fresh"}]`},
	})
	c := newGitHubTestConnector(t, server, Config{})
	ref := issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1073}
	if _, err := c.dependencyCommentEvidence(t.Context(), ref, &now); err == nil {
		t.Fatal("expected fetch error")
	}
	for range 2 {
		comments, err := c.dependencyCommentEvidence(t.Context(), ref, &now)
		if err != nil {
			t.Fatal(err)
		}
		if len(comments) != 1 || comments[0].Body != "fresh" {
			t.Fatalf("comments = %+v", comments)
		}
	}
	if len(server.requests()) != 2 {
		t.Fatalf("requests = %d; want 2", len(server.requests()))
	}
}

func TestDependencyCommentEvidenceCacheKeepsNewerRevision(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	path := "/repos/digitaldrywood/detent/issues/1073/comments?per_page=100"
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: path, body: `[{"id":1,"body":"newer"}]`},
		{method: http.MethodGet, path: path, body: `[{"id":1,"body":"older"}]`},
		{method: http.MethodGet, path: "/repos/fixture/other/issues/1073/comments?per_page=100", body: `[{"id":1,"body":"other repo"}]`},
	})
	c := newGitHubTestConnector(t, server, Config{})
	ref := issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1073}
	for _, tc := range []struct {
		ref      issueRef
		revision time.Time
		body     string
	}{
		{ref, later, "newer"}, {ref, now, "older"}, {ref, later, "newer"},
		{issueRef{Owner: "fixture", Name: "other", Number: 1073}, later, "other repo"},
	} {
		comments, err := c.dependencyCommentEvidence(t.Context(), tc.ref, &tc.revision)
		if err != nil {
			t.Fatal(err)
		}
		if len(comments) != 1 || comments[0].Body != tc.body {
			t.Fatalf("comments = %+v; want %q", comments, tc.body)
		}
	}
	if len(server.requests()) != 3 {
		t.Fatalf("requests = %d; want 3", len(server.requests()))
	}
}
