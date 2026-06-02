package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
