package nativework_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectAging(t *testing.T) {
	f := hubfixture.NewDefaultNative(t, hubserver.Config{})
	other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "other-project")
	read := func(f hubfixture.Native) hubserver.ProjectIntegration {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/integration", f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var value hubserver.ProjectIntegration
		hubfixture.Decode(t, response, &value)
		return value
	}
	initial := read(f)
	if initial.AgingEnabled || read(other).AgingEnabled {
		t.Fatal("aging must default off for both projects")
	}
	request := updateProjectIntegrationRequest{AgingEnabled: new(true), ExpectedRevision: initial.Revision, Intake: initial.Intake, Projection: initial.Projection, RepositoryEnabled: initial.RepositoryEnabled}
	request.IdempotencyKey = "enable-aging"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/integration", hubfixture.AdminToken, request), http.StatusOK)
	if !read(f).AgingEnabled || read(other).AgingEnabled {
		t.Fatal("aging change did not remain scoped to its project")
	}
	request.IdempotencyKey = "stale-aging"
	request.AgingEnabled = new(false)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/integration", hubfixture.AdminToken, request), http.StatusConflict)
	current := read(f)
	request.ExpectedRevision = current.Revision
	request.IdempotencyKey = "disable-aging"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/integration", hubfixture.AdminToken, request), http.StatusOK)
	if read(f).AgingEnabled || read(other).AgingEnabled {
		t.Fatal("aging was not disabled")
	}
}

func TestNativeCandidateAging(t *testing.T) {
	now := time.Date(2026, time.November, 11, 20, 0, 0, 0, time.UTC)
	var clock atomic.Value
	clock.Store(now.Add(-10 * time.Hour))
	f := hubfixture.NewDefaultNative(t, hubserver.Config{Clock: func() time.Time { return clock.Load().(time.Time) }})
	older := f.Create(t, "older-created")
	clock.Store(now.Add(-9 * time.Hour))
	waiting := f.Create(t, "longer-waiting")
	clock.Store(now)
	repository := f.Hub.Config.TenantRepository
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
			if err := repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.Project.ID)
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
					facts, err := w.WorkItemFacts(t.Context(), f.Project.ID, issue.WorkItemID)
					if err != nil {
						return err
					}
					facts.DispatchWaitSince = new(now.Add(-wait))
					if err := w.PutWorkItemFacts(t.Context(), facts); err != nil {
						return err
					}
					queue, err := w.QueueEntry(t.Context(), f.Project.ID, issue.WorkItemID, string(f.Project.ID))
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
			query := claimstore.CandidateQuery{ProjectID: f.Project.ID, Scope: string(f.Project.ID), AvailableAt: now, Limit: 1}
			var page, next claimstore.CandidatePage
			if err := repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
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
			err := repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
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
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(waiting.WorkItemID)+"/workflow", f.Token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "aging-" + state}, ExpectedRevision: waiting.Revision, State: state, Reason: "user_requested"})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		hubfixture.Decode(t, response, &waiting)
		var facts claimstore.WorkItemFacts
		if err := repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			facts, err = r.WorkItemFacts(t.Context(), f.Project.ID, waiting.WorkItemID)
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

type updateProjectIntegrationRequest struct {
	AgingEnabled              *bool           `json:"aging_enabled,omitempty"`
	DuplicateChallenge        *bool           `json:"duplicate_challenge,omitempty"`
	ArchiveCompletedAfterDays json.RawMessage `json:"archive_completed_after_days,omitempty"`
	ArchiveCancelledAfterDays json.RawMessage `json:"archive_cancelled_after_days,omitempty"`
	tracker.Mutation
	States            *[]tracker.NativeState `json:"states,omitempty"`
	WorkflowMarkdown  *string                `json:"workflow_markdown,omitempty"`
	ExpectedRevision  tracker.Revision       `json:"expected_revision,string"`
	Intake            string                 `json:"intake"`
	Projection        string                 `json:"projection"`
	RepositoryEnabled bool                   `json:"repository_enabled"`
}
