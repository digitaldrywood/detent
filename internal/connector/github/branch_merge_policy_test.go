package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRepositoryBranchMergePolicy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		rules      string
		protection string
		wantQueue  bool
		wantStrict bool
		wantLimit  int
		status     int
		wantError  bool
	}{
		{name: "queue", rules: `[{"type":"merge_queue","parameters":{"max_entries_to_build":5}}]`, wantQueue: true, wantLimit: 5},
		{name: "classic strict", rules: `[]`, protection: `{"strict":true}`, wantStrict: true},
		{name: "ruleset strict", rules: `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true}}]`, wantStrict: true},
		{name: "unprotected", rules: `[]`},
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
			if got.Branch != "release/stable" || got.MergeQueue != tt.wantQueue || got.Strict != tt.wantStrict || got.AdmissionLimit != tt.wantLimit {
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
