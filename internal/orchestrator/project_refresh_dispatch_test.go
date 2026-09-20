package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestProjectRefreshDispatchAvoidsIssueReads(t *testing.T) {
	const count = 150
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	rest, pages := 0, 0
	issueNode := func(n int) map[string]any {
		return map[string]any{
			"__typename": "Issue", "id": fmt.Sprintf("I%d", n), "number": n,
			"title": "Candidate", "state": "OPEN", "body": "scheduler body",
			"url":        fmt.Sprintf("https://github.com/fixture/dispatch/issues/%d", n),
			"repository": map[string]string{"nameWithOwner": "fixture/dispatch"},
			"comments": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{
				"id":        fmt.Sprintf("C%d", n),
				"body":      "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```",
				"createdAt": now.Format(time.RFC3339), "updatedAt": now.Format(time.RFC3339),
			}}},
			"blockedBy": map[string]any{"nodes": []any{map[string]any{
				"id": "DONE", "number": 151, "state": "CLOSED", "title": "Completed prerequisite",
				"repository": map[string]string{"nameWithOwner": "fixture/dispatch"},
			}}},
			"closedByPullRequestsReferences": map[string]any{"totalCount": 0, "nodes": []any{}},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			rest++
			if strings.HasSuffix(r.URL.Path, "/pulls") {
				fmt.Fprint(w, "[]")
				return
			}
			http.Error(w, `{"message":"unexpected dispatch REST read"}`, http.StatusNotFound)
			return
		}
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		data := map[string]any{}
		switch {
		case strings.Contains(req.Query, "RefreshBlockerRevision"):
			data["nodes"] = []any{}
		case strings.Contains(req.Query, "CandidateHydration"):
			for key, value := range req.Variables {
				if strings.HasPrefix(key, "item") {
					data[key] = map[string]any{"id": value, "fieldValues": map[string]any{"nodes": []any{
						map[string]any{"__typename": "ProjectV2ItemFieldSingleSelectValue", "name": "Todo", "updatedAt": now.Format(time.RFC3339), "field": map[string]string{"name": "Status"}},
						map[string]any{"__typename": "ProjectV2ItemFieldTextValue", "text": "agents", "updatedAt": now.Format(time.RFC3339), "field": map[string]string{"name": "Team"}},
						map[string]any{"__typename": "ProjectV2ItemFieldSingleSelectValue", "name": "queued", "updatedAt": now.Add(time.Minute).Format(time.RFC3339), "field": map[string]string{"name": "render_status"}},
						map[string]any{"__typename": "ProjectV2ItemFieldNumberValue", "number": 3, "updatedAt": now.Format(time.RFC3339), "field": map[string]string{"name": "Estimate"}},
					}}}
				}
			}
			for key, value := range req.Variables {
				if strings.HasPrefix(key, "id") {
					var n int
					fmt.Sscanf(value.(string), "I%d", &n)
					data["issue"+strings.TrimPrefix(key, "id")] = issueNode(n)
				}
			}
		case strings.Contains(req.Query, "ObservedStatusProjectItems"), strings.Contains(req.Query, "query DetentGitHubProjectItems("):
			if strings.Contains(req.Query, "ObservedStatusProjectItems") {
				pages++
			}
			if strings.Contains(req.Query, "fieldValues(") {
				t.Error("enumeration contains custom fields")
			}
			start, end := 1, 100
			if req.Variables["after"] != nil {
				start, end = 101, count
			}
			nodes := []any{}
			for n := start; n <= end; n++ {
				item := map[string]any{"id": fmt.Sprintf("P%d", n), "content": issueNode(n), "statusValue": map[string]string{"name": "Todo", "updatedAt": now.Format(time.RFC3339)}}

				nodes = append(nodes, item)
			}
			data["node"] = map[string]any{"items": map[string]any{"totalCount": count, "nodes": nodes, "pageInfo": map[string]any{"hasNextPage": end < count, "endCursor": "page2"}}}
		default:
			t.Errorf("unexpected query: %s", req.Query)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	tracker, err := github.NewConnector(github.Config{Endpoint: server.URL + "/graphql", Repository: "fixture/dispatch", ProjectSlug: "PVT_1", TokenSource: github.StaticTokenSource("fixture"), HTTPClient: server.Client(), ActiveStates: []string{"Todo"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := normalizeConfig(Config{MaxConcurrentAgents: count, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
	orch := Orchestrator{cfg: cfg, connector: tracker}
	for cycle := range 2 {
		result := tracker.FetchRefreshIssues(t.Context(), []string{"Todo"}, nil, connector.IssueFilterHint{})
		if result.CandidateError != nil || result.StatusError != nil || len(result.Candidates) != count {
			t.Fatalf("refresh %d: candidates=%d errors=%v/%v", cycle, len(result.Candidates), result.CandidateError, result.StatusError)
		}
		before := rest
		state := newState(cfg)
		hydrated := 0
		cache := make(map[string]dependencyBlocker)
		newDispatchPlanner(cfg).plan(&state, result.Candidates, now, dispatchPlanHooks{
			hydrate: func(issue connector.Issue) (connector.Issue, bool) {
				hydrated++
				issue, ok := orch.hydrateDispatchIssue(t.Context(), &state, issue, now)
				if ok {
					issue = orch.hydrateDispatchDependencies(t.Context(), issue, cache)
				}
				return issue, ok
			},
			dispatch: func(dispatchAction) bool { return false },
			decision: func(d dispatchPlanDecision) {
				if d.SkipReason == dispatchSkipHydrationFailed {
					t.Errorf("hydrate_failed: %s", d.Issue.ID)
				}
			},
		})
		if hydrated != count || rest != before {
			t.Fatalf("cycle %d: hydrated=%d, dispatch REST=%d; want 150, 0", cycle, hydrated, rest-before)
		}
		declinedConfig := cfg
		declinedConfig.Authorization = selector.Selector{Labels: selector.Labels{Include: []string{"authorized"}}}
		declinedState := newState(declinedConfig)
		declined := 0
		newDispatchPlanner(declinedConfig).plan(&declinedState, result.Candidates, now, dispatchPlanHooks{
			hydrate: func(issue connector.Issue) (connector.Issue, bool) {
				t.Error("label-declined candidate reached hydration")
				return orch.hydrateDispatchIssue(t.Context(), &declinedState, issue, now)
			},
			decision: func(d dispatchPlanDecision) {
				if d.SkipReason == dispatchSkipAuthorizationSelector {
					declined++
				}
			},
		})
		if declined != count || rest != before {
			t.Fatalf("declined=%d REST=%d", declined, rest-before)
		}
		for _, issue := range result.Candidates {
			if issue.Fields["Team"] != "agents" || issue.Fields["Estimate"] != "3" || !issue.FieldUpdatedAt["Team"].Equal(now) || issue.Description != "scheduler body" {
				t.Fatalf("incomplete refresh fields/evidence: %+v", issue)
			}
		}
		issue := result.Candidates[0]
		if issue.WorkpadSignal == nil || issue.WorkpadSignal.Status != workpad.StatusInProgress || len(issue.Comments) != 1 || issue.DependencySource != connector.BlockedRefSourceNative || len(issue.BlockedBy) != 1 || !strings.EqualFold(issue.BlockedBy[0].State, "Done") {
			t.Fatalf("scheduler evidence lost: %+v", issue)
		}
		for _, tc := range []struct {
			value string
			want  bool
		}{{"agents", true}, {"other", false}} {
			fieldConfig := cfg
			fieldConfig.Authorization = selector.Selector{Fields: []selector.FieldEquals{{Name: "Team", Value: tc.value}}}
			fieldState := newState(fieldConfig)
			_, allowed, reason := newDispatchPlanner(fieldConfig).dispatchAction(&fieldState, issue, now)
			if allowed != tc.want || (!allowed && reason != dispatchSkipAuthorizationSelector) {
				t.Fatalf("field %q: allowed=%t reason=%s", tc.value, allowed, reason)
			}
		}
		artifact := gate.Config{Kind: gate.KindArtifact, Artifact: gate.ArtifactConfig{StatusField: "render_status", WaitStatuses: []string{"queued"}}}
		for _, tc := range []struct {
			name string
			lane time.Time
			wait bool
		}{
			{"field after lane entry", now, true},
			{"field at lane entry", now.Add(time.Minute), false},
			{"field before lane entry", now.Add(2 * time.Minute), false},
		} {
			issue.StageUpdatedAt = &tc.lane
			if got := artifactGateWaitStatusBlocksDispatch(issue, artifact); got != tc.wait {
				t.Errorf("%s: wait=%t", tc.name, got)
			}
		}
	}
	if pages != 4 {
		t.Fatalf("pages=%d, want 4", pages)
	}
}

func TestDispatchLabelAuthorizationBeforeHydration(t *testing.T) {
	for _, mode := range []struct {
		name    string
		retry   bool
		blocked bool
	}{
		{name: "fresh"},
		{name: "retry", retry: true},
		{name: "blocked retry", retry: true, blocked: true},
	} {
		for _, tc := range []struct {
			name        string
			auth        selector.Selector
			wantHydrate bool
		}{
			{"missing label", selector.Selector{Labels: selector.Labels{Include: []string{"allowed"}}}, false},
			{"excluded label", selector.Selector{Labels: selector.Labels{Exclude: []string{"present"}}}, false},
			{"matching label", selector.Selector{Labels: selector.Labels{Include: []string{"present"}}}, true},
			{"field deferred", selector.Selector{Fields: []selector.FieldEquals{{Name: "Team", Value: "agents"}}}, true},
			{"label and field", selector.Selector{Labels: selector.Labels{Include: []string{"allowed"}}, Fields: []selector.FieldEquals{{Name: "Team", Value: "agents"}}}, false},
			{"field alternative", selector.Selector{Or: []selector.Selector{{Labels: selector.Labels{Include: []string{"allowed"}}}, {Fields: []selector.FieldEquals{{Name: "Team", Value: "agents"}}}}}, true},
			{"nested label", selector.Selector{And: []selector.Selector{{Labels: selector.Labels{Include: []string{"allowed"}}}}}, false},
		} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, mode.name), func(t *testing.T) {
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 2, ActiveStates: []string{"Todo"}, Authorization: tc.auth})
				state := newState(cfg)
				now := time.Now()
				issue := dispatchTestIssue("candidate", "Todo")
				issue.Labels = []string{"present"}
				issue.Fields = nil
				if mode.retry {
					state.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(-time.Second)}
					state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
					if !tc.wantHydrate {
						state.BudgetRefusals[issue.ID] = BudgetRefusal{}
					}
				}
				if mode.blocked {
					state.Blocked[issue.ID] = Blocked{Issue: issue, Reason: "existing blocker", BlockedAt: now.Add(-time.Minute)}
				}
				calls := 0
				declined := false
				newDispatchPlanner(cfg).plan(&state, []connector.Issue{issue}, now, dispatchPlanHooks{
					hydrate: func(i connector.Issue) (connector.Issue, bool) {
						calls++
						i.Fields = map[string]string{"Team": "agents"}
						return i, true
					},
					dispatch: func(dispatchAction) bool { return false },
					decision: func(d dispatchPlanDecision) {
						if d.SkipReason == dispatchSkipAuthorizationSelector {
							declined = true
						}
					},
				})
				if (calls > 0) != tc.wantHydrate || declined == tc.wantHydrate {
					t.Fatalf("hydrate=%d declined=%t", calls, declined)
				}
				if mode.retry && !tc.wantHydrate {
					_, retryRetained := state.Retry[issue.ID]
					_, claimRetained := state.Claimed[issue.ID]
					_, budgetRetained := state.BudgetRefusals[issue.ID]
					if retryRetained || claimRetained || budgetRetained {
						t.Errorf("declined retry retains ownership: retry=%t claim=%t budget=%t", retryRetained, claimRetained, budgetRetained)
					}
					_, blockedRetained := state.Blocked[issue.ID]
					if blockedRetained != mode.blocked {
						t.Errorf("blocked retained=%t, want %t", blockedRetained, mode.blocked)
					}
				}
			})
		}
	}
}

// The incident board must deliver the exact selected identities all the way to
// the dispatch hook, without dispatch hydration undoing the connector savings.
func TestProjectRefreshInstanceSelectorCandidateDelivery(t *testing.T) {
	numbers := []int{3531, 3485, 3481, 3480, 1604, 9001, 9002, 9003}
	lanes := []string{"Todo", "Todo", "Todo", "Todo", "Todo", "Backlog", "Blocked", "Human Review"}
	for _, lane := range []struct {
		name string
		n    int
	}{{"Backlog", 247}, {"Todo", 127}, {"Blocked", 88}, {"Human Review", 5}, {"Rework", 6}, {"In Progress", 1}, {"Done", 1}, {"Cancelled", 1}} {
		for range lane.n {
			numbers = append(numbers, 10000+len(numbers))
			lanes = append(lanes, lane.name)
		}
	}
	issueNode := func(n int) map[string]any {
		labels := []any{}
		for _, selected := range numbers[:8] {
			if selected == n {
				labels = append(labels, map[string]string{"name": "detent:macbook-air-1"})
			}
		}
		return map[string]any{"__typename": "Issue", "id": fmt.Sprintf("I%d", n), "number": n, "state": "OPEN", "title": "Task", "body": "Task", "repository": map[string]string{"nameWithOwner": "getparable/parable"}, "labels": map[string]any{"totalCount": len(labels), "pageInfo": map[string]any{"hasNextPage": false}, "nodes": labels}, "comments": map[string]any{"totalCount": 0, "nodes": []any{}}, "blockedBy": map[string]any{"nodes": []any{}}}
	}
	rest := map[string]int{}
	hydratedIDs := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			rest[r.URL.Path]++
			if strings.HasSuffix(r.URL.Path, "/pulls") {
				fmt.Fprint(w, `[]`)
				return
			}
			t.Errorf("unexpected issue enrichment: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		data := map[string]any{}
		switch {
		case strings.Contains(req.Query, "CandidateHydration"):
			for key, value := range req.Variables {
				if strings.HasPrefix(key, "id") {
					var n int
					fmt.Sscanf(value.(string), "I%d", &n)
					hydratedIDs[value.(string)]++
					data["issue"+strings.TrimPrefix(key, "id")] = issueNode(n)
				}
				if strings.HasPrefix(key, "item") {
					data[key] = map[string]any{"id": value, "fieldValues": map[string]any{"nodes": []any{map[string]any{"__typename": "ProjectV2ItemFieldSingleSelectValue", "name": "Todo", "field": map[string]string{"name": "Status"}}}}}
				}
			}
		case strings.Contains(req.Query, "items(first:"):
			start := 0
			if after, ok := req.Variables["after"].(string); ok {
				fmt.Sscanf(after, "%d", &start)
			}
			end := min(start+100, len(numbers))
			items := []any{}
			for i := start; i < end; i++ {
				items = append(items, map[string]any{"id": fmt.Sprintf("P%d", numbers[i]), "statusValue": map[string]string{"name": lanes[i]}, "content": issueNode(numbers[i])})
			}
			data["node"] = map[string]any{"items": map[string]any{"totalCount": len(numbers), "nodes": items, "pageInfo": map[string]any{"hasNextPage": end < len(numbers), "endCursor": strconv.Itoa(end)}}}
		default:
			t.Errorf("unexpected query %s", req.Query)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	tracker, err := github.NewConnector(github.Config{Endpoint: server.URL + "/graphql", Repository: "getparable/parable", ProjectSlug: "PVT_1", TokenSource: github.StaticTokenSource("fixture"), HTTPClient: server.Client(), ActiveStates: []string{"Todo"}, RESTFanoutMaxRequests: 500})
	if err != nil {
		t.Fatal(err)
	}
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 5, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done", "Cancelled"}, Authorization: selector.Selector{Labels: selector.Labels{Include: []string{"detent:macbook-air-1"}}}})
	orch := Orchestrator{cfg: cfg, connector: tracker}
	result := tracker.FetchRefreshIssues(connector.WithRESTFanoutBudget(t.Context(), "refresh"), []string{"Todo"}, []string{"Backlog", "Blocked", "Human Review", "Rework", "In Progress", "Done", "Cancelled"}, orch.authorizationFilterHint())
	if result.CandidateError != nil || result.StatusError != nil {
		t.Fatalf("refresh errors: %v / %v", result.CandidateError, result.StatusError)
	}
	state := newState(cfg)
	delivered := map[string]int{}
	now := time.Now()
	newDispatchPlanner(cfg).plan(&state, result.Candidates, now, dispatchPlanHooks{
		hydrate: func(issue connector.Issue) (connector.Issue, bool) {
			return orch.hydrateDispatchIssue(t.Context(), &state, issue, now)
		},
		dispatch: func(action dispatchAction) bool { delivered[action.issue.Identifier]++; return true },
	})
	if len(delivered) != 5 {
		t.Fatalf("delivered=%v want exactly five", delivered)
	}
	for _, n := range numbers[:5] {
		id := fmt.Sprintf("getparable/parable#%d", n)
		if delivered[id] != 1 || hydratedIDs[fmt.Sprintf("I%d", n)] != 1 {
			t.Errorf("%s delivered=%d hydration=%v", id, delivered[id], hydratedIDs)
		}
	}
	if len(hydratedIDs) != 5 || len(rest) != 1 || rest["/repos/getparable/parable/pulls"] != 1 {
		t.Fatalf("hydration=%v REST=%v; want five batched targets and one repository PR list", hydratedIDs, rest)
	}
}
