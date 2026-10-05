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
	"github.com/digitaldrywood/detent/internal/gate"
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
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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

func TestDispatchPlannerFindsReadyTailBeyondKnownWaits(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"native waits", "unknown waits", "due retries", "running", "blocked", "claimed", "deferred completion", "pending retry", "pending CI", "refreshed CI", "completed gate", "operator rejection", "artifact wait", "mixed", "ready merge"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			capacity := 6
			switch mode {
			case "running":
				capacity += 24
			case "mixed":
				capacity += 6
			}
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: capacity, ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done"},
				AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand}},
			})
			if mode == "artifact wait" {
				cfg.AutoPromote.Gate = gate.Config{Kind: gate.KindArtifact, Artifact: gate.ArtifactConfig{StatusField: "render_status", WaitStatuses: []string{"rendering"}}}
			}
			if mode == "ready merge" {
				cfg.DispatchPriorityByState = []string{"Merging", "Rework", "In Progress", "Todo"}
			}
			state := newState(cfg)
			now := time.Date(2026, 9, 30, 21, 51, 0, 0, time.UTC)
			var candidates []connector.Issue
			for i := range 30 {
				issue := dispatchTestIssue(fmt.Sprintf("%02d", i), "Todo")
				issue.CreatedAt = new(now.Add(time.Duration(i) * time.Second))
				if i < 24 {
					wait := mode
					if mode == "mixed" {
						wait = []string{"running", "blocked", "deferred completion", "pending retry"}[i%4]
					}
					switch wait {
					case "native waits", "unknown waits", "due retries", "ready merge":
						issue.BlockedBy = []connector.BlockedRef{{Identifier: "fixture/dispatch#999", State: "Todo"}}
						if mode != "unknown waits" {
							issue.DependencySource = connector.BlockedRefSourceNative
						}
						if mode == "due retries" {
							state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 1, DueAt: now}
						}
					case "running":
						state.Running[issue.ID] = Running{Issue: issue}
					case "blocked":
						state.Blocked[issue.ID] = Blocked{Issue: issue, Reason: "human_action"}
					case "claimed":
						state.Claimed[issue.ID] = Claimed{}
					case "deferred completion":
						state.deferredCompletions[issue.ID] = deferredCompletion{}
					case "pending retry":
						state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 1, DueAt: now.Add(time.Minute)}
					case "pending CI", "refreshed CI", "completed gate", "operator rejection":
						issue.State = "Rework"
						issue.PullRequest = &connector.PullRequest{Number: i + 1, State: "OPEN", HeadSHA: "current", CIStatus: "pending"}
						if wait == "completed gate" || wait == "operator rejection" {
							issue.State = "In Progress"
							state.Completed[issue.ID] = Completed{Issue: cloneIssue(issue), FinalState: FinalStateCompleted, CompletedAt: now}
						}
					case "artifact wait":
						issue.Fields = map[string]string{"render_status": "rendering"}
					}
				}
				if mode == "ready merge" && (i == 0 || i == 29) {
					issue.State = "Merging"
					issue.BlockedBy = nil
					issue.PRRepository = "fixture/dispatch"
					issue.PullRequest = &connector.PullRequest{Number: 100 + i, State: "OPEN", HeadSHA: "current", CIStatus: "green", MergeableState: "clean"}
					if i == 0 {
						issue.StageUpdatedAt = new(now.Add(-time.Hour))
						state.Blocked[issue.ID] = Blocked{Issue: cloneIssue(issue), Reason: "human_action", Source: BlockedSourceProjectStatus}
					}
				}
				candidates = append(candidates, issue)
			}
			var hydrated []string
			planner := newDispatchPlanner(cfg)
			rejections := 0
			if mode == "operator rejection" {
				planner.operatorRejectedHead = func(connector.Issue) (bool, error) {
					rejections++
					return true, nil
				}
			}
			plan := planner.plan(&state, candidates, now, dispatchPlanHooks{
				hydrate: func(issue connector.Issue) (connector.Issue, bool) {
					hydrated = append(hydrated, issue.ID)
					if mode == "refreshed CI" {
						issue.PullRequest.CIStatus = "green"
					}
					return issue, true
				},
			})
			if mode != "unknown waits" && mode != "due retries" && mode != "pending CI" && mode != "completed gate" && mode != "artifact wait" {
				want := []string{"24", "25", "26", "27", "28", "29"}
				switch mode {
				case "refreshed CI", "operator rejection":
					want = []string{"00", "01", "02", "03", "04", "05"}
				case "ready merge":
					want = []string{"29", "24", "25", "26", "27", "28"}
				}
				var dispatched []string
				for _, decision := range plan.Dispatches {
					dispatched = append(dispatched, decision.IssueID)
				}
				if !slices.Equal(dispatched, want) || !slices.Equal(hydrated, want) {
					t.Fatalf("ready tail dispatches=%+v hydrated=%v", plan.Dispatches, hydrated)
				}
				if mode == "operator rejection" && rejections != 6 {
					t.Fatalf("rejection callbacks = %d, want six fresh eligibility calls", rejections)
				}
			} else if len(plan.Dispatches) != 0 || len(hydrated) != 14 || hydrated[0] != "00" || hydrated[13] != "13" {
				t.Fatalf("unknown/retry evidence must retain bound: dispatches=%+v hydrated=%v", plan.Dispatches, hydrated)
			}
		})
	}
}

func TestDispatchPlannerDependencyWaitProvenanceAcrossTicks(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"", connector.BlockedRefSourceNative} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 6, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			state := newState(cfg)
			planner := newDispatchPlanner(cfg)
			now := time.Date(2026, 10, 1, 1, 55, 0, 0, time.UTC)
			var candidates []connector.Issue
			for i := range 30 {
				issue := dispatchTestIssue(fmt.Sprintf("%02d", i), "Todo")
				issue.CreatedAt = new(now.Add(time.Duration(i) * time.Second))
				if i < 2 {
					issue.DependencySource = source
					issue.BlockedBy = []connector.BlockedRef{{Identifier: "fixture/dispatch#999", State: "Todo"}}
				}
				candidates = append(candidates, issue)
			}
			for tick := range 2 {
				clear(state.Running)
				clear(state.Claimed)
				if tick == 1 && source == connector.BlockedRefSourceNative {
					candidates[0].BlockedBy, candidates[1].BlockedBy = nil, nil
				}
				planner.trackBlockedCandidates(&state, candidates, now)
				var hydrated []string
				plan := planner.plan(&state, candidates, now, dispatchPlanHooks{
					hydrate: func(issue connector.Issue) (connector.Issue, bool) {
						hydrated = append(hydrated, issue.ID)
						if tick == 1 {
							issue.BlockedBy = nil
						}
						if blocked, ok := state.Blocked[issue.ID]; ok && blockedFromDependency(blocked) && !issueBlockedByNonTerminal(issue, cfg.TerminalStates) {
							delete(state.Blocked, issue.ID)
						}
						return issue, true
					},
				})
				want := []string{"00", "01", "02", "03", "04", "05"}
				if tick == 0 {
					want = []string{"02", "03", "04", "05", "06", "07"}
					if source != connector.BlockedRefSourceNative {
						want = append([]string{"00", "01"}, want...)
					}
				}
				if len(plan.Dispatches) != 6 || !slices.Equal(hydrated, want) {
					t.Fatalf("tick %d: hydrated=%v dispatches=%+v; want six admissions with reads %v", tick, hydrated, plan.Dispatches, want)
				}
				if tick == 1 {
					for _, id := range []string{"00", "01"} {
						if _, held := state.Blocked[id]; held {
							t.Fatalf("tick %d retained cleared dependency %s", tick, id)
						}
					}
				}
				now = now.Add(time.Minute)
			}
		})
	}
}
