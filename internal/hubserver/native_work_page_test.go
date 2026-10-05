package hubserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkPageOperationalScope(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	history := seedArchiveIssues(t, f.service, scope, 130, "Done")
	todo := seedArchiveIssues(t, f.service, scope, 4, "Todo")
	ip := seedArchiveIssues(t, f.service, scope, 2, "In Progress")
	worker := f.worker(t, "work-page-worker")
	var activeLease tracker.NativeLease
	for index, issue := range ip {
		lease := claimNativeAttempt(t, f, worker, "work-page-machine-"+string(issue.WorkItemID), "work-page-session-"+string(issue.WorkItemID), issue.WorkItemID)
		start := nativeStartedEvent(lease)
		start.IdempotencyKey = string(issue.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
		if index == 0 {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at = ? WHERE lease_id = ?", formatHubTime(time.Now().Add(-time.Hour)), lease.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			activeLease = lease
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET labels_json = '[\"selected\"]', assignees_json = '[\"operator\"]' WHERE native_id = ?", todo[0].WorkItemID); err != nil {
		t.Fatal(err)
	}
	for _, issue := range []tracker.NativeIssue{history[120], todo[1]} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title = ?, labels_json = '[\"alternate\",\"Historical label\"]', assignees_json = '[\"reviewer\"]' WHERE native_id = ?", "Historical needle "+strconv.Itoa(issue.Number), issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 1 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET labels_json = '[\"human-owned\"]' WHERE native_id = ?", todo[2].WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies (dependent_issue_id, blocker_issue_id, provenance, created_at, updated_at) SELECT a.id, b.id, 'native', ?, ? FROM issues a, issues b WHERE a.native_id = ? AND b.native_id = ?", testTimestamp, testTimestamp, todo[3].WorkItemID, todo[2].WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 0 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", todo[0].WorkItemID); err != nil {
		t.Fatal(err)
	}
	otherProject := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-project")
	otherProject.create(t, "Historical needle")
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx = f.service.withOperatorCatalog(ctx, credential, string(f.project.OrganizationID))
	transports := map[string]func(string, string, any) hostedContextReply{}
	for _, name := range []string{"stdio", "http"} {
		transports[name] = hostedContextProtocol(t, f.service, ctx, name)
	}
	read := func(query string) tracker.NativeIssuePage {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&limit=100"+query, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
		params, err := url.ParseQuery("include=work&limit=100" + query)
		if err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"project_id": string(f.project.ID), "include": []string{"work"}, "limit": 100}
		for _, field := range []string{"state", "label", "assignee"} {
			if values := params[field]; len(values) != 0 {
				args[field+"s"] = values
			}
		}
		for _, field := range []string{"cursor", "archived"} {
			if value := params.Get(field); value != "" {
				args[field] = value
			}
		}
		if value := params.Get("q"); value != "" {
			args["query"] = value
		}
		if values := params["priority"]; len(values) != 0 {
			priorities := []int{}
			for _, value := range values {
				priority, err := strconv.Atoi(value)
				if err != nil {
					t.Fatal(err)
				}
				priorities = append(priorities, priority)
			}
			args["priorities"] = priorities
		}
		for transport, call := range transports {
			raw := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
			var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
			if err := json.Unmarshal(raw, &result); err != nil || result.Data.Work == nil || !reflect.DeepEqual(result.Data.Work.Lanes, page.Work.Lanes) || len(result.Data.Work.Items) != len(page.Work.Items) || len(result.Data.Items) != len(page.Items) {
				t.Fatalf("%s query=%s result=%s err=%v", transport, query, raw, err)
			}
			for index, issue := range page.Items {
				if result.Data.Items[index].WorkItemID != issue.WorkItemID {
					t.Fatalf("%s dropped filter for %s", transport, query)
				}
			}
			for index, issue := range page.Work.Items {
				if result.Data.Work.Items[index].WorkItemID != issue.WorkItemID {
					t.Fatalf("%s lost operational selection for %s", transport, query)
				}
			}
		}
		return page
	}
	first := read("")
	for transport, call := range transports {
		for _, name := range []string{operatortool.Dashboard, operatortool.BoardState} {
			t.Run(transport+"/"+name, func(t *testing.T) {
				catalog := call("tools/list", "", nil).Result
				if !strings.Contains(string(catalog), `"name":"`+name+`"`) {
					t.Fatalf("native read missing from catalog: %s", catalog)
				}
				for _, test := range []struct {
					state  string
					counts nativeBoardCounts
				}{
					{"", nativeBoardCounts{Running: 1, QueuedInventory: 5, Open: 6, ClosedInventory: 130, Total: 136}},
					{"Todo", nativeBoardCounts{QueuedInventory: 4, Open: 4, Total: 4}},
					{"Done", nativeBoardCounts{ClosedInventory: 130, Total: 130}},
				} {
					if name == operatortool.Dashboard && test.state != "" {
						continue
					}
					args := map[string]any{"project_id": string(f.project.ID), "limit": 100}
					if test.state != "" {
						args["state"] = test.state
					}
					raw := hostedContextData(t, call("tools/call", name, args), false)
					var result nativeBoardResult
					if err := json.Unmarshal(raw, &result); err != nil || result.Counts != test.counts || len(result.Projects) != 1 || result.Projects[0].Project.ID != f.project.ID || result.GeneratedAt.IsZero() || result.Truncated != (test.counts.Total > 100) {
						t.Fatalf("native inventory: %s err=%v", raw, err)
					}
					if !slices.Contains(result.Unavailable, "aggregate_dispatch_readiness") || result.EligibilityTool != operatortool.ExplainItem || strings.Contains(string(raw), `"ready"`) || strings.Contains(string(raw), "foreign-project") {
						t.Fatalf("inventory claimed readiness or leaked scope: %s", raw)
					}
					if test.state != "Done" && len(result.Projects[0].Board.Work.Items) != test.counts.Open {
						t.Fatalf("lost bounded operational items: %s", raw)
					}
				}
				hostedContextData(t, call("tools/call", name, map[string]any{"project_id": "prj_foreign"}), true)
				hostedContextData(t, call("tools/call", name, map[string]any{"project_id": string(f.project.ID), "limit": 201}), true)
				raw := hostedContextData(t, call("tools/call", name, map[string]any{"limit": 1}), false)
				var bounded nativeBoardResult
				if err := json.Unmarshal(raw, &bounded); err != nil || !bounded.Truncated || len(bounded.Projects) != 1 || len(bounded.Projects[0].Board.Items) > 1 || len(bounded.Projects[0].Board.Work.Items) > 1 {
					t.Fatalf("unscoped read exceeded bounds: %s err=%v", raw, err)
				}
			})
		}
	}
	for _, test := range []struct {
		name, query          string
		total, running, open int
	}{
		{"default", "", 136, 1, 6},
		{"history cursor", "&cursor=" + url.QueryEscape(first.NextCursor), 136, 1, 6},
		{"terminal lane", "&state=Done", 130, 0, 0},
		{"dispatchable lane", "&state=Todo", 4, 0, 4},
		{"IP membership", "&state=In%20Progress", 2, 1, 2},
		{"label", "&label=selected", 1, 0, 1},
		{"assignee", "&assignee=operator", 1, 0, 1},
		{"archived", "&archived=true", 0, 0, 0},
		{"multiple states", "&state=Todo&state=Done", 134, 0, 4},
		{"multiple dimensions", "&state=Todo&state=Done&label=selected&label=alternate&assignee=operator&assignee=reviewer&priority=0&priority=1", 3, 0, 2},
		{"older title", "&q=Historical%20needle", 2, 0, 1},
		{"older label", "&q=historical%20LABEL", 2, 0, 1},
		{"older identifier", "&q=" + url.QueryEscape(f.project.Name+"#"+strconv.Itoa(history[120].Number)), 1, 0, 0},
		{"native identifier", "&q=" + url.QueryEscape(string(f.project.ID)+"#"+strconv.Itoa(history[120].Number)), 1, 0, 0},
		{"body is not operational search", "&q=Preserved%20body", 0, 0, 0},
		{"AND dimensions", "&state=In%20Progress&label=selected&label=alternate", 0, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := read(test.query)
			if page.Work == nil || len(page.Items) > 100 || len(page.Work.Items) != test.open || page.Work.Truncated || page.Work.AsOf.IsZero() {
				t.Fatalf("page lost bounds or open selection: %+v", page.Work)
			}
			total, running := 0, 0
			for _, lane := range page.Work.Lanes {
				total += lane.Total
				running += lane.Running
			}
			if total != test.total || running != test.running {
				t.Fatalf("scope total=%d running=%d, want %d/%d", total, running, test.total, test.running)
			}
			for _, issue := range append(page.Items, page.Work.Items...) {
				if issue.Body != "" || issue.ProjectID != f.project.ID {
					t.Fatal("compact projection loaded a body or foreign project")
				}
			}
		})
	}
	if first.NextCursor == "" || first.Items[0].WorkItemID != todo[0].WorkItemID || first.Work.Items[0].WorkItemID != ip[1].WorkItemID || !slices.ContainsFunc(first.Work.Items, func(issue tracker.NativeIssue) bool { return issue.WorkItemID == todo[0].WorkItemID }) {
		t.Fatal("default lane page omitted operational work")
	}
	now := time.Now().Truncate(time.Second).Add(500 * time.Millisecond)
	f.service.config.now = func() time.Time { return now }
	for _, test := range []struct {
		name             string
		renewed, expires time.Time
		running          int
	}{
		{"mixed timestamp precision", now.Truncate(time.Second), now.Add(time.Hour), 1},
		{"expiry boundary", now.Add(-time.Minute), now, 0},
		{"future renewal", now.Add(time.Second), now.Add(time.Hour), 0},
		{"restore current lease", now.Add(-time.Second), now.Add(time.Hour), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET renewed_at = ?, expires_at = ? WHERE lease_id = ?", formatHubTime(test.renewed), formatHubTime(test.expires), activeLease.ID); err != nil {
				t.Fatal(err)
			}
			running := 0
			for _, lane := range read("").Work.Lanes {
				running += lane.Running
			}
			if running != test.running {
				t.Fatalf("running=%d, want %d", running, test.running)
			}
		})
	}
	more := seedArchiveIssues(t, f.service, scope, 130, "Todo")
	for _, issue := range more {
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title = 'Open needle', labels_json = '[\"open-match\"]', assignees_json = '[\"reviewer\"]' WHERE native_id = ?", issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 1 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
	}
	query := "&q=Open%20needle&state=Todo&state=Done&label=open-match&label=selected&assignee=operator&assignee=reviewer&priority=0&priority=1"
	matching := read(query)
	found := map[tracker.NativeWorkItemID]bool{}
	for {
		total := 0
		for _, lane := range matching.Work.Lanes {
			total += lane.Total
		}
		if total != 130 || len(matching.Items) > 100 || len(matching.Work.Items) > 100 {
			t.Fatalf("matching scope or bounds changed: %+v", matching.Work)
		}
		for _, issue := range matching.Items {
			if issue.ProjectID != f.project.ID || issue.Title != "Open needle" || issue.Body != "" {
				t.Fatalf("matching query leaked an item: %+v", issue)
			}
			found[issue.WorkItemID] = true
		}
		if matching.NextCursor == "" {
			break
		}
		matching = read(query + "&cursor=" + url.QueryEscape(matching.NextCursor))
	}
	if len(found) != 130 || !found[more[0].WorkItemID] {
		t.Fatal("older open match was unreachable")
	}
	for _, invalid := range []string{
		"&q=one&q=two", "&include=work", "&state=" + strings.Repeat("x", 4097),
		strings.Repeat("&state=Todo", 33), query + "&q=changed&cursor=" + url.QueryEscape(read(query).NextCursor),
	} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&limit=100"+invalid, f.token, nil), http.StatusUnprocessableEntity)
	}
	changed := strings.Replace(query, "state=Todo", "state=In%20Progress", 1)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&limit=100"+changed+"&cursor="+url.QueryEscape(read(query).NextCursor), f.token, nil), http.StatusUnprocessableEntity)
	page := read("")
	if !page.Work.Truncated || len(page.Work.Items) != 100 || page.Work.Items[0].WorkItemID != ip[1].WorkItemID {
		t.Fatal("large open inventory displaced live work or exceeded the bound")
	}
	q := &runtimeReadQuery{nativeQueryer: f.service.database.db}
	_, err = readNativeWorkSummary(t.Context(), q, scope, "SELECT i.native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.organization_id = ? AND i.project_id = ? AND i.archived = 0", []any{scope.organization, scope.project}, 100, f.service.config.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(q.statements) > 302 {
		t.Fatalf("compact read exceeded bounded hydration: %d queries", len(q.statements))
	}
	for _, statement := range q.statements {
		if strings.Contains(statement, "i.body") || strings.Contains(statement, "collaboration_events") || strings.Contains(statement, "workflow_history") {
			t.Fatalf("operational read loaded bodies or history: %s", statement)
		}
	}
	t.Logf("%d compact queries for at most 100 open items; aggregate totals independent of 130 history items", len(q.statements))
}
