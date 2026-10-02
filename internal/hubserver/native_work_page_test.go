package hubserver

import (
	"net/http"
	"net/url"
	"slices"
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
	seedArchiveIssues(t, f.service, scope, 130, "Todo")
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
