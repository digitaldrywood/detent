package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2026-03-10" {
			t.Fatalf("X-GitHub-Api-Version = %q, want 2026-03-10", got)
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

func TestClientGraphQLRefreshesTokenAfterAuthFailure(t *testing.T) {
	t.Parallel()

	source := newRefreshingTokenTestSource("stale-token", "fresh-token")
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		requests <- token
		switch token {
		case "Bearer stale-token":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		case "Bearer fresh-token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
		default:
			t.Fatalf("Authorization = %q, want stale then fresh token", token)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: source,
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
	if err := client.GraphQL(context.Background(), "query { viewer { login } }", nil, &got); err != nil {
		t.Fatalf("GraphQL() error = %v", err)
	}
	if got.Viewer.Login != "octocat" {
		t.Fatalf("Viewer.Login = %q, want octocat", got.Viewer.Login)
	}
	if source.refreshes.Load() != 1 {
		t.Fatalf("RefreshToken() calls = %d, want 1", source.refreshes.Load())
	}
	if first, second := <-requests, <-requests; first != "Bearer stale-token" || second != "Bearer fresh-token" {
		t.Fatalf("Authorization sequence = %q, %q; want stale then fresh", first, second)
	}
	health, ok := client.AuthHealth()
	if !ok {
		t.Fatal("AuthHealth() ok = false, want true")
	}
	if health.Status != connector.AuthStatusRecovered {
		t.Fatalf("AuthHealth().Status = %q, want %q", health.Status, connector.AuthStatusRecovered)
	}
	if health.LastError == "" || health.LastErrorAt.IsZero() || health.LastRecoveredAt.IsZero() {
		t.Fatalf("AuthHealth() missing recovery detail: %#v", health)
	}
}

func TestClientRESTRefreshesTokenAfterAuthFailure(t *testing.T) {
	t.Parallel()

	source := newRefreshingTokenTestSource("stale-token", "fresh-token")
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		requests <- token
		switch token {
		case "Bearer stale-token":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		case "Bearer fresh-token":
			if r.URL.Path != "/repos/digitaldrywood/detent/issues" {
				t.Fatalf("path = %s, want REST request path", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("Authorization = %q, want stale then fresh token", token)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: source,
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	var got struct {
		OK bool `json:"ok"`
	}
	if err := client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/issues", nil, &got); err != nil {
		t.Fatalf("REST() error = %v", err)
	}
	if !got.OK {
		t.Fatal("REST() response OK = false, want true")
	}
	if source.refreshes.Load() != 1 {
		t.Fatalf("RefreshToken() calls = %d, want 1", source.refreshes.Load())
	}
	if first, second := <-requests, <-requests; first != "Bearer stale-token" || second != "Bearer fresh-token" {
		t.Fatalf("Authorization sequence = %q, %q; want stale then fresh", first, second)
	}
}

func TestClientRESTDebugLogsEndpointPurposeWithoutSecrets(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/digitaldrywood/detent/commits/abc/check-runs" {
			t.Fatalf("path = %s, want check-runs path", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer super-secret-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"check_runs":[],"body_secret":"do-not-log-response-body"}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("super-secret-token"),
		HTTPClient:  server.Client(),
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/commits/abc/check-runs", nil, nil); err != nil {
		t.Fatalf("REST() error = %v", err)
	}

	logText := logs.String()
	for _, fragment := range []string{
		"github rest request",
		"github rest response",
		`endpoint_family="check runs"`,
		"request_purpose=hydrate_pull_request_checks",
		"body_present=false",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs missing %q:\n%s", fragment, logText)
		}
	}
	for _, leaked := range []string{"super-secret-token", "do-not-log-response-body"} {
		if strings.Contains(logText, leaked) {
			t.Fatalf("logs leaked %q:\n%s", leaked, logText)
		}
	}
}

func TestClientRESTAggregatesUsageAndBacksOffAfterRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch call := calls.Add(1); call {
		case 1:
			if r.URL.Path != "/repos/digitaldrywood/detent/issues" {
				t.Fatalf("first path = %s, want label issues path", r.URL.Path)
			}
			if got := r.URL.Query().Get("labels"); got != "detent:todo" {
				t.Fatalf("labels query = %q, want detent:todo", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Used", "121")
			w.Header().Set("X-RateLimit-Remaining", "4879")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
			w.Header().Set("X-RateLimit-Resource", "core")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case 2:
			if r.URL.Path != "/repos/digitaldrywood/detent/issues/666/comments" {
				t.Fatalf("second path = %s, want issue comments path", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "120")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Used", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
			w.Header().Set("X-RateLimit-Resource", "core")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"secondary rate limit"}`))
		default:
			t.Fatalf("unexpected REST call %d to %s", call, r.URL.Path)
		}
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

	var issues []restIssue
	if err := client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/issues?labels=detent%3Atodo", nil, &issues); err != nil {
		t.Fatalf("REST() label issues error = %v", err)
	}
	var comments []restComment
	err = client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/issues/666/comments", nil, &comments)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("REST() comments error = %v, want ErrRateLimited", err)
	}
	err = client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/commits/abc/check-runs", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("REST() during backoff error = %v, want ErrRateLimited", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("REST calls = %d, want 2 before local backoff", calls.Load())
	}

	usage := client.FlushRESTRateLimitUsage()
	if !usage.HasRateLimit {
		t.Fatal("FlushRESTRateLimitUsage().HasRateLimit = false, want true")
	}
	if usage.TotalRequests != 2 || !usage.RateLimited {
		t.Fatalf("FlushRESTRateLimitUsage() totals = requests %d rate_limited %v, want 2 true", usage.TotalRequests, usage.RateLimited)
	}
	if usage.RateLimit.Remaining != 0 || usage.RateLimit.RetryAfter != 2*time.Minute || usage.RateLimit.Resource != "core" {
		t.Fatalf("FlushRESTRateLimitUsage().RateLimit = %#v, want remaining 0 retry-after 2m core", usage.RateLimit)
	}
	if usage.BackoffUntil.IsZero() {
		t.Fatal("FlushRESTRateLimitUsage().BackoffUntil is zero, want backoff deadline")
	}
	if got := restEndpointUsageCount(usage.Requests, "label issues"); got != 1 {
		t.Fatalf("label issues usage count = %d, want 1; usage = %#v", got, usage.Requests)
	}
	if got := restEndpointUsageCount(usage.Requests, "issue comments"); got != 1 {
		t.Fatalf("issue comments usage count = %d, want 1; usage = %#v", got, usage.Requests)
	}
}

func TestClientRESTDoesNotGloballyBackOffAfterSecondaryFanoutThrottle(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch call := calls.Add(1); call {
		case 1:
			if r.URL.Path != "/repos/digitaldrywood/detent/commits/abc/check-runs" {
				t.Fatalf("first path = %s, want check-runs path", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "120")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Used", "122")
			w.Header().Set("X-RateLimit-Remaining", "4878")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
			w.Header().Set("X-RateLimit-Resource", "core")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"secondary rate limit"}`))
		case 2:
			if r.URL.Path != "/repos/digitaldrywood/detent/issues" {
				t.Fatalf("second path = %s, want label issues path", r.URL.Path)
			}
			if got := r.URL.Query().Get("labels"); got != "detent:todo" {
				t.Fatalf("labels query = %q, want detent:todo", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Used", "123")
			w.Header().Set("X-RateLimit-Remaining", "4877")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
			w.Header().Set("X-RateLimit-Resource", "core")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected REST call %d to %s", call, r.URL.Path)
		}
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

	err = client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/commits/abc/check-runs", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("REST() check-runs error = %v, want ErrRateLimited", err)
	}
	var issues []restIssue
	if err := client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/issues?labels=detent%3Atodo", nil, &issues); err != nil {
		t.Fatalf("REST() label issues after secondary throttle error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("REST calls = %d, want secondary throttle not to block label issues", calls.Load())
	}

	usage := client.FlushRESTRateLimitUsage()
	if usage.RateLimit.RetryAfter != 0 {
		t.Fatalf("RateLimit.RetryAfter = %s, want no global REST retry-after", usage.RateLimit.RetryAfter)
	}
	if !usage.BackoffUntil.IsZero() {
		t.Fatalf("BackoffUntil = %v, want no global REST backoff", usage.BackoffUntil)
	}
	checkRuns := restEndpointUsage(usage.Requests, "check runs")
	if !checkRuns.RateLimited || checkRuns.RetryAfter != 2*time.Minute {
		t.Fatalf("check runs usage = %#v, want endpoint retry-after", checkRuns)
	}
}

func TestClientRESTStopsFanoutAtRequestCap(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Fatalf("unexpected REST call to %s", r.URL.Path)
		}
		if r.URL.Path != "/repos/digitaldrywood/detent/pulls" {
			t.Fatalf("path = %s, want pull requests path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "100")
		w.Header().Set("X-RateLimit-Remaining", "4900")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
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

	var pulls []restPullRequest
	if err := client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/pulls?state=all", nil, &pulls); err != nil {
		t.Fatalf("REST() pull requests error = %v", err)
	}
	err = client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/commits/abc/check-runs", nil, nil)
	if !errors.Is(err, ErrRESTBudgetReserved) {
		t.Fatalf("REST() capped fanout error = %v, want ErrRESTBudgetReserved", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("REST calls = %d, want only first request sent", calls.Load())
	}

	usage := client.FlushRESTRateLimitUsage()
	if usage.TotalRequests != 2 || !usage.RateLimited {
		t.Fatalf("FlushRESTRateLimitUsage() totals = requests %d rate_limited %v, want 2 true", usage.TotalRequests, usage.RateLimited)
	}
	if got := restEndpointUsageCount(usage.Requests, "pull requests"); got != 1 {
		t.Fatalf("pull requests usage count = %d, want 1; usage = %#v", got, usage.Requests)
	}
	if got := restEndpointUsageCount(usage.Requests, "check runs"); got != 1 {
		t.Fatalf("check runs usage count = %d, want throttled synthetic request; usage = %#v", got, usage.Requests)
	}
}

func TestClientRESTStopsFanoutBelowReserve(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Fatalf("unexpected REST call to %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "4100")
		w.Header().Set("X-RateLimit-Remaining", "900")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
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

	var pulls []restPullRequest
	if err := client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/pulls?state=all", nil, &pulls); err != nil {
		t.Fatalf("REST() pull requests error = %v", err)
	}
	err = client.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/commits/abc/statuses", nil, nil)
	if !errors.Is(err, ErrRESTBudgetReserved) {
		t.Fatalf("REST() reserve fanout error = %v, want ErrRESTBudgetReserved", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("REST calls = %d, want reserve to stop second request", calls.Load())
	}

	usage := client.FlushRESTRateLimitUsage()
	if usage.RateLimit.Remaining != 900 {
		t.Fatalf("RateLimit.Remaining = %d, want 900", usage.RateLimit.Remaining)
	}
	if got := restEndpointUsageCount(usage.Requests, "commit statuses"); got != 1 {
		t.Fatalf("commit statuses usage count = %d, want throttled synthetic request; usage = %#v", got, usage.Requests)
	}
}

func TestClientRESTBackoffAppliesAcrossClientsWithSharedToken(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().UTC().Add(time.Hour)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		w.Header().Set("X-RateLimit-Resource", "core")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"secondary rate limit"}`))
	}))
	t.Cleanup(server.Close)

	clientA, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("shared-rest-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() clientA error = %v", err)
	}
	clientB, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: StaticTokenSource("shared-rest-token"),
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() clientB error = %v", err)
	}

	err = clientA.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/issues/666/comments", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("clientA REST() error = %v, want ErrRateLimited", err)
	}
	err = clientB.REST(context.Background(), http.MethodGet, "/repos/digitaldrywood/detent/issues?labels=detent%3Atodo", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("clientB REST() error = %v, want ErrRateLimited from shared backoff", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("REST calls = %d, want only the first client to hit the server", calls.Load())
	}

	usage := clientB.FlushRESTRateLimitUsage()
	if !usage.BackoffUntil.After(time.Now()) {
		t.Fatalf("clientB BackoffUntil = %v, want shared future deadline", usage.BackoffUntil)
	}
}

func TestClientReportsStaleAuthWhenRefreshFails(t *testing.T) {
	t.Parallel()

	source := newRefreshingTokenTestSource("stale-token", "")
	source.refreshErr = errors.New("gh auth token failed")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint:    server.URL,
		TokenSource: source,
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	err = client.GraphQL(context.Background(), "query { viewer { login } }", nil, nil)
	if !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("GraphQL() error = %v, want ErrAuthenticationFailed", err)
	}
	health, ok := client.AuthHealth()
	if !ok {
		t.Fatal("AuthHealth() ok = false, want true")
	}
	if health.Status != connector.AuthStatusStale {
		t.Fatalf("AuthHealth().Status = %q, want %q", health.Status, connector.AuthStatusStale)
	}
	if health.LastError == "" || health.LastErrorAt.IsZero() {
		t.Fatalf("AuthHealth() missing stale auth detail: %#v", health)
	}
	if health.LastRecoveredAt.IsZero() == false {
		t.Fatalf("AuthHealth().LastRecoveredAt = %v, want zero", health.LastRecoveredAt)
	}
}

func restEndpointUsageCount(usages []connector.RESTEndpointUsage, family string) int64 {
	for _, usage := range usages {
		if usage.EndpointFamily == family {
			return usage.Count
		}
	}
	return 0
}

func restEndpointUsage(usages []connector.RESTEndpointUsage, family string) connector.RESTEndpointUsage {
	for _, usage := range usages {
		if usage.EndpointFamily == family {
			return usage
		}
	}
	return connector.RESTEndpointUsage{}
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

func TestClientGraphQLRecordsRateLimitStatusWithoutSnapshot(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
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

	err = client.GraphQLWithType(context.Background(), "issue_parent_metadata", "query { viewer { login } }", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("GraphQLWithType() error = %v, want ErrRateLimited", err)
	}

	usage := client.FlushGraphQLRateLimitUsage()
	if usage.HasRateLimit {
		t.Fatalf("FlushGraphQLRateLimitUsage().HasRateLimit = true, want false with no snapshot")
	}
	if usage.RateLimitStatus != connector.GraphQLRateLimitStatusExhausted {
		t.Fatalf("FlushGraphQLRateLimitUsage().RateLimitStatus = %q, want %q", usage.RateLimitStatus, connector.GraphQLRateLimitStatusExhausted)
	}

	usage = client.FlushGraphQLRateLimitUsage()
	if usage.RateLimitStatus != "" {
		t.Fatalf("second FlushGraphQLRateLimitUsage().RateLimitStatus = %q, want cleared", usage.RateLimitStatus)
	}
}

func TestClientGraphQLRateLimitFailureDoesNotPublishStaleSnapshot(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	responses := make(chan string, 2)
	responses <- `{"data":{"rateLimit":{"limit":5000,"used":120,"remaining":4880,"cost":2,"resetAt":"` + resetAt.Format(time.RFC3339) + `"}}}`
	responses <- `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`
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
		t.Fatalf("GraphQLWithType() healthy query error = %v", err)
	}
	usage := client.FlushGraphQLRateLimitUsage()
	if !usage.HasRateLimit || usage.RateLimit.Remaining != 4880 {
		t.Fatalf("FlushGraphQLRateLimitUsage() = %#v, want healthy current bucket", usage)
	}

	err = client.GraphQLWithType(context.Background(), "issue_parent_metadata", "query { viewer { login } }", nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("GraphQLWithType() error = %v, want ErrRateLimited", err)
	}
	usage = client.FlushGraphQLRateLimitUsage()
	if usage.HasRateLimit {
		t.Fatalf("FlushGraphQLRateLimitUsage().HasRateLimit = true, want false after failure with no fresh bucket: %#v", usage)
	}
	if usage.RateLimit != (connector.GraphQLRateLimit{}) {
		t.Fatalf("FlushGraphQLRateLimitUsage().RateLimit = %#v, want zero stale bucket", usage.RateLimit)
	}
	if usage.RateLimitStatus != connector.GraphQLRateLimitStatusExhausted {
		t.Fatalf("FlushGraphQLRateLimitUsage().RateLimitStatus = %q, want %q", usage.RateLimitStatus, connector.GraphQLRateLimitStatusExhausted)
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

type refreshingTokenTestSource struct {
	mu           sync.Mutex
	token        string
	refreshToken string
	refreshErr   error
	refreshes    atomic.Int64
}

func newRefreshingTokenTestSource(token string, refreshToken string) *refreshingTokenTestSource {
	return &refreshingTokenTestSource{token: token, refreshToken: refreshToken}
}

func (s *refreshingTokenTestSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, nil
}

func (s *refreshingTokenTestSource) RefreshToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.refreshes.Add(1)
	if s.refreshErr != nil {
		return "", s.refreshErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = s.refreshToken
	return s.token, nil
}
