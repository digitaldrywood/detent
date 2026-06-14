package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestClientGraphQLSendsBearerRequest(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("Accept = %q, want GitHub JSON media type", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Fatalf("X-GitHub-Api-Version = %q, want 2022-11-28", got)
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		requests <- payload

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	var got struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	err = client.GraphQL(context.Background(), "query Viewer($id: ID!) { viewer { login } }", map[string]any{"id": "PVT_1"}, &got)
	if err != nil {
		t.Fatalf("GraphQL() error = %v", err)
	}
	if got.Viewer.Login != "octocat" {
		t.Fatalf("viewer.login = %q, want octocat", got.Viewer.Login)
	}

	payload := <-requests
	if payload["query"] == "" {
		t.Fatal("query payload is blank")
	}
	variables := payload["variables"].(map[string]any)
	if variables["id"] != "PVT_1" {
		t.Fatalf("variables.id = %v, want PVT_1", variables["id"])
	}
}

func TestClientGraphQLClassifiesFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		headers    map[string]string
		body       string
		want       error
	}{
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			body:       `{"message":"bad credentials"}`,
			want:       ErrAuthenticationFailed,
		},
		{
			name:       "forbidden rate limit",
			statusCode: http.StatusForbidden,
			headers:    map[string]string{"X-RateLimit-Remaining": "0"},
			body:       `{"message":"rate limit"}`,
			want:       ErrRateLimited,
		},
		{
			name:       "secondary rate limit",
			statusCode: http.StatusForbidden,
			headers:    map[string]string{"Retry-After": "120"},
			body:       `{"message":"secondary rate limit"}`,
			want:       ErrRateLimited,
		},
		{
			name:       "not found",
			statusCode: http.StatusNotFound,
			body:       `{"message":"not found"}`,
			want:       ErrNotFound,
		},
		{
			name:       "server error",
			statusCode: http.StatusBadGateway,
			body:       `{"message":"bad gateway"}`,
			want:       ErrTransient,
		},
		{
			name:       "graphql rate limit",
			statusCode: http.StatusOK,
			body:       `{"errors":[{"type":"RATE_LIMITED","message":"slow down"}]}`,
			want:       ErrRateLimited,
		},
		{
			name:       "graphql generic",
			statusCode: http.StatusOK,
			body:       `{"errors":[{"message":"field error"}]}`,
			want:       ErrGraphQLErrors,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			client, err := NewClient(ClientConfig{
				Endpoint:    server.URL,
				TokenSource: StaticTokenSource("test-token"),
				HTTPClient:  server.Client(),
			})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			err = client.GraphQL(context.Background(), "query { viewer { login } }", nil, nil)
			if !errors.Is(err, tt.want) {
				t.Fatalf("GraphQL() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestClientGraphQLCapturesRateLimitSnapshot(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"},"rateLimit":{"limit":5000,"used":120,"remaining":4880,"cost":2,"resetAt":"` + resetAt.Format(time.RFC3339) + `"}}}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	var got struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := client.GraphQL(context.Background(), "query { viewer { login } rateLimit { remaining resetAt cost } }", nil, &got); err != nil {
		t.Fatalf("GraphQL() error = %v", err)
	}

	rateLimit, ok := client.GraphQLRateLimit()
	if !ok {
		t.Fatal("GraphQLRateLimit() ok = false, want true")
	}
	if rateLimit.Limit != 5000 || rateLimit.Used != 120 || rateLimit.Remaining != 4880 || rateLimit.Cost != 2 {
		t.Fatalf("GraphQLRateLimit() = %#v, want limit 5000 used 120 remaining 4880 cost 2", rateLimit)
	}
	if !rateLimit.ResetAt.Equal(resetAt) {
		t.Fatalf("GraphQLRateLimit().ResetAt = %v, want %v", rateLimit.ResetAt, resetAt)
	}
}

func TestClientGraphQLAggregatesRateLimitCostsByQueryType(t *testing.T) {
	t.Parallel()

	responses := make(chan string, 3)
	responses <- `{"data":{"rateLimit":{"limit":5000,"used":10,"remaining":4990,"cost":4,"resetAt":"2026-06-01T13:00:00Z"}}}`
	responses <- `{"data":{"rateLimit":{"limit":5000,"used":13,"remaining":4987,"cost":3,"resetAt":"2026-06-01T13:00:00Z"}}}`
	responses <- `{"data":{"rateLimit":{"limit":5000,"used":15,"remaining":4985,"cost":2,"resetAt":"2026-06-01T13:00:00Z"}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(<-responses))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := client.GraphQLWithType(context.Background(), "candidate_issues", "query { rateLimit { cost } }", nil, nil); err != nil {
		t.Fatalf("first GraphQLWithType() error = %v", err)
	}
	if err := client.GraphQLWithType(context.Background(), "candidate_issues", "query { rateLimit { cost } }", nil, nil); err != nil {
		t.Fatalf("second GraphQLWithType() error = %v", err)
	}
	if err := client.GraphQLWithType(context.Background(), "running_states", "query { rateLimit { cost } }", nil, nil); err != nil {
		t.Fatalf("third GraphQLWithType() error = %v", err)
	}

	usage := client.FlushGraphQLRateLimitUsage()
	if !usage.HasRateLimit {
		t.Fatal("FlushGraphQLRateLimitUsage().HasRateLimit = false, want true")
	}
	if usage.TotalQueries != 3 || usage.TotalCost != 9 {
		t.Fatalf("FlushGraphQLRateLimitUsage() totals = queries %d cost %d, want queries 3 cost 9", usage.TotalQueries, usage.TotalCost)
	}
	if usage.RateLimit.Remaining != 4985 || usage.RateLimit.Cost != 2 {
		t.Fatalf("FlushGraphQLRateLimitUsage().RateLimit = %#v, want last snapshot remaining 4985 cost 2", usage.RateLimit)
	}
	want := []struct {
		queryType string
		count     int64
		cost      int64
	}{
		{queryType: "candidate_issues", count: 2, cost: 7},
		{queryType: "running_states", count: 1, cost: 2},
	}
	if len(usage.QueryCosts) != len(want) {
		t.Fatalf("QueryCosts len = %d, want %d: %#v", len(usage.QueryCosts), len(want), usage.QueryCosts)
	}
	for index, wantCost := range want {
		got := usage.QueryCosts[index]
		if got.QueryType != wantCost.queryType || got.Count != wantCost.count || got.Cost != wantCost.cost {
			t.Fatalf("QueryCosts[%d] = %#v, want %#v", index, got, wantCost)
		}
	}

	client.ResetGraphQLRateLimitUsage()
	usage = client.FlushGraphQLRateLimitUsage()
	if len(usage.QueryCosts) != 0 || usage.TotalQueries != 0 || usage.TotalCost != 0 {
		t.Fatalf("usage after reset = %#v, want no query costs", usage)
	}
}

func TestClientGraphQLInfersMutationCostsFromHeaders(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	type graphQLResponse struct {
		body    string
		headers map[string]string
	}
	responses := make(chan graphQLResponse, 3)
	responses <- graphQLResponse{
		body: `{"data":{"rateLimit":{"limit":5000,"used":10,"remaining":4990,"cost":4,"resetAt":"` + resetAt.Format(time.RFC3339) + `"}}}`,
	}
	responses <- graphQLResponse{
		body: `{"data":{"addComment":{"commentEdge":{"node":{"id":"IC_kw1"}}}}}`,
		headers: map[string]string{
			"X-RateLimit-Limit":     "5000",
			"X-RateLimit-Used":      "13",
			"X-RateLimit-Remaining": "4987",
			"X-RateLimit-Reset":     strconv.FormatInt(resetAt.Unix(), 10),
		},
	}
	responses <- graphQLResponse{
		body: `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_kw1"}}}}`,
		headers: map[string]string{
			"X-RateLimit-Limit":     "5000",
			"X-RateLimit-Used":      "15",
			"X-RateLimit-Remaining": "4985",
			"X-RateLimit-Reset":     strconv.FormatInt(resetAt.Unix(), 10),
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		response := <-responses
		for key, value := range response.headers {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(response.body))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := client.GraphQLWithType(context.Background(), "candidate_issues", "query { rateLimit { cost } }", nil, nil); err != nil {
		t.Fatalf("GraphQLWithType() query error = %v", err)
	}
	if err := client.GraphQLWithType(context.Background(), "create_comment", "mutation { addComment(input: {}) { commentEdge { node { id } } } }", nil, nil); err != nil {
		t.Fatalf("GraphQLWithType() comment mutation error = %v", err)
	}
	if err := client.GraphQLWithType(context.Background(), "update_project_field", "mutation { updateProjectV2ItemFieldValue(input: {}) { projectV2Item { id } } }", nil, nil); err != nil {
		t.Fatalf("GraphQLWithType() field mutation error = %v", err)
	}

	usage := client.FlushGraphQLRateLimitUsage()
	want := []connector.GraphQLQueryCost{
		{QueryType: "candidate_issues", Count: 1, Cost: 4},
		{QueryType: "create_comment", Count: 1, Cost: 3},
		{QueryType: "update_project_field", Count: 1, Cost: 2},
	}
	if usage.TotalQueries != 3 || usage.TotalCost != 9 {
		t.Fatalf("FlushGraphQLRateLimitUsage() totals = queries %d cost %d, want queries 3 cost 9", usage.TotalQueries, usage.TotalCost)
	}
	if len(usage.QueryCosts) != len(want) {
		t.Fatalf("QueryCosts len = %d, want %d: %#v", len(usage.QueryCosts), len(want), usage.QueryCosts)
	}
	for index, wantCost := range want {
		if usage.QueryCosts[index] != wantCost {
			t.Fatalf("QueryCosts[%d] = %#v, want %#v", index, usage.QueryCosts[index], wantCost)
		}
	}
}

func TestClientGraphQLSerializesHeaderInferredMutationCosts(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(16)
	defer runtime.GOMAXPROCS(oldProcs)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	client := &Client{}
	client.setRateLimit(connector.GraphQLRateLimit{
		Limit:     5000,
		Used:      10,
		Remaining: 4990,
		ResetAt:   resetAt,
		UpdatedAt: now,
	})

	const mutations = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 1; i <= mutations; i++ {
		used := int64(10 + i)
		remaining := int64(5000) - used
		wg.Go(func() {
			<-start
			headers := http.Header{}
			headers.Set("X-RateLimit-Limit", "5000")
			headers.Set("X-RateLimit-Used", strconv.FormatInt(used, 10))
			headers.Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
			headers.Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

			snapshot := client.recordRateLimitFromHeaders(headers, now)
			client.recordGraphQLQueryCostFromHeaders("mutation", snapshot)
		})
	}
	close(start)
	wg.Wait()

	usage := client.FlushGraphQLRateLimitUsage()
	if usage.TotalCost != mutations {
		t.Fatalf("FlushGraphQLRateLimitUsage().TotalCost = %d, want %d", usage.TotalCost, mutations)
	}
	if len(usage.QueryCosts) != 1 {
		t.Fatalf("QueryCosts len = %d, want 1: %#v", len(usage.QueryCosts), usage.QueryCosts)
	}
	if usage.QueryCosts[0].QueryType != "mutation" || usage.QueryCosts[0].Cost != mutations {
		t.Fatalf("QueryCosts[0] = %#v, want mutation cost %d", usage.QueryCosts[0], mutations)
	}
	if usage.QueryCosts[0].Count != mutations {
		t.Fatalf("QueryCosts[0].Count = %d, want %d", usage.QueryCosts[0].Count, mutations)
	}
	if usage.RateLimit.Used != 10+mutations || usage.RateLimit.Remaining != 5000-10-mutations {
		t.Fatalf("RateLimit = %#v, want latest used %d remaining %d", usage.RateLimit, 10+mutations, 5000-10-mutations)
	}
}

func TestClientGraphQLCountsStaleHeaderInferredMutation(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	client := &Client{}
	client.setRateLimit(connector.GraphQLRateLimit{
		Limit:     5000,
		Used:      15,
		Remaining: 4985,
		ResetAt:   resetAt,
		UpdatedAt: now,
	})

	headers := http.Header{}
	headers.Set("X-RateLimit-Limit", "5000")
	headers.Set("X-RateLimit-Used", "13")
	headers.Set("X-RateLimit-Remaining", "4987")
	headers.Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

	snapshot := client.recordRateLimitFromHeaders(headers, now.Add(time.Second))
	client.recordGraphQLQueryCostFromHeaders("mutation", snapshot)

	usage := client.FlushGraphQLRateLimitUsage()
	if usage.TotalQueries != 1 || usage.TotalCost != 0 {
		t.Fatalf("FlushGraphQLRateLimitUsage() totals = queries %d cost %d, want queries 1 cost 0", usage.TotalQueries, usage.TotalCost)
	}
	if len(usage.QueryCosts) != 1 {
		t.Fatalf("QueryCosts len = %d, want 1: %#v", len(usage.QueryCosts), usage.QueryCosts)
	}
	if usage.QueryCosts[0] != (connector.GraphQLQueryCost{QueryType: "mutation", Count: 1, Cost: 0}) {
		t.Fatalf("QueryCosts[0] = %#v, want mutation count 1 cost 0", usage.QueryCosts[0])
	}
	if usage.RateLimit.Used != 15 || usage.RateLimit.Remaining != 4985 {
		t.Fatalf("RateLimit = %#v, want previous snapshot preserved", usage.RateLimit)
	}
}

func TestClientGraphQLCapturesRetryAfterRateLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"secondary rate limit"}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = client.GraphQL(context.Background(), "query { viewer { login } }", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("GraphQL() error = %v, want ErrRateLimited", err)
	}

	rateLimit, ok := client.GraphQLRateLimit()
	if !ok {
		t.Fatal("GraphQLRateLimit() ok = false, want true")
	}
	if rateLimit.RetryAfter != 2*time.Minute || rateLimit.Remaining != 0 || rateLimit.Limit != 5000 {
		t.Fatalf("GraphQLRateLimit() = %#v, want retry-after 2m and exhausted headers", rateLimit)
	}
}

func TestClientGraphQLClearsRetryAfterOnHeaderRefresh(t *testing.T) {
	t.Parallel()

	responses := make(chan func(http.ResponseWriter), 2)
	responses <- func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "120")
		w.Header().Set("X-RateLimit-Remaining", "4880")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"secondary rate limit"}`))
	}
	responses <- func(w http.ResponseWriter) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "121")
		w.Header().Set("X-RateLimit-Remaining", "4879")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handle := <-responses
		handle(w)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = client.GraphQL(context.Background(), "query { viewer { login } }", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("GraphQL() error = %v, want ErrRateLimited", err)
	}
	rateLimit, ok := client.GraphQLRateLimit()
	if !ok {
		t.Fatal("GraphQLRateLimit() ok = false, want true")
	}
	if rateLimit.RetryAfter != 2*time.Minute {
		t.Fatalf("GraphQLRateLimit().RetryAfter = %s, want 2m", rateLimit.RetryAfter)
	}

	err = client.GraphQL(context.Background(), "query { viewer { login } }", nil, nil)
	if err != nil {
		t.Fatalf("GraphQL() error = %v", err)
	}

	rateLimit, ok = client.GraphQLRateLimit()
	if !ok {
		t.Fatal("GraphQLRateLimit() ok = false, want true")
	}
	if rateLimit.RetryAfter != 0 {
		t.Fatalf("GraphQLRateLimit().RetryAfter = %s, want cleared", rateLimit.RetryAfter)
	}
	if rateLimit.Remaining != 4879 || rateLimit.Used != 121 || rateLimit.Limit != 5000 {
		t.Fatalf("GraphQLRateLimit() = %#v, want refreshed primary headers", rateLimit)
	}
}

func TestClientGraphQLRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid json", body: `{`},
		{name: "missing data", body: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			client, err := NewClient(ClientConfig{
				Endpoint:    server.URL,
				TokenSource: StaticTokenSource("test-token"),
				HTTPClient:  server.Client(),
			})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			var out map[string]any
			err = client.GraphQL(context.Background(), "query { viewer { login } }", nil, &out)
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("GraphQL() error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}
