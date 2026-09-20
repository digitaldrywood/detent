package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestProjectRefreshInstanceSelector(t *testing.T) {
	for _, mode := range []string{"schema fallback", "incomplete evidence", "batched evidence", "304", "partial failure", "observed routing"} {
		for _, count := range []int{8, 484} {
			t.Run(mode+"/"+strconv.Itoa(count), func(t *testing.T) {
				numbers := []int{3531, 3485, 3481, 3480, 1604, 9001, 9002, 9003}
				lanes := []string{"Todo", "Todo", "Todo", "Todo", "Todo", "Backlog", "Blocked", "Human Review"}
				if count == 484 {
					for _, lane := range []struct {
						name string
						n    int
					}{{"Backlog", 247}, {"Todo", 127}, {"Blocked", 88}, {"Human Review", 5}, {"Rework", 6}, {"In Progress", 1}, {"Done", 1}, {"Cancelled", 1}} {
						for range lane.n {
							numbers = append(numbers, 10000+len(numbers))
							lanes = append(lanes, lane.name)
						}
					}
				}
				requests := map[string]int{}
				notModified := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					write := func(v any) {
						if err := json.NewEncoder(w).Encode(v); err != nil {
							t.Error(err)
						}
					}
					if r.Method == http.MethodPost {
						var req struct {
							Query     string
							Variables map[string]any
						}
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
							return
						}
						switch {
						case strings.Contains(req.Query, "CandidateHydration"):
							if mode == "schema fallback" || mode == "304" || mode == "partial failure" || mode == "observed routing" {
								fmt.Fprint(w, `{"errors":[{"message":"Cannot query field blockedBy on type Issue"}]}`)
								return
							}
							data := map[string]any{}
							if mode == "batched evidence" {
								for key, value := range req.Variables {
									if strings.HasPrefix(key, "id") {
										data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": value, "body": "Task", "comments": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "body": "Historical note"}}}, "blockedBy": map[string]any{"nodes": []any{}}}
									}
								}
							}
							addSelectorFixtureProjectFields(data, req.Variables)
							write(map[string]any{"data": data})
						case strings.Contains(req.Query, "ProjectFieldHydration"):
							data := map[string]any{}
							addSelectorFixtureProjectFields(data, req.Variables)
							write(map[string]any{"data": data})
						case strings.Contains(req.Query, "items(first:"):
							start := 0
							if after, ok := req.Variables["after"].(string); ok {
								start, _ = strconv.Atoi(after)
							}
							end := min(start+100, len(numbers))
							page := projectItemsConnection{TotalCount: len(numbers), PageInfo: pageInfo{HasNextPage: end < len(numbers), EndCursor: strconv.Itoa(end)}}
							for i := start; i < end; i++ {
								n := numbers[i]
								node := &githubIssueNode{TypeName: "Issue", ID: fmt.Sprintf("I%d", n), Number: n, Title: "Task", Body: "Task", State: "OPEN", Repository: repository{NameWithOwner: "getparable/parable"}, Author: &actor{Login: "alice"}}
								node.Labels.Nodes = []label{}
								node.Assignees.Nodes = []assignee{{Login: "bob"}}
								node.Assignees.TotalCount = 1
								if i < 8 {
									node.Labels.Nodes = []label{{Name: "detent:macbook-air-1"}}
									node.Labels.TotalCount = 1
								}
								node.Comments.TotalCount = 1
								page.Nodes = append(page.Nodes, projectItemNode{ID: fmt.Sprintf("P%d", n), StatusValue: &singleSelectValue{Name: lanes[i]}, Content: node})
							}
							write(map[string]any{"data": map[string]any{"node": map[string]any{"items": page}}})
						default:
							fmt.Fprint(w, `{"data":{}}`)
						}
						return
					}
					requests[r.URL.RequestURI()]++
					if mode == "partial failure" && strings.Contains(r.URL.Path, "/3481/comments") {
						http.Error(w, "fixture failure after progress", http.StatusBadGateway)
						return
					}
					if mode == "304" {
						w.Header().Set("ETag", `"stable"`)
						if r.Header.Get("If-None-Match") != "" {
							notModified++
							w.WriteHeader(http.StatusNotModified)
							return
						}
					}

					switch {
					case strings.HasSuffix(r.URL.Path, "/comments"):
						fmt.Fprint(w, `[{"id":1,"body":"Historical note"}]`)
					case strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by"), strings.HasSuffix(r.URL.Path, "/pulls"):
						fmt.Fprint(w, `[]`)
					default:
						t.Errorf("unexpected REST %s", r.URL)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "getparable/parable", RESTFanoutMaxRequests: 500, ActiveStates: []string{"Todo"}})
				hint := connector.IssueFilterHint{LabelInclude: []string{"detent:macbook-air-1"}}
				if mode == "observed routing" {
					hint.SchedulerStates = []string{"Blocked", "Human Review", "Rework", "In Progress"}
				}
				ctx := connector.WithRESTFanoutBudget(t.Context(), "refresh")
				result := c.FetchRefreshIssues(ctx, []string{"Todo"}, []string{"Blocked", "Human Review", "Rework", "In Progress", "Done", "Cancelled", "Backlog"}, hint)
				if mode == "partial failure" {
					if result.CandidateError == nil || len(result.Candidates) != 0 || len(result.Statuses) != 0 || len(result.LaneSignalCandidates) != 0 || len(requests) < 3 {
						t.Fatalf("partial failure published evidence: %+v; requests=%v", result, requests)
					}
					return
				}
				if mode == "304" {
					ctx = connector.WithRESTFanoutBudget(t.Context(), "refresh")
					result = c.FetchRefreshIssues(ctx, []string{"Todo"}, []string{"Blocked", "Human Review", "Rework", "In Progress", "Done", "Cancelled", "Backlog"}, hint)
				}
				measured := map[string]int{}
				for path, n := range requests {
					measured[restEndpointFamily(http.MethodGet, path)] += n
				}
				t.Logf("refresh window mode=%s board=%d candidates=%d REST families=%v", mode, count, len(result.Candidates), measured)

				if result.CandidateError != nil || result.StatusError != nil {
					t.Fatalf("refresh errors: %v / %v", result.CandidateError, result.StatusError)
				}
				var got []string
				for _, issue := range result.Candidates {
					if issue.Fields["Status"] != "Todo" || issue.DependencySource != connector.BlockedRefSourceNative || len(issue.Comments) != 1 {
						t.Fatalf("incomplete candidate evidence: %+v", issue)
					}
					got = append(got, issue.Identifier)
				}
				slices.Sort(got)
				want := []string{"getparable/parable#1604", "getparable/parable#3480", "getparable/parable#3481", "getparable/parable#3485", "getparable/parable#3531"}
				if !slices.Equal(got, want) {
					t.Fatalf("candidates=%v want=%v", got, want)
				}
				total := 0
				for path, n := range requests {
					total += n
					for _, number := range numbers[8:] {
						if strings.Contains(path, fmt.Sprintf("/issues/%d/", number)) || strings.Contains(path, fmt.Sprintf("issue-%d", number)) {
							t.Errorf("excluded enrichment: %s", path)
						}
					}
				}
				wantRequests := 11
				if mode == "observed routing" {
					wantRequests = 16
				}
				if mode == "304" {
					wantRequests = 22
				}
				if mode == "batched evidence" {
					wantRequests = 1
				}
				if total != wantRequests {
					t.Errorf("REST requests=%d want %d", total, wantRequests)
				}
				families := map[string]int{}
				for path, n := range requests {
					families[restEndpointFamily(http.MethodGet, path)] += n
				}
				wantPerIssue := 5
				if mode == "batched evidence" {
					wantPerIssue = 0
				}
				if mode == "observed routing" {
					wantPerIssue = 7
				}
				if mode == "304" {
					wantPerIssue = 10
				}
				if families["issue comments"] != wantPerIssue || families["issue dependencies"] != wantPerIssue || families["issue reads"] != 0 {
					t.Errorf("families=%v want %d comment/dependency reads and zero issue reads", families, wantPerIssue)
				}
				usage := c.client.FlushRESTRateLimitUsage()
				if usage.TotalRequests != int64(total) || usage.NotModifiedRequests != int64(notModified) || usage.BillableRequests != int64(total-notModified) {
					t.Errorf("REST accounting=%+v actual=%d 304=%d", usage, total, notModified)
				}
				if mode == "304" {
					budget, _ := connector.RESTFanoutBudgetFromContext(ctx)
					units, _ := budget.Reserve(0, 0)
					t.Logf("second refresh local fanout units=%d", units)
					if units != 11 {
						t.Errorf("conditional local fanout units=%d want 11 (quarter-request units)", units)
					}
					if notModified != 11 {
						t.Errorf("304=%d want=11", notModified)
					}
				}
				t.Logf("fixture combined refresh window: REST=%d requests=%v", total, requests)
			})
		}
	}
}

func addSelectorFixtureProjectFields(data map[string]any, variables map[string]any) {
	for key, id := range variables {
		if strings.HasPrefix(key, "item") {
			data[key] = map[string]any{"id": id, "fieldValues": map[string]any{"nodes": []any{map[string]any{"__typename": "ProjectV2ItemFieldSingleSelectValue", "name": "Todo", "field": map[string]string{"name": "Status"}}}}}
		}
	}
}

func TestRefreshSelectorPredicatesAndMetadata(t *testing.T) {
	for _, tt := range []struct {
		name      string
		hint      connector.IssueFilterHint
		metadata  string
		want      bool
		wantError bool
		pages     int
	}{
		{name: "author alternatives", hint: connector.IssueFilterHint{Authors: []string{"other", "ALICE"}}, want: true},
		{name: "author excluded", hint: connector.IssueFilterHint{Authors: []string{"other"}}},
		{name: "missing author", hint: connector.IssueFilterHint{Authors: []string{"alice"}}, metadata: "no author"},
		{name: "assignee alternatives", hint: connector.IssueFilterHint{Assignees: []string{"other", "BOB"}}, want: true},
		{name: "assignee excluded", hint: connector.IssueFilterHint{Assignees: []string{"other"}}},
		{name: "all inclusion labels", hint: connector.IssueFilterHint{LabelInclude: []string{"ready", "SELECTED"}}, want: true},
		{name: "inclusion requires all", hint: connector.IssueFilterHint{LabelInclude: []string{"ready", "missing"}}},
		{name: "any exclusion label", hint: connector.IssueFilterHint{LabelExclude: []string{"missing", "ready"}}},
		{name: "combined AND", hint: connector.IssueFilterHint{Authors: []string{"alice"}, Assignees: []string{"bob"}, LabelInclude: []string{"ready"}, LabelExclude: []string{"declined"}}, want: true},
		{name: "combined denied", hint: connector.IssueFilterHint{Authors: []string{"alice"}, Assignees: []string{"other"}, LabelInclude: []string{"ready"}}},
		{name: "nested inclusion", hint: connector.IssueFilterHint{LabelInclude: []string{"late"}}, metadata: "labels", want: true, pages: 1},
		{name: "nested exclusion", hint: connector.IssueFilterHint{LabelExclude: []string{"late"}}, metadata: "labels", pages: 1},
		{name: "nested assignee", hint: connector.IssueFilterHint{Assignees: []string{"late"}}, metadata: "assignees", want: true, pages: 1},
		{name: "missing metadata reread", hint: connector.IssueFilterHint{LabelExclude: []string{"late"}}, metadata: "missing", pages: 1},
		{name: "incomplete metadata fails closed", hint: connector.IssueFilterHint{LabelExclude: []string{"late"}}, metadata: "incomplete", wantError: true, pages: 1},
		{name: "excluded blank status cannot be repaired", hint: connector.IssueFilterHint{LabelExclude: []string{"ready"}}, metadata: "blank"},
		{name: "invalid nested cursor", hint: connector.IssueFilterHint{LabelExclude: []string{"late"}}, metadata: "bad cursor", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enriched, pages := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					if err := json.NewEncoder(w).Encode(v); err != nil {
						t.Error(err)
					}
				}
				if r.Method != http.MethodPost {
					enriched++
					fmt.Fprint(w, `[]`)
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
				switch {
				case strings.Contains(req.Query, "RefreshSelectorMetadata"):
					pages++
					if tt.metadata == "incomplete" {
						fmt.Fprint(w, `{"data":{"node":{"id":"I1","values":{"nodes":[]}}}}`)
						return
					}
					field := "name"
					total := 3
					if tt.metadata == "assignees" {
						field = "login"
						total = 2
					}
					if tt.metadata == "missing" {
						total = 1
					}
					write(map[string]any{"data": map[string]any{"node": map[string]any{"id": "I1", "values": map[string]any{"totalCount": total, "pageInfo": pageInfo{}, "nodes": []any{map[string]string{field: "late"}}}}}})
				case strings.Contains(req.Query, "items(first:"):
					labels := map[string]any{"totalCount": 2, "pageInfo": pageInfo{}, "nodes": []any{map[string]string{"name": "ready"}, map[string]string{"name": "selected"}}}
					assignees := map[string]any{"totalCount": 1, "pageInfo": pageInfo{}, "nodes": []any{map[string]string{"login": "bob"}}}
					switch tt.metadata {
					case "labels":
						labels["totalCount"] = 3
						labels["pageInfo"] = pageInfo{HasNextPage: true, EndCursor: "next"}
					case "assignees":
						assignees["totalCount"] = 2
						assignees["pageInfo"] = pageInfo{HasNextPage: true, EndCursor: "next"}
					case "missing", "incomplete":
						delete(labels, "totalCount")
						delete(labels, "pageInfo")
					case "bad cursor":
						labels["pageInfo"] = pageInfo{HasNextPage: true}
					}
					var author any = map[string]string{"login": "alice"}
					if tt.metadata == "no author" {
						author = nil
					}
					node := map[string]any{"__typename": "Issue", "id": "I1", "number": 1, "title": "Task", "state": "OPEN", "repository": map[string]string{"nameWithOwner": "fixture/selector"}, "author": author, "assignees": assignees, "labels": labels}
					write(map[string]any{"data": map[string]any{"node": map[string]any{"items": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "P1", "statusValue": map[string]string{"name": func() string {
						if tt.metadata == "blank" {
							return ""
						}
						return "Todo"
					}()}, "content": node}}}}}})
				case strings.Contains(req.Query, "CandidateHydration"):
					enriched++
					data := map[string]any{"issue0": map[string]any{"id": "I1", "body": "Task", "comments": map[string]any{"totalCount": 0, "nodes": []any{}}, "blockedBy": map[string]any{"nodes": []any{}}}}
					addHydratedProjectFields(data, req.Variables)
					write(map[string]any{"data": data})
				default:
					fmt.Fprint(w, `{"data":{}}`)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/selector", ActiveStates: []string{"Todo"}})
			got := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Todo", "Backlog"}, tt.hint)
			if (got.CandidateError != nil) != tt.wantError {
				t.Fatalf("error=%v want error=%t", got.CandidateError, tt.wantError)
			}
			if (len(got.Candidates) == 1) != tt.want || (len(got.Statuses) == 1) != tt.want {
				t.Fatalf("candidates=%d statuses=%d want selected=%t", len(got.Candidates), len(got.Statuses), tt.want)
			}
			if !tt.want && enriched != 0 {
				t.Errorf("excluded issue enrichment=%d", enriched)
			}
			if tt.metadata == "blank" && len(c.refreshScan.blankStatuses) != 0 {
				t.Errorf("excluded repair targets=%v", c.refreshScan.blankStatuses)
			}
			if pages != tt.pages {
				t.Errorf("metadata pages=%d want=%d", pages, tt.pages)
			}
		})
	}
}

func TestRefreshSelectedEvidenceChangesWithoutIssueTimestamp(t *testing.T) {
	const stamp = "2026-09-16T20:00:00Z"
	cycle := 0
	hydration := map[string]int{}
	rest := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		if r.Method != http.MethodPost {
			rest[r.URL.Path]++
			fmt.Fprint(w, `[]`)
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
			for key, id := range req.Variables {
				if !strings.HasPrefix(key, "id") {
					continue
				}
				hydration[id.(string)]++
				deps := []any{}
				if cycle > 0 {
					deps = append(deps, map[string]any{"id": "I2", "number": 2, "state": "CLOSED", "repository": map[string]string{"nameWithOwner": "fixture/fresh"}})
				}
				data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": id, "updatedAt": stamp, "body": "Task", "comments": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "body": fmt.Sprintf("comment %d", cycle), "updatedAt": stamp}}}, "blockedBy": map[string]any{"nodes": deps}}
			}
			addHydratedProjectFields(data, req.Variables)
		case strings.Contains(req.Query, "CandidatePullRequestReferences"):
			refs := []any{}
			if cycle > 0 {
				refs = append(refs, candidatePRFixtureReference("fixture/fresh", 1))
			}
			data["nodes"] = []any{map[string]any{"id": "I1", "closedByPullRequestsReferences": map[string]any{"totalCount": len(refs), "nodes": refs}}}
			data["repo0"] = map[string]any{"pullRequests": map[string]any{"nodes": []any{}}}
		case strings.Contains(req.Query, "CandidatePullRequestStatus"):
			snapshot := candidatePRFixtureSnapshot("fixture/fresh", 1)
			candidateFixtureCommit(snapshot)["statusCheckRollup"] = nil
			data["pr0"] = map[string]any{"pullRequest": snapshot}
		case strings.Contains(req.Query, "items(first:"):
			items := []any{}
			for n := 1; n <= 2; n++ {
				labels := []any{}
				lane := "Done"
				if n == 1 {
					labels = append(labels, map[string]string{"name": "selected"})
					lane = "Human Review"
				}
				node := map[string]any{"__typename": "Issue", "id": fmt.Sprintf("I%d", n), "number": n, "title": "Task", "state": "OPEN", "updatedAt": stamp, "repository": map[string]string{"nameWithOwner": "fixture/fresh"}, "labels": map[string]any{"totalCount": len(labels), "pageInfo": pageInfo{}, "nodes": labels}}
				items = append(items, map[string]any{"id": fmt.Sprintf("P%d", n), "updatedAt": stamp, "statusValue": map[string]string{"name": lane}, "content": node})
			}
			data["node"] = map[string]any{"updatedAt": stamp, "items": map[string]any{"totalCount": 2, "nodes": items}}
		default:
			t.Errorf("unexpected query %s", req.Query)
		}
		write(map[string]any{"data": data})
	}))
	defer server.Close()
	c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/fresh", ActiveStates: []string{"Human Review"}})
	for nextCycle := range 2 {
		cycle = nextCycle
		got := c.FetchRefreshIssues(t.Context(), []string{"Human Review"}, []string{"Done"}, connector.IssueFilterHint{LabelInclude: []string{"selected"}})
		if got.CandidateError != nil || got.StatusError != nil || len(got.Candidates) != 1 || len(got.Statuses) != 0 {
			t.Fatalf("refresh %d: %+v", cycle, got)
		}
		issue := got.Candidates[0]
		if issue.Identifier != "fixture/fresh#1" || len(issue.Comments) != 1 || issue.Comments[0].Body != fmt.Sprintf("comment %d", cycle) || len(issue.BlockedBy) != cycle || (issue.PullRequest != nil) != (cycle == 1) {
			t.Fatalf("stale evidence in cycle %d: %+v", cycle, issue)
		}
		if cycle == 1 && (issue.BlockedBy[0].Identifier != "fixture/fresh#2" || issue.BlockedBy[0].State != "Done") {
			t.Fatalf("excluded dependency evidence: %+v", issue.BlockedBy)
		}
	}
	if hydration["I1"] != 2 || hydration["I2"] != 0 || len(rest) != 0 {
		t.Fatalf("two refreshes: hydration=%v REST=%v; want I1=2 I2=0 REST=0", hydration, rest)
	}
}
