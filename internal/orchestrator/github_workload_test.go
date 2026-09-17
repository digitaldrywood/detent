package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

// This joins the real connector, refresh tick, eligibility and lane writer.
// Time and HTTP are injected; no external server or worker process is started.
func TestGitHubFleetRefreshWorkload(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		projects, active, ticks int
	}{
		{"five active hourly", 5, 5, 120},
		{"one active four empty hourly", 5, 1, 120},
		{"ten cold", 10, 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
			reset := now.Add(time.Hour)
			budget := &fleetWorkloadBudget{reset: reset}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fleet := make([]*Orchestrator, tc.projects)
			states := make([]State, tc.projects)
			fixtures := make([]*fleetWorkloadHTTP, tc.projects)
			for i := range fleet {
				repo := fmt.Sprintf("fixture/project%d", i)
				fixture := &fleetWorkloadHTTP{t: t, budget: budget, repo: repo, active: i < tc.active, blocked: tc.ticks > 1, lane: "Todo"}
				fixtures[i] = fixture
				tracker, err := github.NewConnector(github.Config{
					Endpoint:   fmt.Sprintf("https://fleet-%s-%d.test/graphql", strings.ReplaceAll(tc.name, " ", "-"), i),
					Repository: repo, ProjectSlug: "PVT_1", TokenSource: github.StaticTokenSource("fixture-token"), HTTPClient: fixture,
					ActiveStates: []string{"Todo", "In Progress"}, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				})
				if err != nil {
					t.Fatal(err)
				}
				cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: repo}, PollInterval: 30 * time.Second, MaxConcurrentAgents: 2, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done", "Cancelled"}})
				orch := newRateLimitTestOrchestrator(cfg, tracker)
				orch.supervisor = newTestSupervisor(t, parityBlockingRunner{}, cfg)
				orch.runResults = make(chan runpkg.Completion, 1)
				orch.now = func() time.Time { return now }
				fleet[i], states[i] = orch, newState(cfg)
			}
			for tick := range tc.ticks {
				for i, orch := range fleet {
					fixture, state := fixtures[i], &states[i]
					fixture.blocked = tick < tc.ticks-1
					orch.tick(ctx, state, now)
					if state.LastRefreshError != "" {
						t.Fatalf("tick %d project %d refresh: %s", tick, i, state.LastRefreshError)
					}
					if _, _, backoff := githubLookupBackoff(state.BackendOutages); backoff {
						t.Fatalf("tick %d project %d lookup backoff", tick, i)
					}
					wantCandidates := 0
					if fixture.active {
						wantCandidates = 1
					}
					if len(state.BoardIssues) != wantCandidates {
						t.Fatalf("tick %d project %d board=%d want=%d", tick, i, len(state.BoardIssues), wantCandidates)
					}
					wantRunning := 0
					if fixture.active && !fixture.blocked {
						wantRunning = 1
					}
					if state.DispatchStatus.EligibleCandidateCount != wantRunning {
						t.Fatalf("tick %d project %d eligible=%d want=%d", tick, i, state.DispatchStatus.EligibleCandidateCount, wantRunning)
					}
					if len(state.Running) != wantRunning || fixture.transitions != wantRunning {
						t.Fatalf("tick %d project %d running=%d transitions=%d want=%d decisions=%+v", tick, i, len(state.Running), fixture.transitions, wantRunning, state.SchedulerDecisions)
					}
				}
				now = now.Add(30 * time.Second)
			}
			if budget.billable >= 2500 || budget.limited != 0 {
				t.Fatalf("billable=%d 429=%d", budget.billable, budget.limited)
			}
			t.Logf("projects=%d active=%d ticks=%d cadence=30s REST=%d billable_REST=%d GraphQL=%d 304=%d starts=%d refresh_errors=0 lookup_backoff=0 429=%d", tc.projects, tc.active, tc.ticks, budget.rest, budget.billable, budget.graphql, budget.rest-budget.billable, tc.active, budget.limited)
		})
	}
}

type fleetWorkloadBudget struct {
	rest, billable, graphql, limited int
	reset                            time.Time
}
type fleetWorkloadHTTP struct {
	t               *testing.T
	budget          *fleetWorkloadBudget
	repo, lane      string
	active, blocked bool
	transitions     int
}

func (f *fleetWorkloadHTTP) Do(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	var value any
	if r.Method == http.MethodPost {
		f.budget.graphql++
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		data := map[string]any{}
		switch {
		case strings.Contains(req.Query, "updateProjectV2ItemFieldValue"):
			if req.Variables["optionId"] != "progress" {
				f.t.Errorf("unexpected transition: %v", req.Variables)
			}
			f.transitions++
			f.lane = "In Progress"
			data["updateProjectV2ItemFieldValue"] = map[string]any{"projectV2Item": map[string]string{"id": "ITEM_1"}}
		case strings.Contains(req.Query, "DetentGitHubStatusField"):
			data["node"] = map[string]any{"field": map[string]any{"id": "STATUS", "options": []any{map[string]string{"id": "todo", "name": "Todo"}, map[string]string{"id": "progress", "name": "In Progress"}}}}
		case strings.Contains(req.Query, "DetentGitHubProjectItemForIssue"):
			data["node"] = map[string]any{"projectItems": map[string]any{"nodes": []any{map[string]any{"id": "ITEM_1", "project": map[string]string{"id": "PVT_1"}, "statusValue": map[string]string{"name": f.lane}}}}}
		case strings.Contains(req.Query, "DetentGitHubProjectFieldItems"):
			node := f.issue()
			node["projectItems"] = map[string]any{"nodes": []any{map[string]any{"id": "ITEM_1", "project": map[string]string{"id": "PVT_1"}}}}
			data["issue0"] = node
		case strings.Contains(req.Query, "DetentGitHubProjectFieldHydration"):
			data["item0"] = map[string]any{"id": "ITEM_1", "statusValue": map[string]string{"name": f.lane}, "fieldValues": map[string]any{"nodes": []any{}}}
		case strings.Contains(req.Query, "DetentGitHubCandidateHydration"):
			for key, id := range req.Variables {
				if strings.HasPrefix(key, "item") {
					data[key] = map[string]any{"id": id, "fieldValues": map[string]any{"nodes": []any{}}}
				}
			}
			data["issue0"] = f.issue()
		case strings.Contains(req.Query, "DetentGitHubProjectItems"), strings.Contains(req.Query, "DetentGitHubObservedStatusProjectItems"):
			items := []any{}
			if f.active {
				issue := f.issue()
				if !strings.Contains(req.Query, "blockedBy(") {
					delete(issue, "blockedBy")
					delete(issue, "body")
					delete(issue, "closedByPullRequestsReferences")
				}
				items = append(items, map[string]any{"id": "ITEM_1", "content": issue, "statusValue": map[string]string{"name": f.lane}})
			}
			data["node"] = map[string]any{"items": map[string]any{"totalCount": len(items), "nodes": items}}
		default:
			f.t.Errorf("unexpected GraphQL: %s", req.Query)
		}
		data["rateLimit"] = map[string]any{"limit": 5000, "remaining": 5000 - f.budget.graphql, "cost": 1, "resetAt": f.budget.reset.Format(time.RFC3339)}
		value = map[string]any{"data": data}
	} else {
		f.budget.rest++
		switch {
		case r.URL.Path == "/repos/"+f.repo+"/issues":
			rows := []any{}
			if f.active {
				rows = append(rows, f.restIssue())
			}
			value = rows
		case strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by"):
			value = []any{}
			if f.blocked {
				value = []any{map[string]any{"node_id": "BLOCKER", "number": 2, "state": "open", "repository": map[string]string{"full_name": f.repo}, "html_url": "https://github.com/" + f.repo + "/issues/2"}}
			}
		case strings.HasSuffix(r.URL.Path, "/comments"), strings.HasSuffix(r.URL.Path, "/pulls"):
			value = []any{}
		case strings.HasSuffix(r.URL.Path, "/issues/2"):
			value = map[string]any{"node_id": "BLOCKER", "number": 2, "state": "open", "title": "External dependency", "body": "", "html_url": "https://github.com/" + f.repo + "/issues/2", "labels": []any{map[string]string{"name": "detent:todo"}}}
		case strings.HasSuffix(r.URL.Path, "/issues/1"):
			value = f.restIssue()
		default:
			f.t.Errorf("unexpected REST: %s %s", r.Method, r.URL)
			value = map[string]string{}
		}
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	status := http.StatusOK
	if r.Method != http.MethodPost {
		etag := fmt.Sprintf("\"%x\"", sha256.Sum256(body))
		headers.Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			status = http.StatusNotModified
			body = nil
		} else {
			f.budget.billable++
		}
		headers.Set("X-RateLimit-Limit", "5000")
		headers.Set("X-RateLimit-Remaining", strconv.Itoa(5000-f.budget.billable))
		headers.Set("X-RateLimit-Used", strconv.Itoa(f.budget.billable))
		headers.Set("X-RateLimit-Reset", strconv.FormatInt(f.budget.reset.Unix(), 10))
		headers.Set("X-RateLimit-Resource", "core")
	}
	if f.budget.billable > 5000 || f.budget.graphql > 5000 {
		status = http.StatusTooManyRequests
		f.budget.limited++
		body = []byte(`{"message":"fixture hourly budget exhausted"}`)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

func (f *fleetWorkloadHTTP) issue() map[string]any {
	blockers := []any{}
	if f.blocked {
		blockers = append(blockers, map[string]any{"id": "BLOCKER", "number": 2, "state": "OPEN", "repository": map[string]string{"nameWithOwner": f.repo}, "labels": map[string]any{"nodes": []any{map[string]string{"name": "detent:todo"}}}})
	}
	return map[string]any{"__typename": "Issue", "id": "ISSUE_1", "number": 1, "state": "OPEN", "title": "Implement fixture change", "body": "fixture body", "url": "https://github.com/" + f.repo + "/issues/1", "repository": map[string]string{"nameWithOwner": f.repo}, "comments": map[string]any{"totalCount": 0, "nodes": []any{}}, "blockedBy": map[string]any{"nodes": blockers}, "closedByPullRequestsReferences": map[string]any{"totalCount": 0, "nodes": []any{}}}
}
func (f *fleetWorkloadHTTP) restIssue() map[string]any {
	return map[string]any{"node_id": "ISSUE_1", "number": 1, "state": "open", "title": "Implement fixture change", "body": "fixture body", "html_url": "https://github.com/" + f.repo + "/issues/1", "labels": []any{}, "assignees": []any{}, "comments": 0}
}
