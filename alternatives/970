package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRESTScopeOutcomeClassification(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		header http.Header
		err    error
		want   string
	}{
		{"success", http.StatusOK, nil, nil, "200"},
		{"created", http.StatusCreated, nil, nil, "200"},
		{"not modified", http.StatusNotModified, nil, nil, "304"},
		{"rate limited", http.StatusTooManyRequests, nil, nil, "429"},
		{"forbidden rate limit", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, nil, "429"},
		{"forbidden", http.StatusForbidden, nil, nil, "error"},
		{"server error", http.StatusBadGateway, nil, nil, "error"},
		{"transport error", 0, nil, errors.New("network"), "error"},
		{"canceled", 0, nil, context.Canceled, "error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := &connector.RESTScope{Name: "refresh"}
			ctx := connector.WithRESTScope(t.Context(), scope)
			var calls int
			client, err := NewClient(ClientConfig{Endpoint: "https://scope.test/graphql", TokenSource: StaticTokenSource("scope-token"), HTTPClient: staticHTTPClient{do: func(req *http.Request) (*http.Response, error) {
				calls++
				if tt.err != nil {
					return nil, tt.err
				}
				response := jsonResponse(req, tt.status, `{}`, tt.header)
				return response, nil
			}}})
			if err != nil {
				t.Fatal(err)
			}
			client.restBackoffs = newRESTBackoffRegistry()
			_ = client.REST(ctx, http.MethodGet, "/repos/a/b/issues/3/comments", nil, nil)
			if calls != 1 {
				t.Fatalf("requests = %d, want 1", calls)
			}
			assertScopeTiming(t, scope, "http_transport", "issue comments", tt.want, 1)
			assertScopeTiming(t, scope, "token_resolution_inclusive", "issue comments", "200", 1)
			got := scope.Counts()
			if len(got) != 1 || got[0].EndpointFamily != "issue comments" || got[0].Outcome != tt.want || got[0].Count != 1 {
				t.Fatalf("Counts() = %#v, want one %s issue comments request", got, tt.want)
			}
		})
	}
	for _, variant := range []struct {
		name string
		run  func(context.Context, *Client) error
	}{
		{"REST", func(ctx context.Context, c *Client) error { return c.REST(ctx, http.MethodGet, "/user", nil, nil) }},
		{"GraphQL", func(ctx context.Context, c *Client) error {
			return c.GraphQLWithType(ctx, graphQLQueryCandidateIssues, "query { viewer { login } }", nil, nil)
		}},
		{"REST text", func(ctx context.Context, c *Client) error {
			_, _, err := c.RESTText(ctx, "/user", "text/plain", 3)
			return err
		}},
		{"REST text size", func(ctx context.Context, c *Client) error {
			_, _, _, err := c.RESTTextWithSize(ctx, "/user", "text/plain", 3)
			return err
		}},
		{"REST batch probe", func(ctx context.Context, c *Client) error {
			_, err := c.restProbe(ctx, http.MethodGet, "/user", nil)
			return err
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			t.Run("exclude response bookkeeping", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					scope := &connector.RESTScope{Name: "refresh"}
					ctx := connector.WithProgressReporter(connector.WithRESTScope(t.Context(), scope), func() {
						time.Sleep(7 * time.Second)
					})
					logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
						if attr.Key == slog.MessageKey && strings.HasSuffix(attr.Value.String(), "response") {
							time.Sleep(5 * time.Second)
						}
						return attr
					}}))
					calls := 0
					client, err := NewClient(ClientConfig{Endpoint: "https://segments.test/graphql", TokenSource: StaticTokenSource("segments-token"), Logger: logger, RESTDebugLogging: true, HTTPClient: staticHTTPClient{do: func(_ *http.Request) (*http.Response, error) {
						calls++
						time.Sleep(time.Second)
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &scopeTimingBody{Reader: strings.NewReader(`{"data":{}}`), closeDelay: 3 * time.Second}}, nil
					}}})
					if err != nil {
						t.Fatal(err)
					}
					started := time.Now()
					if err := variant.run(ctx, client); err != nil {
						t.Fatal(err)
					}
					family := restEndpointFamily(http.MethodGet, "/user")
					if variant.name == "GraphQL" {
						family = "graphql"
					}
					timing := assertScopeTiming(t, scope, "http_transport", family, "200", 1)
					if calls != 1 || timing.ElapsedSumNS != int64(4*time.Second) || timing.ElapsedMaxNS != int64(4*time.Second) || time.Since(started) != 16*time.Second {
						t.Fatalf("calls=%d timing=%#v wall=%v, want one 4s HTTP attempt and 16s wall", calls, timing, time.Since(started))
					}
				})
			})
			t.Run("overlapping body and close", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					scope := &connector.RESTScope{Name: "refresh"}
					scope.Set("dispatch", "dispatch_ready_issues")
					ctx := connector.WithRESTScope(t.Context(), scope)
					doGate, readGate, closeGate := make(chan struct{}), make(chan struct{}), make(chan struct{})
					calls := make(chan struct{}, 2)
					client, err := NewClient(ClientConfig{Endpoint: "https://overlap.test/graphql", TokenSource: StaticTokenSource("overlap-token"), HTTPClient: staticHTTPClient{do: func(req *http.Request) (*http.Response, error) {
						calls <- struct{}{}
						<-doGate
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &scopeTimingBody{Reader: strings.NewReader(`{"data":{}}`), readGate: readGate, closeGate: closeGate}}, nil
					}}})
					if err != nil {
						t.Fatal(err)
					}
					completed := make(chan error, 2)
					started := time.Now()
					for range 2 {
						go func() { completed <- variant.run(ctx, client) }()
					}
					synctest.Wait()
					if len(calls) != 2 {
						t.Fatalf("requests = %d, want 2", len(calls))
					}
					scope.Set("later", "later")
					time.Sleep(time.Second)
					close(doGate)
					synctest.Wait()
					time.Sleep(2 * time.Second)
					close(readGate)
					synctest.Wait()
					time.Sleep(time.Second)
					close(closeGate)
					synctest.Wait()
					for range 2 {
						if err := <-completed; err != nil {
							t.Fatal(err)
						}
					}
					wall := time.Since(started)
					family := restEndpointFamily(http.MethodGet, "/user")
					if variant.name == "GraphQL" {
						family = "graphql"
					}
					timing := assertScopeTiming(t, scope, "http_transport", family, "200", 2)
					if timing.Stage != "dispatch" || timing.Step != "dispatch_ready_issues" || timing.ElapsedSumNS != int64(2*wall) || timing.ElapsedMaxNS != int64(wall) {
						t.Fatalf("timing = %#v, known wall = %v", timing, wall)
					}
					if len(scope.Timings()) != 2 {
						t.Fatalf("retained per-request rows: %#v", scope.Timings())
					}
					if variant.name == "GraphQL" && len(scope.Counts()) != 0 {
						t.Fatalf("GraphQL became REST: %#v", scope.Counts())
					}
					for _, item := range scope.Counts() {
						if item.Stage != "dispatch" || item.Step != "dispatch_ready_issues" || item.Count != 2 {
							t.Fatalf("late REST attribution: %#v", item)
						}
					}
				})
			})
			for _, failure := range []struct {
				name                     string
				token                    string
				readErr, closeErr, doErr error
			}{
				{name: "token failure"},
				{name: "body failure", token: "failure-token", readErr: io.ErrUnexpectedEOF},
				{name: "close failure", token: "failure-token", closeErr: io.ErrClosedPipe},
				{name: "canceled", token: "failure-token", doErr: context.Canceled},
			} {
				t.Run(failure.name, func(t *testing.T) {
					scope := &connector.RESTScope{Name: "refresh"}
					calls := 0
					client, err := NewClient(ClientConfig{Endpoint: "https://failure.test/graphql", TokenSource: StaticTokenSource(failure.token), HTTPClient: staticHTTPClient{do: func(req *http.Request) (*http.Response, error) {
						calls++
						if failure.doErr != nil {
							return nil, failure.doErr
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &scopeTimingBody{Reader: strings.NewReader(`{"data":{}}`), readErr: failure.readErr, closeErr: failure.closeErr}}, nil
					}}})
					if err != nil {
						t.Fatal(err)
					}
					err = variant.run(connector.WithRESTScope(t.Context(), scope), client)
					family := restEndpointFamily(http.MethodGet, "/user")
					if variant.name == "GraphQL" {
						family = "graphql"
					}
					if failure.token == "" {
						if !errors.Is(err, ErrMissingToken) || calls != 0 || len(scope.Timings()) != 1 || len(scope.Counts()) != 0 {
							t.Fatalf("token failure: err=%v calls=%d timings=%#v counts=%#v", err, calls, scope.Timings(), scope.Counts())
						}
						assertScopeTiming(t, scope, "token_resolution_inclusive", family, "error", 1)
						return
					}
					if calls != 1 {
						t.Fatalf("calls=%d, want 1", calls)
					}
					if failure.readErr != nil && !errors.Is(err, failure.readErr) || failure.doErr != nil && !errors.Is(err, failure.doErr) {
						t.Fatalf("error = %v", err)
					}
					if failure.closeErr != nil && err != nil {
						t.Fatalf("close error changed contract: %v", err)
					}
					assertScopeTiming(t, scope, "http_transport", family, "error", 1)
					if variant.name != "GraphQL" {
						wantOutcome := "200"
						if failure.doErr != nil {
							wantOutcome = "error"
						}
						counts := scope.Counts()
						if len(counts) != 1 || counts[0].Outcome != wantOutcome || counts[0].Count != 1 {
							t.Fatalf("legacy outcomes changed: %#v", counts)
						}
					}
				})
			}
		})
	}

}

func TestRESTScopeRequestsExcludeLocalDeferrals(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Sensitive-Header", "secret-header-value")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"body":"secret-body-value"}]`))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, TokenSource: StaticTokenSource("secret-token-value"),
		HTTPClient: server.Client(), RESTPolicy: RESTBudgetPolicy{FanoutMaxRequests: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := &connector.RESTScope{ProjectID: "example", RefreshID: "refresh-1", Name: "refresh"}
	ctx := connector.WithRESTScope(connector.WithRESTFanoutBudget(t.Context(), "refresh"), scope)
	scope.Set("tracker_fetch", "fetch_candidates")
	if err := client.REST(ctx, http.MethodGet, "/repos/a/b/issues?secret-query-value", nil, &[]json.RawMessage{}); err != nil {
		t.Fatal(err)
	}
	scope.Set("dispatch", "dispatch_ready_issues")
	if err := client.REST(ctx, http.MethodGet, "/repos/a/b/issues/3/comments", nil, &[]json.RawMessage{}); !errors.Is(err, ErrRESTFanoutDeferred) {
		t.Fatalf("second REST error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
	assertScopeTiming(t, scope, "http_transport", "repository issues", "200", 1)
	assertScopeTiming(t, scope, "token_resolution_inclusive", "issue comments", "200", 1)
	for _, item := range scope.Timings() {
		if item.Boundary == "http_transport" && item.Stage == "dispatch" {
			t.Fatalf("refused HTTP fabricated: %#v", item)
		}
	}
	if err := client.GraphQLWithType(ctx, "secret-purpose-value", "query secret-query-name { private_field }", map[string]any{"private": "secret-variable-value"}, nil); err == nil {
		t.Fatal("invalid GraphQL body unexpectedly decoded")
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	assertScopeTiming(t, scope, "http_transport", "graphql", "200", 1)
	for _, item := range scope.Timings() {
		if item.EndpointFamily == "graphql" && item.QueryPurpose != "graphql" {
			t.Fatalf("unbounded purpose: %#v", item)
		}
	}
	var logs bytes.Buffer
	connector.LogRESTScope(slog.New(slog.NewTextHandler(&logs, nil)), scope)
	output := logs.String()
	for _, want := range []string{"request_count=1", "fanout_deferred=1", "fetch_candidates", "dispatch_ready_issues"} {
		if !strings.Contains(output, want) {
			t.Errorf("scope log missing %q: %s", want, output)
		}
	}
	for _, secret := range []string{"secret-query-value", "secret-header-value", "secret-body-value", "secret-token-value", "secret-purpose-value", "secret-query-name", "secret-variable-value", server.URL} {
		if strings.Contains(output, secret) {
			t.Fatalf("scope log contains %q: %s", secret, output)
		}
	}
}

func TestRESTQuotaWindowReportsFullyInstrumentedChange(t *testing.T) {
	var calls atomic.Int64
	resetAt := time.Now().Add(time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(5000-call, 10))
		w.Header().Set("X-RateLimit-Used", strconv.FormatInt(call, 10))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	client, err := NewClient(ClientConfig{Endpoint: server.URL, TokenSource: StaticTokenSource("quota-window-test-token"), HTTPClient: server.Client(), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := client.REST(t.Context(), http.MethodGet, "/repos/a/b/issues", nil, &[]json.RawMessage{}); err != nil {
			t.Fatal(err)
		}
	}
	assertScopeTiming(t, client.unscopedRESTScope, "http_transport", "repository issues", "200", 2)
	assertScopeTiming(t, client.unscopedRESTScope, "token_resolution_inclusive", "repository issues", "200", 2)
	usage := client.FlushRESTRateLimitUsage()
	if len(client.unscopedRESTScope.Counts()) != 0 || len(client.unscopedRESTScope.Timings()) != 0 {
		t.Fatal("outside_refresh drain retained previous cohort")
	}

	if len(usage.Divergences) != 1 || usage.Divergences[0].ObservedRequests != 1 || usage.Divergences[0].DetentRequests != 1 || usage.Divergences[0].AttributedRequests+usage.Divergences[0].UnattributedRequests != 0 {
		t.Fatalf("quota windows = %#v, want one fully instrumented request", usage.Divergences)
	}
	for _, want := range []string{`msg="github rest quota window"`, "resource=core", "consumed_quota=1", "instrumented_billable_requests=1", "unattributed_remainder=0"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("quota log missing %q: %s", want, logs.String())
		}
	}
	loggedScopes := strings.Count(logs.String(), `msg="github rest scope usage"`)
	client.FlushRESTRateLimitUsage()
	if strings.Count(logs.String(), `msg="github rest scope usage"`) != loggedScopes {
		t.Fatal("empty outside_refresh cohort logged prior values")
	}
	logged := logs.Len()
	_ = client.GraphQLWithType(t.Context(), graphQLQueryCandidateIssues, "query { viewer { login } }", nil, nil)
	if calls.Load() != 3 {
		t.Fatalf("HTTP calls = %d, want 3", calls.Load())
	}
	if len(client.unscopedRESTScope.Counts()) != 0 || len(client.unscopedRESTScope.Timings()) != 2 {
		t.Fatalf("GraphQL cohort: counts=%#v timings=%#v", client.unscopedRESTScope.Counts(), client.unscopedRESTScope.Timings())
	}
	client.FlushRESTRateLimitUsage()
	output := strings.SplitN(logs.String()[logged:], "\n", 2)[0]
	if !strings.Contains(output, "request_count=0") || !strings.Contains(output, "candidate_issues") || strings.Contains(output, "repository issues") {
		t.Fatalf("outside_refresh cohort leaked prior values: %s", output)
	}

}

func assertScopeTiming(t *testing.T, scope *connector.RESTScope, boundary, family, outcome string, count int64) connector.GitHubTiming {
	t.Helper()
	for _, item := range scope.Timings() {
		if item.Boundary == boundary && item.EndpointFamily == family && item.Outcome == outcome {
			if item.AttemptCount != count || item.TimedCount != count || item.ElapsedSumNS < 0 || item.ElapsedMaxNS < 0 || item.ElapsedMaxNS > item.ElapsedSumNS || item.FirstObservedAt.IsZero() || item.LastObservedAt.Before(item.FirstObservedAt) {
				t.Fatalf("invalid timing: %#v, want %d timed attempts", item, count)
			}
			return item
		}
	}
	t.Fatalf("missing %s %s %s timing: %#v", boundary, family, outcome, scope.Timings())
	return connector.GitHubTiming{}
}

type scopeTimingBody struct {
	io.Reader
	readGate   <-chan struct{}
	closeGate  <-chan struct{}
	readErr    error
	closeErr   error
	closeDelay time.Duration
}

func (b *scopeTimingBody) Read(p []byte) (int, error) {
	if b.readGate != nil {
		<-b.readGate
	}
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.Reader.Read(p)
}

func (b *scopeTimingBody) Close() error {
	time.Sleep(b.closeDelay)
	if b.closeGate != nil {
		<-b.closeGate
	}
	return b.closeErr
}
