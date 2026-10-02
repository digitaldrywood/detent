package github

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type rotatingAttributionTokenSource struct {
	token atomic.Value
}

func (s *rotatingAttributionTokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.token.Load().(string), nil
}

func (*rotatingAttributionTokenSource) CredentialIdentity(string) string {
	return "github-app-installation:4242"
}

func TestGraphQLAttributionCombinesProjectsByCredentialAndReconciles(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	registry := &graphQLAttributionRegistry{}
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	for _, query := range []struct {
		project, kind string
		used, cost    int64
	}{
		{"project-a", "candidate_issues", 102, 2},
		{"project-b", "lane_coordination", 105, 3},
		{"project-a", "merge_queue", 108, 3},
	} {
		registry.record(t.Context(), "secret-token", query.project, query.kind, query.cost, graphQLCostSnapshot{Used: query.used, Cost: query.cost, ResetAt: reset, Measured: true}, logger, nil)
	}
	registry.mu.Lock()
	var fingerprint string
	for fingerprint = range registry.windows {
		break
	}
	registry.mu.Unlock()
	registry.finish(fingerprint, reset)
	registry.finish(fingerprint, reset)
	if strings.Count(output.String(), "github graphql hourly attribution") != 1 {
		t.Fatalf("log lines = %q", output.String())
	}
	if strings.Contains(output.String(), "secret-token") {
		t.Fatal("log contains credential")
	}
	var line struct {
		QueryCount         int64                     `json:"query_count"`
		AttributedCost     int64                     `json:"attributed_cost"`
		RateLimitUsedDelta int64                     `json:"rate_limit_used_delta"`
		UnattributedCost   int64                     `json:"unattributed_cost"`
		ByProjectPurpose   []graphQLAttributionEntry `json:"by_project_purpose"`
	}
	if err := json.Unmarshal(output.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line.QueryCount != 3 || line.AttributedCost != 8 || line.RateLimitUsedDelta != 8 || line.UnattributedCost != 0 || len(line.ByProjectPurpose) != 3 {
		t.Fatalf("attribution = %#v", line)
	}
	for _, purpose := range []string{"tracker_fetch", "lane_coordination", "merge"} {
		if !strings.Contains(output.String(), purpose) {
			t.Fatalf("missing purpose %q: %s", purpose, output.String())
		}
	}
}

func TestGraphQLAttributionReportsUnattributedProviderCost(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	registry := &graphQLAttributionRegistry{}
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	reset := time.Now().Add(time.Hour)
	registry.record(t.Context(), "token", "project", "candidate_issues", 2, graphQLCostSnapshot{Used: 102, Cost: 2, ResetAt: reset, Measured: true}, logger, nil)
	registry.record(t.Context(), "token", "project", "candidate_issues", 2, graphQLCostSnapshot{Used: 120, Cost: 2, ResetAt: reset, Measured: true}, logger, nil)
	registry.mu.Lock()
	var fingerprint string
	for fingerprint = range registry.windows {
		break
	}
	registry.mu.Unlock()
	registry.finish(fingerprint, reset)
	var line struct {
		AttributedCost     int64 `json:"attributed_cost"`
		RateLimitUsedDelta int64 `json:"rate_limit_used_delta"`
		UnattributedCost   int64 `json:"unattributed_cost"`
	}
	if err := json.Unmarshal(output.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line.AttributedCost != 4 || line.RateLimitUsedDelta != 20 || line.UnattributedCost != 16 {
		t.Fatalf("reconciliation = %#v", line)
	}
}

func TestGraphQLAttributionReconcilesWithRateLimitEndpoint(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate_limit" {
			t.Errorf("path = %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{"graphql": map[string]any{"used": 108, "reset": reset.Unix()}}})
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL + "/graphql", Project: "project", TokenSource: StaticTokenSource("secret"), HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	registry := &graphQLAttributionRegistry{}
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	registry.record(t.Context(), "secret", "project", "candidate_issues", 2, graphQLCostSnapshot{Used: 102, Cost: 2, ResetAt: reset, Measured: true}, logger, client)
	registry.mu.Lock()
	var fingerprint string
	for fingerprint = range registry.windows {
		break
	}
	registry.mu.Unlock()
	registry.probe(t.Context(), fingerprint, reset)
	registry.finish(fingerprint, reset)
	if requests.Load() != 1 {
		t.Fatalf("/rate_limit requests = %d, want 1", requests.Load())
	}
	var line struct {
		RateLimitSource    string `json:"rate_limit_source"`
		RateLimitUsedDelta int64  `json:"rate_limit_used_delta"`
		UnattributedCost   int64  `json:"unattributed_cost"`
	}
	if err := json.Unmarshal(output.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line.RateLimitSource != "rest" || line.RateLimitUsedDelta != 8 || line.UnattributedCost != 6 {
		t.Fatalf("reconciliation = %#v", line)
	}
}

func TestGraphQLAttributionKeepsOneWindowAcrossInstallationTokenRotation(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate_limit" || r.Header.Get("Authorization") != "Bearer rotated-secret" {
			t.Errorf("probe path = %q, authorization = %q", r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{"graphql": map[string]any{"used": 108, "reset": reset.Unix()}}})
	}))
	defer server.Close()
	source := &rotatingAttributionTokenSource{}
	source.token.Store("original-secret")
	client, err := NewClient(ClientConfig{Endpoint: server.URL + "/graphql", Project: "project", TokenSource: source, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	registry := &graphQLAttributionRegistry{}
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	registry.record(t.Context(), "original-secret", "project", "candidate_issues", 2, graphQLCostSnapshot{Used: 102, Cost: 2, ResetAt: reset, Measured: true}, logger, client)
	source.token.Store("rotated-secret")
	registry.record(t.Context(), "rotated-secret", "project", "merge_queue", 2, graphQLCostSnapshot{Used: 104, Cost: 2, ResetAt: reset, Measured: true}, logger, client)
	registry.mu.Lock()
	windowCount := len(registry.windows)
	if windowCount != 1 {
		registry.mu.Unlock()
		t.Fatalf("attribution windows = %d, want 1", windowCount)
	}
	var fingerprint string
	for fingerprint = range registry.windows {
		break
	}
	registry.mu.Unlock()
	registry.probe(t.Context(), fingerprint, reset)
	registry.finish(fingerprint, reset)
	if requests.Load() != 1 {
		t.Fatalf("/rate_limit requests = %d, want 1", requests.Load())
	}
	if strings.Count(output.String(), "github graphql hourly attribution") != 1 || strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "github-app-installation") {
		t.Fatalf("attribution log = %q", output.String())
	}
	var line struct {
		QueryCount         int64  `json:"query_count"`
		AttributedCost     int64  `json:"attributed_cost"`
		RateLimitSource    string `json:"rate_limit_source"`
		RateLimitUsedDelta int64  `json:"rate_limit_used_delta"`
	}
	if err := json.Unmarshal(output.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line.QueryCount != 2 || line.AttributedCost != 4 || line.RateLimitSource != "rest" || line.RateLimitUsedDelta != 8 {
		t.Fatalf("attribution = %#v", line)
	}
}

func TestGraphQLAttributionCostUsesResponseAndHeaderSnapshot(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name               string
		data               string
		headers            graphQLHeaderRateLimit
		wantCost, wantUsed int64
	}{
		{"response", `{"rateLimit":{"used":42,"cost":3,"resetAt":"2026-09-23T15:00:00Z"}}`, graphQLHeaderRateLimit{}, 3, 42},
		{"response cost with headers", `{"rateLimit":{"cost":2}}`, graphQLHeaderRateLimit{HasCurrent: true, Current: connectorRateLimitForAttribution(44, reset)}, 2, 44},
		{"headers", `{}`, graphQLHeaderRateLimit{HasCurrent: true, HasPrevious: true, HasPrimarySnapshot: true, Previous: connectorRateLimitForAttribution(44, reset), Current: connectorRateLimitForAttribution(47, reset)}, 3, 47},
		{"first header only", `{}`, graphQLHeaderRateLimit{HasCurrent: true, HasPrimarySnapshot: true, Current: connectorRateLimitForAttribution(47, reset)}, 0, 47},
	} {
		t.Run(test.name, func(t *testing.T) {
			cost, snapshot, ok := graphQLAttributionCost(json.RawMessage(test.data), test.headers)
			if !ok || cost != test.wantCost || snapshot.Used != test.wantUsed || !snapshot.ResetAt.Equal(reset) {
				t.Fatalf("cost = %d, snapshot = %#v, ok = %t", cost, snapshot, ok)
			}
		})
	}
}

func connectorRateLimitForAttribution(used int64, reset time.Time) connector.GraphQLRateLimit {
	return connector.GraphQLRateLimit{Used: used, ResetAt: reset}
}
