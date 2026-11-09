package hubserver

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresLocalWorkspaceMaintenanceExpiresEveryTenant(t *testing.T) {
	clock := time.Now().UTC().Truncate(time.Microsecond)
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	service := newWorkspaceNativeService(t, Config{now: now, Workspace: &WorkspaceConfig{Enabled: true, RequestTimeout: time.Minute, maintenanceTicks: make(chan time.Time)}})
	type savedWorkspace struct {
		organization tracker.OrganizationID
		project      tracker.ProjectID
		base         string
		token        string
		id           string
		dispatch     string
		terminal     string
	}
	var saved []savedWorkspace
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, organization, "Example workspace project")
		terminal, found := firstTerminalState(fixture.project)
		if !found {
			t.Fatal("fixture has no terminal workflow state")
		}
		for index := range 2 {
			subject := fixture.create(t, fmt.Sprintf("Example workspace subject %d", index))
			body := workspaceRequest{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("workspace-%d", index)}, WorkItemID: string(subject.WorkItemID), Requires: []string{workspacesession.CapabilityFiles}}
			response := performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/workspaces", fixture.token, body)
			requireNativeStatus(t, response, http.StatusCreated)
			var session workspacesession.Session
			decodeHubResponse(t, response, &session)
			if session.State != workspacesession.StateRequested {
				t.Fatalf("workspace state=%q want requested", session.State)
			}
			record := savedWorkspace{organization: organization, project: fixture.project.ID, base: fixture.base, token: fixture.token, id: session.ID, terminal: terminal}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				record.dispatch, err = r.(workspaceReader).WorkspaceDispatchItem(t.Context(), fixture.project.ID, session.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			saved = append(saved, record)
		}
	}
	clockMu.Lock()
	clock = clock.Add(service.workspaces.config.RequestTimeout)
	clockMu.Unlock()
	service.workspaces.sweep(t.Context())
	failed := 0
	states := make(map[tracker.OrganizationID]string)
	for _, record := range saved {
		response := performHubAPIRequest(t, service, http.MethodGet, record.base+"/workspaces/"+record.id, record.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var session workspacesession.Session
		decodeHubResponse(t, response, &session)
		if session.State == workspacesession.StateFailed {
			failed++
		} else if session.State != workspacesession.StateRequested {
			t.Fatalf("first maintenance tick organization=%s workspace=%s state=%q", record.organization, record.id, session.State)
		}
		if previous := states[record.organization]; previous != "" && previous != session.State {
			t.Fatalf("first maintenance tick partially visited organization=%s", record.organization)
		}
		states[record.organization] = session.State
	}
	if failed != 2 {
		t.Fatalf("first maintenance tick expired %d workspaces, want one tenant's two workspaces", failed)
	}
	service.workspaces.sweep(t.Context())
	verify := func() {
		t.Helper()
		for _, record := range saved {
			response := performHubAPIRequest(t, service, http.MethodGet, record.base+"/workspaces/"+record.id, record.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var session workspacesession.Session
			decodeHubResponse(t, response, &session)
			if session.State != workspacesession.StateFailed || session.Reason != workspacesession.ReasonNoRunner {
				t.Fatalf("organization=%s workspace=%s state=%q reason=%q", record.organization, record.id, session.State, session.Reason)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), record.organization, corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(workspaceReader)
				association, err := owner.WorkspaceClaimSession(t.Context(), record.project, tracker.NativeWorkItemID(record.dispatch))
				if err != nil {
					return err
				}
				if association.ClosedAt == nil {
					t.Fatalf("organization=%s workspace=%s dispatch association is open", record.organization, record.id)
				}
				dispatch, err := r.WorkItem(t.Context(), record.project, tracker.NativeWorkItemID(record.dispatch))
				if err != nil {
					return err
				}
				if stringValue(dispatch.WorkflowStateName) != record.terminal {
					t.Fatalf("organization=%s dispatch=%s state=%q want %q", record.organization, record.dispatch, stringValue(dispatch.WorkflowStateName), record.terminal)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	verify()
	service = reopenWorkspaceHub(t, service)
	verify()
}
