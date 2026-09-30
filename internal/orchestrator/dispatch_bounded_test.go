package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDispatchPlannerBoundsCandidateEvaluation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"hydration failure", "dependency wait", "dispatch refusal", "due retries", "occupied slots", "successful dispatch"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 2, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			state := newState(cfg)
			if mode == "occupied slots" {
				state.MaxConcurrentAgents = 6
				for i := range 4 {
					state.Running[fmt.Sprintf("running-%d", i)] = Running{}
				}
			}
			now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
			var candidates []connector.Issue
			for i := 150; i > 0; i-- {
				priority := 2
				if i == 1 {
					priority = 1
				}
				issue := connector.Issue{ID: fmt.Sprintf("I_%03d", i), Identifier: fmt.Sprintf("fixture/dispatch#%d", i), Title: "candidate", State: "Todo", AssignedToWorker: true, Priority: &priority, CreatedAt: new(now.Add(time.Duration(i) * time.Second))}
				candidates = append(candidates, issue)
				if mode == "due retries" {
					state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 1, DueAt: now}
				}
			}
			var hydrated []string
			plan := newDispatchPlanner(cfg).plan(&state, candidates, now, dispatchPlanHooks{
				hydrate: func(issue connector.Issue) (connector.Issue, bool) {
					hydrated = append(hydrated, issue.ID)
					if mode == "dependency wait" {
						issue.BlockedBy = []connector.BlockedRef{{Identifier: "fixture/dispatch#999", State: "Todo"}}
					}
					return issue, mode != "hydration failure"
				},
				dispatch: func(action dispatchAction) bool {
					if mode == "successful dispatch" {
						newDispatchPlanner(cfg).markDispatched(&state, action, now)
						return true
					}
					return false
				},
			})
			want := []string{"I_001", "I_002", "I_003", "I_004", "I_005", "I_006", "I_007", "I_008", "I_009", "I_010"}
			expectedDispatches := 0
			if mode == "successful dispatch" {
				want = want[:2]
				expectedDispatches = 2
			}
			if !slices.Equal(hydrated, want) {
				t.Fatalf("hydrated %d candidates: %v; want %v", len(hydrated), hydrated, want)
			}
			if len(plan.Dispatches) != expectedDispatches {
				t.Fatalf("unexpected dispatches: %+v", plan.Dispatches)
			}
		})
	}
}

func TestDispatchPlannerBoundsGitHubIssueReads(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var reads []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/fixture/dispatch/issues/"))
			if err != nil {
				t.Error(err)
				http.Error(w, `{"message":"unexpected request"}`, http.StatusBadRequest)
				return
			}
			mu.Lock()
			reads = append(reads, number)
			mu.Unlock()
			http.Error(w, `{"message":"issue hydration unavailable"}`, http.StatusBadRequest)
			return
		}
		var request struct {
			Variables struct {
				IDs []string `json:"issueIds"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var nodes []map[string]any
		for _, id := range request.Variables.IDs {
			number, err := strconv.Atoi(strings.TrimPrefix(id, "I_"))
			if err != nil {
				t.Error(err)
				return
			}
			nodes = append(nodes, map[string]any{"__typename": "Issue", "id": id, "number": number, "repository": map[string]string{"nameWithOwner": "fixture/dispatch"}})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	tracker, err := github.NewConnector(github.Config{Endpoint: server.URL + "/graphql", Repository: "fixture/dispatch", GitHubStatusSource: github.GitHubStatusSourceLabel, TokenSource: github.StaticTokenSource("fixture"), HTTPClient: server.Client(), ActiveStates: []string{"Todo"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 2, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
	state := newState(cfg)
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	var candidates []connector.Issue
	for i := 150; i > 0; i-- {
		candidates = append(candidates, connector.Issue{
			ID: fmt.Sprintf("I_%03d", i), Identifier: fmt.Sprintf("fixture/dispatch#%d", i),
			URL:   fmt.Sprintf("https://github.test/fixture/dispatch/issues/%d", i),
			Title: "candidate", State: "Todo", AssignedToWorker: true,
			CreatedAt: new(now.Add(time.Duration(i) * time.Second)),
			PRNumber:  new(900 + i), PRRepository: "fixture/dispatch",
			PullRequest: &connector.PullRequest{Number: 900 + i, HeadSHA: fmt.Sprintf("head-%d", i)},
		})
	}
	orch := Orchestrator{cfg: cfg, connector: tracker}
	decisions := 0
	plan := newDispatchPlanner(cfg).plan(&state, candidates, now, dispatchPlanHooks{
		hydrate: func(issue connector.Issue) (connector.Issue, bool) {
			return orch.hydrateDispatchIssue(t.Context(), &state, issue, now)
		},
		decision: func(decision dispatchPlanDecision) {
			decisions++
			number := decisions
			issue := decision.Issue
			if decision.SkipReason != dispatchSkipHydrationFailed || decision.Selected ||
				issue.ID != fmt.Sprintf("I_%03d", number) || issue.Identifier != fmt.Sprintf("fixture/dispatch#%d", number) ||
				issue.URL != fmt.Sprintf("https://github.test/fixture/dispatch/issues/%d", number) || issue.State != "Todo" ||
				issue.PRRepository != "fixture/dispatch" || issue.PRNumber == nil || *issue.PRNumber != 900+number ||
				issue.PullRequest == nil || issue.PullRequest.Number != 900+number || issue.PullRequest.HeadSHA != fmt.Sprintf("head-%d", number) {
				t.Errorf("failed hydration lost candidate evidence or became selected: %+v", decision)
			}
		},
	})
	if len(plan.Dispatches) != 0 || decisions != 10 {
		t.Fatalf("failed hydration dispatches=%d decisions=%d; want 0 and 10", len(plan.Dispatches), decisions)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(reads, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("GitHub issue reads = %v; want top ten candidates", reads)
	}
}
