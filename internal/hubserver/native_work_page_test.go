package hubserver

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 0 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", todo[0].WorkItemID); err != nil {
		t.Fatal(err)
	}
	otherProject := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-project")
	otherProject.create(t, "Historical needle")
	read := func(query string) tracker.NativeIssuePage {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&limit=100"+query, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
		return page
	}
	first := read("")
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
	if first.NextCursor == "" || first.Items[0].WorkItemID != history[0].WorkItemID || first.Work.Items[0].WorkItemID != ip[1].WorkItemID || !slices.ContainsFunc(first.Work.Items, func(issue tracker.NativeIssue) bool { return issue.WorkItemID == todo[0].WorkItemID }) {
		t.Fatal("default history page omitted operational work")
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
	_, err := readNativeWorkSummary(t.Context(), q, scope, "SELECT i.native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.organization_id = ? AND i.project_id = ? AND i.archived = 0", []any{scope.organization, scope.project}, 100, f.service.config.now())
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
