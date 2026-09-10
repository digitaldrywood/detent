package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRepositoryBranchMergePolicy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		rules           string
		protection      string
		wantQueue       bool
		wantStrict      bool
		wantLimit       int
		status          int
		wantError       bool
		wantUnavailable bool
	}{
		{name: "queue", rules: `[{"type":"merge_queue","parameters":{"max_entries_to_build":5}}]`, wantQueue: true, wantLimit: 5},
		{name: "classic strict", rules: `[]`, protection: `{"strict":true}`, wantStrict: true},
		{name: "ruleset strict", rules: `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true}}]`, wantStrict: true},
		{name: "unprotected", rules: `[]`},
		{name: "plan unavailable", status: 403, rules: `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`, wantUnavailable: true},
		{name: "unavailable rules", status: 500, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/example/repo":
					fmt.Fprint(w, `{"default_branch":"release/stable"}`)
				case "/repos/example/repo/rules/branches/release/stable":
					if tt.status != 0 {
						w.WriteHeader(tt.status)
						fmt.Fprint(w, tt.rules)
						return
					}
					fmt.Fprint(w, tt.rules)
				case "/repos/example/repo/branches/release/stable/protection/required_status_checks":
					if tt.protection == "" {
						w.WriteHeader(http.StatusNotFound)
						fmt.Fprint(w, `{"message":"Not Found"}`)
						return
					}
					fmt.Fprint(w, tt.protection)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.RepositoryStrictMergePolicy(t.Context(), "example/repo")
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v", err)
			}
			if tt.wantError {
				return
			}
			if got.Branch != "release/stable" || got.MergeQueue != tt.wantQueue || got.Strict != tt.wantStrict || got.AdmissionLimit != tt.wantLimit || got.RulesUnavailableOnPlan != tt.wantUnavailable {
				t.Fatalf("policy = %+v", got)
			}
		})
	}
}

func TestRefreshMergeQueuePolicyTracksRuleChanges(t *testing.T) {
	t.Parallel()
	var enabled atomic.Bool
	enabled.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/example/repo":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case "/repos/example/repo/rules/branches/main":
			if enabled.Load() {
				fmt.Fprint(w, `[{"type":"merge_queue","parameters":{"max_entries_to_build":4}}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case "/graphql":
			fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","headRefOid":"head","baseRefName":"main"}}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		enabled.Store(want)
		if err := c.RefreshMergeQueuePolicy(t.Context()); err != nil {
			t.Fatal(err)
		}
		number := 42
		got, err := c.InspectPullRequestMergeQueue(t.Context(), connector.Issue{PRRepository: "example/repo", PRNumber: &number})
		if err != nil || got.Available != want {
			t.Fatalf("status=%+v error=%v", got, err)
		}
	}
}

func TestInspectMergeQueueScopesPolicyToPullRequestTarget(t *testing.T) {
	t.Parallel()
	targets := []struct {
		repository, branch string
		queued             bool
	}{
		{repository: "example/delivery", branch: "release", queued: true},
		{repository: "example/delivery", branch: "main"},
		{repository: "example/other", branch: "release"},
	}
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/graphql" {
			var request struct {
				Variables struct {
					Number int `json:"number"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			target := targets[request.Variables.Number-1]
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"id":"PR","headRefOid":"head","baseRefName":%q,"mergeQueue":null,"mergeQueueEntry":null}}}}`, target.branch)
			return
		}
		reads.Add(1)
		switch r.URL.Path {
		case "/repos/example/delivery/rules/branches/release":
			fmt.Fprint(w, `[{"type":"merge_queue","parameters":{"max_entries_to_build":4}}]`)
		case "/repos/example/delivery/rules/branches/main", "/repos/example/other/rules/branches/release":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected policy read %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/tracker", GitHubStatusSource: GitHubStatusSourceLabel})
	if err != nil {
		t.Fatal(err)
	}
	for index, target := range targets {
		t.Run(target.repository+"@"+target.branch, func(t *testing.T) {
			number := index + 1
			for range 2 {
				got, err := c.InspectPullRequestMergeQueue(t.Context(), connector.Issue{PRRepository: target.repository, PRNumber: &number})
				if err != nil || got.Available != target.queued {
					t.Fatalf("status=%+v error=%v", got, err)
				}
			}
		})
	}
	if reads.Load() != int64(len(targets)) {
		t.Fatalf("policy reads=%d, want one per repository/branch", reads.Load())
	}
}

func TestBranchRulesPlanAvailability(t *testing.T) {
	for _, tt := range []struct {
		name, body      string
		status          int
		wantUnavailable bool
	}{
		{"plan", `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`, 403, true},
		{"permission", `{"message":"Resource not accessible by integration"}`, 403, false},
		{"wrong status", `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`, 401, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			status := tt.status
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/example/repo" {
					fmt.Fprint(w, `{"default_branch":"main"}`)
					return
				}
				if tt.wantUnavailable {
					w.Header().Set("X-RateLimit-Limit", "5000")
					w.Header().Set("X-RateLimit-Remaining", "4900")
					w.Header().Set("X-RateLimit-Used", "100")
					w.Header().Set("X-RateLimit-Resource", "core")
					w.Header().Set("Retry-After", "60")
				}
				if status == http.StatusOK {
					w.Header().Set("ETag", `"rules"`)
				}
				w.WriteHeader(status)
				if status == http.StatusNotModified {
					return
				}
				if status == 200 {
					fmt.Fprint(w, "[]")
				} else {
					fmt.Fprint(w, tt.body)
				}
			}))
			defer server.Close()
			c, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: t.Name(), Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				err := c.RefreshMergeQueuePolicy(t.Context())
				if tt.wantUnavailable {
					if err != nil {
						t.Fatal(err)
					}
					policy, err := c.branchMergePolicy(t.Context(), "example/repo", "main")
					if err != nil || policy.MergeQueue || !policy.RulesUnavailableOnPlan {
						t.Fatalf("policy=%+v error=%v", policy, err)
					}
					usage := c.client.FlushRESTRateLimitUsage()
					if !usage.HasRateLimit || usage.RateLimit.Remaining != 4900 || usage.RateLimited || !usage.BackoffUntil.IsZero() {
						t.Fatalf("usage=%+v", usage)
					}
					var count, billable int64
					for _, request := range usage.Requests {
						count += request.Count
						billable += request.Billable
					}
					if count != 2 || billable != 2 {
						t.Fatalf("request count=%d billable=%d, want 2 each", count, billable)
					}
					if c.client.hasAuthHealth {
						t.Fatal("plan response affected auth health")
					}
					if err := c.client.restBackoffError(c.client.restBackoffKey, time.Now()); err != nil {
						t.Fatalf("backoff: %v", err)
					}
				} else if err == nil {
					t.Fatal("expected authentication error")
				}
			}
			if tt.wantUnavailable {
				if strings.Contains(logs.String(), "WARN") || strings.Count(logs.String(), "level=INFO") != 1 {
					t.Fatalf("logs: %s", &logs)
				}
				status = 200
				if err := c.RefreshMergeQueuePolicy(t.Context()); err != nil {
					t.Fatal(err)
				}
				status = 403
				if err := c.RefreshMergeQueuePolicy(t.Context()); err != nil {
					t.Fatal(err)
				}
				if strings.Count(logs.String(), "level=INFO") != 2 {
					t.Fatalf("logs after plan change: %s", &logs)
				}
				status = http.StatusNotModified
				if err := c.RefreshMergeQueuePolicy(t.Context()); err != nil {
					t.Fatal(err)
				}
				status = http.StatusForbidden
				if err := c.RefreshMergeQueuePolicy(t.Context()); err != nil {
					t.Fatal(err)
				}
				if strings.Count(logs.String(), "level=INFO") != 3 {
					t.Fatalf("logs after conditional recovery: %s", &logs)
				}

			}
		})
	}
}

func TestMergeQueueRefusalRefreshesCachedBranchPolicy(t *testing.T) {
	t.Parallel()
	for _, failRefresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh failure=%t", failRefresh), func(t *testing.T) {
			var refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/example/repo/pulls/42/merge":
					w.WriteHeader(http.StatusMethodNotAllowed)
					fmt.Fprint(w, `{"message":"Repository rule violations found. Changes must be made through the merge queue"}`)
				case "/repos/example/repo/rules/branches/release/stable":
					refreshes.Add(1)
					if failRefresh {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"message":"forbidden"}`)
						return
					}
					fmt.Fprint(w, `[{"type":"merge_queue","parameters":{"max_entries_to_build":2}}]`)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token"})
			if err != nil {
				t.Fatal(err)
			}
			c.cacheBranchMergePolicy("example/repo", BranchMergePolicy{Branch: "release/stable"})
			err = c.MergePullRequest(t.Context(), "example/repo", 42, "head", "squash")
			if !errors.Is(err, connector.ErrPullRequestMergeQueueRequired) {
				t.Fatalf("error=%v", err)
			}
			if refreshes.Load() != 1 {
				t.Fatalf("refreshes=%d", refreshes.Load())
			}
			cached, ok := c.branchMergePolicies["example/repo@release/stable"]
			if failRefresh && ok {
				t.Fatal("failed refresh retained stale policy")
			}
			if !failRefresh && (!ok || !cached.Policy.MergeQueue) {
				t.Fatalf("policy=%+v", cached)
			}
		})
	}
}
