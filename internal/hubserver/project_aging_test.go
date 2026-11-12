package hubserver

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	now := time.Date(2026, time.November, 11, 20, 0, 0, 0, time.UTC)
	var clock atomic.Value
	clock.Store(now.Add(-10 * time.Hour))
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return clock.Load().(time.Time) }})
	older := f.create(t, "older-created")
	clock.Store(now.Add(-9 * time.Hour))
	waiting := f.create(t, "longer-waiting")
	clock.Store(now)
	repository := f.service.config.TenantRepository
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
			if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				project.Settings.AgingEnabled = test.enabled
				if _, err := w.UpdateProjectRecord(t.Context(), project); err != nil {
					return err
				}
				for index, issue := range []tracker.NativeIssue{older, waiting} {
					wait, priority := test.oldWait, test.priority
					if index == 1 {
						wait, priority = test.wait, 2
					}
					if priority == 0 {
						priority = 2
					}
					facts, err := w.WorkItemFacts(t.Context(), f.project.ID, issue.WorkItemID)
					if err != nil {
						return err
					}
					facts.DispatchWaitSince = new(now.Add(-wait))
					if err := w.PutWorkItemFacts(t.Context(), facts); err != nil {
						return err
					}
					queue, err := w.QueueEntry(t.Context(), f.project.ID, issue.WorkItemID, string(f.project.ID))
					if err != nil {
						return err
					}
					queue.Priority = new(int64(priority))
					queue.Rank = ""
					queue.UpdatedAt = now
					if err := w.PutQueueEntry(t.Context(), queue); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			query := claimstore.CandidateQuery{ProjectID: f.project.ID, Scope: string(f.project.ID), AvailableAt: now, Limit: 1}
			var page, next claimstore.CandidatePage
			if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.CandidatePage(t.Context(), query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("candidates = %v", page.Items)
			}
			got := page.Items[0].WorkItemID
			if got != test.want {
				t.Fatalf("first candidate = %s, want %s", got, test.want)
			}
			query.After = page.Next
			err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				next, err = r.CandidatePage(t.Context(), query)
				return err
			})
			if err != nil || len(next.Items) != 1 || next.Items[0].WorkItemID == got {
				t.Fatalf("next page = %v, err = %v", next.Items, err)
			}
		})
	}
	for _, state := range []string{"Done", "Todo", "In Progress"} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(waiting.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "aging-" + state}, ExpectedRevision: waiting.Revision, State: state, Reason: "user_requested"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &waiting)
		var facts claimstore.WorkItemFacts
		if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			facts, err = r.WorkItemFacts(t.Context(), f.project.ID, waiting.WorkItemID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		wait := facts.DispatchWaitSince
		if state == "Done" {
			if wait != nil {
				t.Fatal("terminal work retained wait time")
			}
		} else if wait == nil || !wait.Equal(now) {
			t.Fatalf("%s wait = %v", state, wait)
		}
	}
}
