package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestClientRESTConditionalRequestUsesCachedResponseBelowReserve(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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

	scope := &connector.RESTScope{Name: "refresh"}
	ctx := connector.WithRESTScope(t.Context(), scope)
	path := "/repos/digitaldrywood/detent/issues?state=open"
	var first []restIssue
	if err := client.REST(ctx, http.MethodGet, path, nil, &first); err != nil {
		t.Fatalf("first REST() error = %v", err)
	}
	client.FlushRESTRateLimitUsage()

	var second []restIssue
	if err := client.REST(ctx, http.MethodGet, path, nil, &second); err != nil {
		t.Fatalf("conditional REST() error = %v", err)
	}
	if len(second) != 1 || second[0].Number != 1133 || second[0].Title != "Fresh board" {
		t.Fatalf("conditional REST() response = %#v, want cached issue", second)
	}

	assertScopeTiming(t, scope, "http_transport", "repository issues", "200", 1)
	assertScopeTiming(t, scope, "http_transport", "repository issues", "304", 1)
	assertScopeTiming(t, scope, "token_resolution_inclusive", "repository issues", "200", 2)
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
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
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
		if index == 4 && !errors.Is(err, ErrRESTFanoutDeferred) {
			t.Fatalf("conditional REST() request 5 error = %v, want ErrRESTFanoutDeferred", err)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("REST calls = %d, want four quarter-cost conditional requests", calls.Load())
	}
}

func TestClientRESTCachedPullRequestFleetFitsDefaultFanoutCap(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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

func TestClientRESTConditionalEvictionUsesSuccessfulReads(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name          string
		readFirst     bool
		noOutput      bool
		invalidOutput bool
		wantEvicted   string
	}{
		{name: "oldest entry is evicted", wantEvicted: "/resource/0"},
		{name: "304 keeps a recently read entry", readFirst: true, wantEvicted: "/resource/1"},
		{name: "304 without output keeps a recently read entry", readFirst: true, noOutput: true, wantEvicted: "/resource/1"},
		{name: "failed decode does not change recency", readFirst: true, invalidOutput: true, wantEvicted: "/resource/0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"value"`)
				if r.Header.Get("If-None-Match") == `"value"` {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				_, _ = w.Write([]byte(`{"lane":"Todo","pull":"fixture/repo#2","blocked_by":"fixture/repo#1"}`))
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(ClientConfig{Endpoint: server.URL, TokenSource: StaticTokenSource("fixture"), HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			headers := http.Header{"Etag": []string{`"value"`}}
			for index := range restConditionalCacheMaxEntries {
				path := "/resource/" + strconv.Itoa(index)
				client.storeRESTConditionalEntry(http.MethodGet, path, headers, []byte(`{"lane":"Todo","pull":"fixture/repo#2","blocked_by":"fixture/repo#1"}`))
				// Fix the initial ordering without depending on clock resolution.
				key := restCacheKey(http.MethodGet, path)
				entry := client.restCache[key]
				entry.lastUsedAt = time.Unix(int64(index), 0)
				client.restCache[key] = entry
			}
			if test.readFirst {
				var evidence struct {
					Lane string `json:"lane"`
				}
				var out any = &evidence
				if test.noOutput {
					out = nil
				} else if test.invalidOutput {
					out = new([]string)
				}
				err := client.REST(t.Context(), http.MethodGet, "/resource/0", nil, out)
				if test.invalidOutput {
					if !errors.Is(err, ErrInvalidResponse) {
						t.Fatalf("304 decode error = %v, want ErrInvalidResponse", err)
					}
				} else if err != nil || !test.noOutput && evidence.Lane != "Todo" {
					t.Fatalf("304 evidence = %+v, error = %v", evidence, err)
				}
			}
			client.storeRESTConditionalEntry(http.MethodGet, "/resource/new", headers, []byte(`{}`))
			if _, ok := client.restConditionalEntry(http.MethodGet, test.wantEvicted); ok {
				t.Fatalf("%s remained cached", test.wantEvicted)
			}
			if got := len(client.restCache); got != restConditionalCacheMaxEntries {
				t.Fatalf("cache size = %d, want %d", got, restConditionalCacheMaxEntries)
			}
			var evidence struct {
				Lane      string `json:"lane"`
				Pull      string `json:"pull"`
				BlockedBy string `json:"blocked_by"`
			}
			if err := client.REST(t.Context(), http.MethodGet, test.wantEvicted, nil, &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Lane != "Todo" || evidence.Pull != "fixture/repo#2" || evidence.BlockedBy != "fixture/repo#1" {
				t.Fatalf("evidence after eviction = %+v", evidence)
			}
		})
	}
}

func TestClientDeleteRESTConditionalEntriesForEndpointRemovesEveryPage(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientConfig{
		Endpoint:    "https://api.github.test/graphql",
		TokenSource: StaticTokenSource("test-token"),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	headers := http.Header{"Etag": []string{`"value"`}}
	endpoint := "/repos/example/repo/commits/head-sha/check-runs"
	paths := []string{
		endpoint,
		endpoint + "?per_page=100",
		endpoint + "?per_page=100&page=2",
		endpoint + "?page=3&per_page=100",
	}
	for _, path := range paths {
		client.storeRESTConditionalEntry(http.MethodGet, path, headers, []byte(`{}`))
	}
	unrelated := "/repos/example/repo/commits/other-sha/check-runs?per_page=100"
	client.storeRESTConditionalEntry(http.MethodGet, unrelated, headers, []byte(`{}`))

	client.deleteRESTConditionalEntriesForEndpoint(http.MethodGet, endpoint+"?per_page=100")

	for _, path := range paths {
		if _, ok := client.restConditionalEntry(http.MethodGet, path); ok {
			t.Fatalf("conditional entry %q still exists", path)
		}
	}
	if _, ok := client.restConditionalEntry(http.MethodGet, unrelated); !ok {
		t.Fatalf("unrelated conditional entry %q was removed", unrelated)
	}
}
