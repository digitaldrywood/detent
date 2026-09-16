package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestProjectPageSchemaFallback(t *testing.T) {
	for _, entry := range []string{"admission", "refresh"} {
		for _, tt := range []struct {
			name         string
			status       int
			body         string
			transportErr error
			wantErr      error
			wantRequests int
		}{
			{name: "success", status: 200, body: `{"data":{"node":{"items":{"nodes":[]}}}}`, wantRequests: 1},
			{name: "undefined field", status: 200, body: `{"errors":[{"message":"Field 'blockedBy' doesn't exist on type 'Issue'"}]}`, wantRequests: 2},
			{name: "cannot query field", status: 200, body: `{"errors":[{"message":"Cannot query field blockedBy on type Issue"}]}`, wantRequests: 2},
			{name: "429", status: 429, body: `{"message":"rate limit exceeded"}`, wantErr: ErrRateLimited, wantRequests: 1},
			{name: "graphql rate limit", status: 200, body: `{"errors":[{"type":"RATE_LIMITED","message":"rate limit exceeded"}]}`, wantErr: ErrRateLimited, wantRequests: 1},
			{name: "generic graphql failure", status: 200, body: `{"errors":[{"message":"temporary service failure"}]}`, wantErr: ErrGraphQLErrors, wantRequests: 1},
			{name: "mixed schema and service failure", status: 200, body: `{"errors":[{"message":"Cannot query field blockedBy on type Issue"},{"message":"temporary service failure"}]}`, wantErr: ErrGraphQLErrors, wantRequests: 1},
			{name: "transport", transportErr: io.ErrUnexpectedEOF, wantErr: io.ErrUnexpectedEOF, wantRequests: 1},
			{name: "cancelled", transportErr: context.Canceled, wantErr: context.Canceled, wantRequests: 1},
			{name: "deadline", transportErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded, wantRequests: 1},
		} {
			t.Run(entry+"/"+tt.name, func(t *testing.T) {
				server := newGraphQLTestServer(t, nil)
				c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1", Repository: "fixture/repo", ActiveStates: []string{"Todo"}})
				requests := 0
				c.client.httpClient = staticHTTPClient{do: func(r *http.Request) (*http.Response, error) {
					requests++
					var payload struct {
						Query     string
						Variables map[string]any
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					wantQuery := candidateProjectItemsQuery
					if requests > 1 {
						wantQuery = schedulerProjectItemsQuery
					}
					if entry == "refresh" {
						wantQuery = thinRefreshProjectItemsQuery
					}
					if payload.Query != wantQuery {
						t.Errorf("unexpected query on request %d", requests)
					}
					if requests == 1 && tt.transportErr != nil {
						return nil, tt.transportErr
					}
					status, body := tt.status, tt.body
					if requests > 1 {
						status, body = 200, `{"data":{"node":{"items":{"nodes":[]}}}}`
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}}
				var err error
				if entry == "admission" {
					_, err = c.readProjectCandidates(t.Context(), connector.CandidateRequest{States: []string{"Todo"}, Limit: 10}, candidateCursor{})
				} else {
					result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, nil, connector.IssueFilterHint{})
					err = result.CandidateError
				}
				if entry == "refresh" && tt.wantRequests == 2 {
					tt.wantRequests, tt.wantErr = 1, ErrGraphQLErrors
				}
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
				if requests != tt.wantRequests {
					t.Errorf("requests = %d, want %d", requests, tt.wantRequests)
				}
			})
		}
	}
}

func TestApplySchedulerEvidenceBlockerReason(t *testing.T) {
	for _, tt := range []struct{ name, existing, body, want string }{
		{"retain existing", "existing reason", "", "existing reason"},
		{"populate missing", "", "## Human Action Needed\n\nnew reason", "new reason"},
		{"refresh existing from evidence", "existing reason", "## Human Action Needed\n\nnew reason", "new reason"},
		{"remain empty", "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newGitHubTestConnector(t, newGraphQLTestServer(t, nil), Config{ProjectSlug: "PVT_1", Repository: "fixture/repo"})
			issue, err := c.applySchedulerEvidence(connector.Issue{ID: "I_1", Identifier: "fixture/repo#1", BlockerReason: tt.existing}, githubIssueNode{ID: "I_1", Body: tt.body, BlockedBy: &issueNodesConnection{}})
			if err != nil {
				t.Fatal(err)
			}
			if issue.BlockerReason != tt.want {
				t.Errorf("reason = %q, want %q", issue.BlockerReason, tt.want)
			}
		})
	}
}
