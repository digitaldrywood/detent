package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRetention(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state                                                                            string
		age                                                                                    int
		lease, expiredLease, openChange, landedChange, never, restore, terminalMove, noHistory bool
		period                                                                                 int
		archived                                                                               bool
	}{
		{name: "old Done", state: "Done", age: 31, archived: true},
		{name: "recent Done", state: "Done", age: 29},
		{name: "Done boundary", state: "Done", age: 30},
		{name: "old Cancelled", state: "Cancelled", age: 8, archived: true},
		{name: "recent Cancelled", state: "Cancelled", age: 6},
		{name: "live lease", state: "Done", age: 31, lease: true},
		{name: "expired lease", state: "Done", age: 31, lease: true, expiredLease: true, archived: true},
		{name: "open Change", state: "Done", age: 31, openChange: true},
		{name: "landed Change", state: "Done", age: 31, openChange: true, landedChange: true, archived: true},
		{name: "Never completed", state: "Done", age: 100, never: true},
		{name: "Never cancelled", state: "Cancelled", age: 100, never: true},
		{name: "restore resets clock", state: "Done", age: 100, restore: true},
		{name: "custom terminal", state: "Shipped", age: 31, archived: true},
		{name: "terminal move preserves clock", state: "Shipped", age: 31, terminalMove: true, archived: true},
		{name: "no terminal history", state: "Done", age: 100, noHistory: true},
		{name: "nonterminal", state: "Todo", age: 100},
		{name: "short completed period", state: "Done", age: 8, period: 7, archived: true},
		{name: "long cancelled period", state: "Cancelled", age: 8, period: 14},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			issue := f.create(t, "retention")
			if test.lease {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				lease := claimNativeAttempt(t, f, f.worker(t, "worker"), "machine", "session", issue.WorkItemID)
				if test.expiredLease {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(now.Add(-time.Hour)), lease.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.never || test.period != 0 {
				current, err := readProjectIntegration(t.Context(), f.service.database.db, scope)
				if err != nil {
					t.Fatal(err)
				}
				field := "archive_completed_after_days"
				if test.state == "Cancelled" {
					field = "archive_cancelled_after_days"
				}
				var period any
				if !test.never {
					period = test.period
				}
				response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, map[string]any{
					"idempotency_key": "set-period", "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled, field: period,
				})
				requireNativeStatus(t, response, http.StatusOK)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if test.state == "Shipped" || test.state == "Cancelled" {
				states := append(append([]tracker.NativeState{}, f.project.States...), tracker.NativeState{Name: test.state, Terminal: true})
				if err := updateNativeProjectStates(t.Context(), tx, scope, states, now); err != nil {
					t.Fatal(err)
				}
			}
			if test.openChange {
				change := tracker.ChangeRequest{ID: "chg_retention", OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, CurrentVersion: "cv_retention"}
				if test.landedChange {
					change.Landed = &tracker.ChangeLanding{VersionID: "cv_retention"}
				}
				raw, err := marshalNative(change)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,?)", change.ID, scope.organization, scope.project, issue.WorkItemID, raw); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_issue_links(change_id,organization_id,project_id,work_item_id) VALUES(?,?,?,?)", change.ID, scope.organization, scope.project, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,'{}')", change.CurrentVersion, change.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.state != "Todo" {
				issue.State = test.state
				if test.terminalMove {
					issue.State = "Done"
				}
				event := "workflow.transitioned"
				if test.noHistory {
					event = "issue.edited"
				}
				issue, err = persistNativeIssue(t.Context(), tx, scope, issue, event, tracker.CollaborationData{FromState: "Todo", ToState: issue.State}, now.Add(-time.Duration(test.age)*24*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if test.terminalMove {
					issue.State = test.state
					issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Done", ToState: test.state}, now.Add(-time.Hour))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.restore {
				issue, err = setNativeArchiveTx(t.Context(), tx, scope, string(issue.WorkItemID), issue.Revision, true, now.Add(-2*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				issue, err = setNativeArchiveTx(t.Context(), tx, scope, string(issue.WorkItemID), issue.Revision, false, now.Add(-time.Hour))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			got := readWorkItem(t, f, issue.WorkItemID, "")
			if got.Archived != test.archived {
				t.Fatalf("archived=%v, want %v", got.Archived, test.archived)
			}
			if !test.archived && got.Revision != issue.Revision {
				t.Fatalf("skipped issue changed revision: %d", got.Revision)
			}
			if test.archived {
				if got.Revision != issue.Revision+1 || got.State != issue.State || got.Body != issue.Body {
					t.Fatalf("archive changed content: %+v", got)
				}
				var data, actor string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT data_json,actor_json FROM collaboration_events WHERE work_item_id=? ORDER BY sequence DESC LIMIT 1", issue.WorkItemID).Scan(&data, &actor); err != nil {
					t.Fatal(err)
				}
				var event tracker.CollaborationData
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatal(err)
				}
				days := 30
				if test.state == "Cancelled" {
					days = 7
				}
				if test.period != 0 {
					days = test.period
				}
				if event.Operation != "archive" || event.ReasonDetail != fmt.Sprintf("Archived automatically after %d days", days) || actor != `{"kind":"integration","principal_id":"auto_archive"}` {
					t.Fatalf("archive event=%s actor=%s", data, actor)
				}
				if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				if repeated := readWorkItem(t, f, issue.WorkItemID, ""); repeated.Revision != got.Revision {
					t.Fatal("sweep archived twice")
				}
			}
			if test.restore || test.lease && !test.expiredLease || test.openChange && !test.landedChange {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET released_at=? WHERE released_at IS NULL", formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				if test.openChange {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_requests SET record_json=json_set(record_json,'$.landed.version_id','cv_retention') WHERE id='chg_retention'"); err != nil {
						t.Fatal(err)
					}
				}
				retryAt := now.Add(time.Minute)
				if test.restore {
					retryAt = now.Add(31 * 24 * time.Hour)
				}
				if err := f.service.maintainNativeRetention(t.Context(), retryAt); err != nil {
					t.Fatal(err)
				}
				if !readWorkItem(t, f, issue.WorkItemID, "").Archived {
					t.Fatal("eligible retry did not archive")
				}
			}
		})
	}
}

func TestProjectArchivePeriods(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	current, err := readProjectIntegration(t.Context(), f.service.database.db, scope)
	if err != nil {
		t.Fatal(err)
	}
	if current.ArchiveCompletedAfterDays == nil || *current.ArchiveCompletedAfterDays != 30 || current.ArchiveCancelledAfterDays == nil || *current.ArchiveCancelledAfterDays != 7 {
		t.Fatalf("defaults=%+v", current)
	}
	for index, test := range []struct {
		name                         string
		completed, cancelled         any
		sendCompleted, sendCancelled bool
		status                       int
		wantCompleted, wantCancelled int
	}{
		{name: "set", completed: 14, sendCompleted: true, status: 200, wantCompleted: 14, wantCancelled: 7},
		{name: "never", cancelled: nil, sendCancelled: true, status: 200, wantCompleted: 14},
		{name: "omit preserves", status: 200, wantCompleted: 14},
		{name: "invalid days", completed: 8, sendCompleted: true, status: 422, wantCompleted: 14},
		{name: "string", completed: "30", sendCompleted: true, status: 422, wantCompleted: 14},
		{name: "zero", completed: 0, sendCompleted: true, status: 422, wantCompleted: 14},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{"idempotency_key": fmt.Sprint("period-", index), "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled}
			if test.sendCompleted {
				payload["archive_completed_after_days"] = test.completed
			}
			if test.sendCancelled {
				payload["archive_cancelled_after_days"] = test.cancelled
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
			requireNativeStatus(t, response, test.status)
			current, err = readProjectIntegration(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			if current.ArchiveCompletedAfterDays == nil || *current.ArchiveCompletedAfterDays != test.wantCompleted {
				t.Fatalf("completed=%v", current.ArchiveCompletedAfterDays)
			}
			if test.wantCancelled == 0 && current.ArchiveCancelledAfterDays != nil || test.wantCancelled != 0 && (current.ArchiveCancelledAfterDays == nil || *current.ArchiveCancelledAfterDays != test.wantCancelled) {
				t.Fatalf("cancelled=%v", current.ArchiveCancelledAfterDays)
			}
		})
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "settings during work")
	claimNativeAttempt(t, f, f.worker(t, "settings-worker"), "machine", "session", issue.WorkItemID)
	payload := map[string]any{"idempotency_key": "live-settings", "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled, "archive_completed_after_days": 60}
	response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
	requireNativeStatus(t, response, http.StatusOK)
	var updated ProjectIntegration
	decodeHubResponse(t, response, &updated)
	if updated.ArchiveCompletedAfterDays == nil || *updated.ArchiveCompletedAfterDays != 60 {
		t.Fatalf("live setting not persisted: %+v", updated)
	}
	payload["idempotency_key"] = "live-workflow"
	payload["expected_revision"] = fmt.Sprint(updated.Revision)
	payload["states"] = f.project.States
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload), http.StatusUnprocessableEntity)

	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository='acme/checkout-only' WHERE id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	checkout, err := readProjectIntegration(t.Context(), f.service.database.db, scope)
	if err != nil {
		t.Fatal(err)
	}
	payload["idempotency_key"] = "checkout-only-period"
	payload["expected_revision"] = fmt.Sprint(checkout.Revision)
	payload["intake"] = checkout.Intake
	delete(payload, "states")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload), http.StatusOK)
	var intake string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT github_intake FROM projects WHERE id=?", f.project.ID).Scan(&intake); err != nil {
		t.Fatal(err)
	}
	if intake != "disabled" {
		t.Fatalf("archive-only save changed integration authority: %s", intake)
	}

}
