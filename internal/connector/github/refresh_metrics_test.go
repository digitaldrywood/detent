package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRefreshGraphQLOperationAttribution(t *testing.T) {
	for _, tt := range []struct {
		name          string
		query         string
		variables     map[string]any
		data          string
		wantRequested int64
		wantReturned  int64
	}{
		{
			name:          "batched nested references",
			query:         labelIssuePullRequestReferencesQuery,
			variables:     map[string]any{"issueIds": []string{"I_1", "I_2"}},
			data:          `{"nodes":[{"id":"I_1","timelineItems":{"nodes":[{}]},"closedByPullRequestsReferences":{"nodes":[{},null]}},{"id":"I_2","timelineItems":{"nodes":[]},"closedByPullRequestsReferences":{"nodes":[]}}],"rateLimit":{"cost":7}}`,
			wantRequested: 602,
			wantReturned:  4,
		},
		{
			name:          "variable page size",
			query:         `query DetentGitHubProjectItems($first:Int!) { node(id:"P_1") { ... on ProjectV2 { items(first:$first) { nodes { id } } } } rateLimit { cost } }`,
			variables:     map[string]any{"first": 25},
			data:          `{"node":{"items":{"nodes":[{"id":"PVTI_1"}]}},"rateLimit":{"cost":2}}`,
			wantRequested: 26,
			wantReturned:  2,
		},
		{
			name:          "aliased root nodes",
			query:         `query DetentGitHubCandidateHydration($id0:ID!,$id1:ID!) { issue0:node(id:$id0) { ... on Issue { id } } issue1:node(id:$id1) { ... on Issue { id } } rateLimit { cost } }`,
			variables:     map[string]any{"id0": "I_1", "id1": "I_2"},
			data:          `{"issue0":{"id":"I_1"},"issue1":{"id":"I_2"},"rateLimit":{"cost":1}}`,
			wantRequested: 2,
			wantReturned:  2,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": json.RawMessage(tt.data)})
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(ClientConfig{Endpoint: server.URL, TokenSource: StaticTokenSource("token"), HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			ctx, points := connector.WithGraphQLPoints(t.Context())
			for range 2 {
				if err := client.GraphQLWithType(ctx, "test_type", tt.query, tt.variables, nil); err != nil {
					t.Fatal(err)
				}
			}
			operations := points.Operations()
			if len(operations) != 1 {
				t.Fatalf("operations = %+v", operations)
			}
			got := operations[0]
			if got.Name != graphQLOperationName(tt.query, "") || got.Requests != 2 || got.Points != points.Total() || got.NodesRequested != 2*tt.wantRequested || got.NodesReturned != 2*tt.wantReturned || got.WallTime <= 0 {
				t.Fatalf("operation = %+v, total points = %d", got, points.Total())
			}
		})
	}
}

func TestRefreshGraphQLRetryWallTimeCountsEachAttemptOnce(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "HTTP authentication failure", status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
		{name: "GraphQL authentication failure", status: http.StatusOK, body: `{"errors":[{"message":"Bad credentials"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source := newRefreshingTokenTestSource("stale", "fresh")
				client, err := NewClient(ClientConfig{
					Endpoint:    "https://example.test/graphql",
					TokenSource: source,
					HTTPClient: staticHTTPClient{do: func(req *http.Request) (*http.Response, error) {
						if req.Header.Get("Authorization") == "Bearer stale" {
							return jsonResponse(req, tt.status, tt.body, nil), nil
						}
						time.Sleep(time.Second)
						return jsonResponse(req, http.StatusOK, `{"data":{"rateLimit":{"cost":1}}}`, nil), nil
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx, metrics := connector.WithGraphQLPoints(t.Context())
				if err := client.GraphQLWithType(ctx, "test", "query RetryOperation { rateLimit { cost } }", nil, nil); err != nil {
					t.Fatal(err)
				}
				operations := metrics.Operations()
				if len(operations) != 1 || operations[0].Requests != 2 || operations[0].WallTime != time.Second {
					t.Fatalf("operations = %+v, want two attempts and one second", operations)
				}
			})
		})
	}
}

func TestRefreshGraphQLSeparatesOperations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		cost := 2
		if strings.Contains(request.Query, "SecondOperation") {
			cost = 5
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"rateLimit": map[string]any{"cost": cost}}})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{Endpoint: server.URL, TokenSource: StaticTokenSource("token"), HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, metrics := connector.WithGraphQLPoints(t.Context())
	for _, query := range []string{
		"query FirstOperation { rateLimit { cost } }",
		"query SecondOperation { rateLimit { cost } }",
	} {
		if err := client.GraphQLWithType(ctx, "same_query_type", query, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	operations := metrics.Operations()
	if len(operations) != 2 || operations[0].Name != "FirstOperation" || operations[0].Points != 2 || operations[1].Name != "SecondOperation" || operations[1].Points != 5 || metrics.Total() != 7 {
		t.Fatalf("operations = %+v, total = %d", operations, metrics.Total())
	}
}
