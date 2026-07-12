package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestClientRESTConditionalRequestUsesCachedResponseBelowReserve(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "4100")
		w.Header().Set("X-RateLimit-Remaining", "900")
		w.Header().Set("X-RateLimit-Resource", "core")
		switch calls.Add(1) {
		case 1:
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Fatalf("first If-None-Match = %q, want empty", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"board-v1"`)
			_, _ = w.Write([]byte(`[{"number":1133,"node_id":"issue-1133","title":"Fresh board"}]`))
		case 2:
			if got := r.Header.Get("If-None-Match"); got != `"board-v1"` {
				t.Fatalf("second If-None-Match = %q, want board-v1 ETag", got)
			}
			w.WriteHeader(http.StatusNotModified)
		default:
			t.Fatalf("unexpected request %d", calls.Load())
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
		RESTPolicy:  RESTBudgetPolicy{MinRemainingReserve: 1000},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	path := "/repos/digitaldrywood/detent/issues?state=open"
	var first []restIssue
	if err := client.REST(context.Background(), http.MethodGet, path, nil, &first); err != nil {
		t.Fatalf("first REST() error = %v", err)
	}
	client.FlushRESTRateLimitUsage()

	var second []restIssue
	if err := client.REST(context.Background(), http.MethodGet, path, nil, &second); err != nil {
		t.Fatalf("conditional REST() error = %v", err)
	}
	if len(second) != 1 || second[0].Number != 1133 || second[0].Title != "Fresh board" {
		t.Fatalf("conditional REST() response = %#v, want cached issue", second)
	}

	usage := client.FlushRESTRateLimitUsage()
	if usage.TotalRequests != 1 || usage.ConditionalRequests != 1 || usage.NotModifiedRequests != 1 || usage.BillableRequests != 0 {
		t.Fatalf("conditional usage = %#v, want one free not-modified request", usage)
	}
	if usage.RateLimit.Used != 4100 || usage.RateLimit.Remaining != 900 {
		t.Fatalf("rate limit = %#v, want unchanged 4100 used and 900 remaining", usage.RateLimit)
	}
}

func TestClientRESTConditionalRequestsReserveConservativeFanoutCost(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("If-None-Match"); got == "" {
			t.Fatal("If-None-Match is empty, want cached validator")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
		RESTPolicy:  RESTBudgetPolicy{FanoutMaxRequests: 1},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	headers := http.Header{"Etag": []string{`"cached"`}}
	for index := range 5 {
		path := "/repos/digitaldrywood/detent/pulls/" + strconv.Itoa(index+1)
		client.storeRESTConditionalEntry(http.MethodGet, path, headers, []byte(`{}`))
		err := client.REST(context.Background(), http.MethodGet, path, nil, nil)
		if index < 4 && err != nil {
			t.Fatalf("conditional REST() request %d error = %v", index+1, err)
		}
		if index == 4 && !errors.Is(err, ErrRESTBudgetReserved) {
			t.Fatalf("conditional REST() request 5 error = %v, want ErrRESTBudgetReserved", err)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("REST calls = %d, want four quarter-cost conditional requests", calls.Load())
	}
}

func TestClientRESTCachedPullRequestFleetFitsDefaultFanoutCap(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("test-token"),
		HTTPClient:  server.Client(),
		RESTPolicy:  RESTBudgetPolicy{FanoutMaxRequests: 80},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	headers := http.Header{"Etag": []string{`"cached"`}}
	for index := range 200 {
		path := "/repos/digitaldrywood/detent/pulls/" + strconv.Itoa(index+1)
		client.storeRESTConditionalEntry(http.MethodGet, path, headers, []byte(`{}`))
		if err := client.REST(context.Background(), http.MethodGet, path, nil, nil); err != nil {
			t.Fatalf("conditional REST() request %d error = %v", index+1, err)
		}
	}
	if calls.Load() != 200 {
		t.Fatalf("REST calls = %d, want 200 cached hydration requests", calls.Load())
	}
	usage := client.FlushRESTRateLimitUsage()
	if usage.BillableRequests != 0 || usage.NotModifiedRequests != 200 {
		t.Fatalf("REST usage = %#v, want 200 free not-modified requests", usage)
	}
}

func TestClientRESTConditionalRequestsCanBeDisabled(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Fatalf("If-None-Match = %q, want empty when disabled", got)
		}
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"board-v1"`)
		_, _ = w.Write([]byte(`[{"number":` + strconv.FormatInt(call, 10) + `}]`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:                   server.URL,
		TokenSource:                StaticTokenSource("test-token"),
		HTTPClient:                 server.Client(),
		DisableConditionalRequests: true,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	for range 2 {
		var issues []restIssue
		if err := client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/issues", nil, &issues); err != nil {
			t.Fatalf("REST() error = %v", err)
		}
	}
	usage := client.FlushRESTRateLimitUsage()
	if usage.TotalRequests != 2 || usage.ConditionalRequests != 0 || usage.NotModifiedRequests != 0 || usage.BillableRequests != 2 {
		t.Fatalf("disabled conditional usage = %#v, want two billable requests", usage)
	}
}

func TestConnectorConditionalPollingEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     string
		disabled   bool
		wantActive bool
	}{
		{name: "project v2", source: GitHubStatusSourceProjectV2, wantActive: true},
		{name: "issue field", source: GitHubStatusSourceIssueField, wantActive: true},
		{name: "label", source: GitHubStatusSourceLabel, wantActive: true},
		{name: "disabled", source: GitHubStatusSourceProjectV2, disabled: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			connector, err := NewConnector(Config{
				Endpoint:                   "https://api.github.test/graphql",
				APIKey:                     "test-token",
				GitHubStatusSource:         test.source,
				DisableConditionalRequests: test.disabled,
			})
			if err != nil {
				t.Fatalf("NewConnector() error = %v", err)
			}
			if got := connector.ConditionalPollingEnabled(); got != test.wantActive {
				t.Fatalf("ConditionalPollingEnabled() = %v, want %v", got, test.wantActive)
			}
		})
	}
}

func TestClientRESTConditionalCacheIsBounded(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientConfig{
		Endpoint:    "https://api.github.test/graphql",
		TokenSource: StaticTokenSource("test-token"),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	headers := http.Header{"Etag": []string{`"value"`}}
	for index := range restConditionalCacheMaxEntries + 1 {
		client.storeRESTConditionalEntry(http.MethodGet, "/resource/"+strconv.Itoa(index), headers, []byte(`{}`))
	}
	if got := len(client.restCache); got != restConditionalCacheMaxEntries {
		t.Fatalf("conditional cache size = %d, want %d", got, restConditionalCacheMaxEntries)
	}
}
