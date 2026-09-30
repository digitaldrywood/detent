package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
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
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := &connector.RESTScope{Name: "refresh"}
			ctx := connector.WithRESTScope(t.Context(), scope)
			var response *http.Response
			if tt.status != 0 {
				response = &http.Response{StatusCode: tt.status, Header: tt.header}
			}
			(&Client{}).recordRESTScopeOutcome(ctx, http.MethodGet, "/repos/a/b/issues/3/comments", response, tt.err)
			got := scope.Counts()
			if len(got) != 1 || got[0].EndpointFamily != "issue comments" || got[0].Outcome != tt.want || got[0].Count != 1 {
				t.Fatalf("Counts() = %#v, want one %s issue comments request", got, tt.want)
			}
		})
	}
}

func TestRESTScopeRequestsExcludeLocalDeferrals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, TokenSource: StaticTokenSource("scope-test-token"),
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
	var logs bytes.Buffer
	connector.LogRESTScope(slog.New(slog.NewTextHandler(&logs, nil)), scope)
	output := logs.String()
	for _, want := range []string{"request_count=1", "fanout_deferred=1", "fetch_candidates", "dispatch_ready_issues"} {
		if !strings.Contains(output, want) {
			t.Errorf("scope log missing %q: %s", want, output)
		}
	}
	if strings.Contains(output, "secret-query-value") {
		t.Fatalf("scope log contains query: %s", output)
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
	usage := client.FlushRESTRateLimitUsage()
	if len(usage.Divergences) != 1 || usage.Divergences[0].ObservedRequests != 1 || usage.Divergences[0].DetentRequests != 1 || usage.Divergences[0].AttributedRequests+usage.Divergences[0].UnattributedRequests != 0 {
		t.Fatalf("quota windows = %#v, want one fully instrumented request", usage.Divergences)
	}
	for _, want := range []string{`msg="github rest quota window"`, "resource=core", "consumed_quota=1", "instrumented_billable_requests=1", "unattributed_remainder=0"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("quota log missing %q: %s", want, logs.String())
		}
	}
}
