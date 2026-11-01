package hubserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectAging(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
	read := func(f nativeFixture) ProjectIntegration {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/integration", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var value ProjectIntegration
		decodeHubResponse(t, response, &value)
		return value
	}
	initial := read(f)
	if initial.AgingEnabled || read(other).AgingEnabled {
		t.Fatal("aging must default off for both projects")
	}
	request := updateProjectIntegrationRequest{AgingEnabled: new(true), ExpectedRevision: initial.Revision, Intake: initial.Intake, Projection: initial.Projection, RepositoryEnabled: initial.RepositoryEnabled}
	request.IdempotencyKey = "enable-aging"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request), http.StatusOK)
	if !read(f).AgingEnabled || read(other).AgingEnabled {
		t.Fatal("aging change did not remain scoped to its project")
	}
	request.IdempotencyKey = "stale-aging"
	request.AgingEnabled = new(false)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request), http.StatusConflict)
	current := read(f)
	request.ExpectedRevision = current.Revision
	request.IdempotencyKey = "disable-aging"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request), http.StatusOK)
	if read(f).AgingEnabled || read(other).AgingEnabled {
		t.Fatal("aging was not disabled")
	}
}

func TestNativeCandidateAging(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	older := f.create(t, "older-created")
	waiting := f.create(t, "longer-waiting")
	now := f.service.config.now().Truncate(time.Second)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	for _, test := range []struct {
		name     string
		enabled  bool
		wait     time.Duration
		oldWait  time.Duration
		priority int
		want     tracker.NativeWorkItemID
	}{
		{name: "default unchanged", wait: 100 * 24 * time.Hour, want: older.WorkItemID},
		{name: "before first day", enabled: true, wait: 24*time.Hour - time.Second, want: older.WorkItemID},
		{name: "at first day", enabled: true, wait: 24 * time.Hour, want: waiting.WorkItemID},
		{name: "at bound", enabled: true, wait: 3 * 24 * time.Hour, oldWait: 2 * 24 * time.Hour, want: waiting.WorkItemID},
		{name: "beyond bound ties", enabled: true, wait: 100 * 24 * time.Hour, oldWait: 3 * 24 * time.Hour, want: older.WorkItemID},
		{name: "explicit priority preserved", enabled: true, wait: 100 * 24 * time.Hour, priority: 1, want: older.WorkItemID},
		{name: "future wait earns nothing", enabled: true, wait: -24 * time.Hour, want: older.WorkItemID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET aging_enabled=? WHERE id=?", test.enabled, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for index, issue := range []tracker.NativeIssue{older, waiting} {
				wait := test.oldWait
				priority := test.priority
				if index == 1 {
					wait, priority = test.wait, 2
				}
				if priority == 0 {
					priority = 2
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_created_at=?, dispatch_wait_since=? WHERE native_id=?", formatHubTime(now.Add(time.Duration(index-10)*time.Hour)), formatHubTime(now.Add(-wait)), issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override=? WHERE issue_id=(SELECT id FROM issues WHERE native_id=?)", priority, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			query := claimCandidateQuery{NativeScope: &scope, Scope: string(f.project.ID), AvailableAt: now, Limit: 1}
			ids, err := nativeCandidateIDs(t.Context(), f.service.database.reader, query, nil, nil, nil, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got tracker.NativeWorkItemID
			if len(ids) != 1 {
				t.Fatalf("candidates = %v", ids)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE id=?", ids[0]).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("first candidate = %s, want %s", got, test.want)
			}
			query.After = ids[0]
			next, err := nativeCandidateIDs(t.Context(), f.service.database.reader, query, nil, nil, nil, nil, nil, nil, nil)
			if err != nil || len(next) != 1 || next[0] == ids[0] {
				t.Fatalf("next page = %v, err = %v", next, err)
			}
		})
	}
	for _, state := range []string{"Done", "Todo", "In Progress"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET updated_at=?, workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) WHERE native_id=?", formatHubTime(now), f.project.ID, state, waiting.WorkItemID); err != nil {
			t.Fatal(err)
		}
		var wait *string
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT dispatch_wait_since FROM issues WHERE native_id=?", waiting.WorkItemID).Scan(&wait); err != nil {
			t.Fatal(err)
		}
		if state == "Done" {
			if wait != nil {
				t.Fatal("terminal work retained wait time")
			}
		} else if wait == nil || *wait != formatHubTime(now) {
			t.Fatalf("%s wait = %v", state, wait)
		}
	}
}
