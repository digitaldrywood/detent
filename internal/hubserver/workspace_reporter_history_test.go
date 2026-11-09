package hubserver

import (
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresWorkspaceReporterHistoryFiltersBeforeLimit(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Microsecond)
	clock := started
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	details := workspaceRecordingArchiveDetails{root: t.TempDir()}
	bootstrap := newWorkspaceNativeService(t, Config{now: now, RunDetails: details, Workspace: &WorkspaceConfig{Enabled: true, maintenanceTicks: make(chan time.Time)}})
	admin := workspaceTestDB(t, bootstrap)
	type pair struct {
		workspace, run, state string
		subject               tracker.NativeWorkItemID
	}
	type tenant struct {
		fixture  hostedSecurityFixture
		reporter hostedSecurityUser
		creator  string
		action   string
		own      []pair
		foreign  pair
	}
	var tenants []*tenant
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		hub := openWorkspaceTenantHub(t, bootstrap.config, admin, organization)
		project := newNativeFixture(t, hub, organization, "Reporter history project")
		fixture := hostedSecurityFixture{service: hub, provider: hub.config.Hosted.Provider.(*hostedSecurityProvider), project: project.project.ID, base: project.base}
		reporter, principal := workspaceRecordingReader(t, fixture, "history_reporter_"+string(organization), "reporter")
		_, creator := workspaceRecordingReader(t, fixture, "history_creator_"+string(organization), "owner")
		if principal == creator {
			t.Fatal("history creator must differ from the reporter principal")
		}
		credential, _, err := hub.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(project.token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: organization, project: project.project.ID, credential: credential}
		ownScope := scope
		ownScope.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: principal}
		ownSubjects := []tracker.NativeWorkItemID{
			workspaceTestIssue(t, hub, ownScope, "First reporter history subject", "Example history"),
			workspaceTestIssue(t, hub, ownScope, "Second reporter history subject", "Example history"),
		}
		foreignScope := scope
		foreignScope.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: creator}
		foreignSubject := workspaceTestIssue(t, hub, foreignScope, "Foreign history subject", "Example history")
		command, err := details.StoreDetail(t.Context(), string(organization), []byte("printf example"))
		if err != nil {
			t.Fatal(err)
		}
		record := &tenant{fixture: fixture, reporter: reporter, creator: creator, action: newNativeID("action")}
		if err := hub.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(workspaceWriter)
			if err := owner.CreateProjectAction(t.Context(), actionstore.Record{Action: workspacesession.Action{ID: record.action, OrganizationID: string(organization), ProjectID: string(project.project.ID), Name: "Example history action", Icon: "play", CreatedBy: creator, Revision: 1, CreatedAt: started, UpdatedAt: started}, Command: command}); err != nil {
				return err
			}
			if err := keepWorkspaceArchive(t.Context(), owner, scope, command, "project-action", record.action, started); err != nil {
				return err
			}
			seed := func(subject tracker.NativeWorkItemID, at time.Time, terminal string) (pair, error) {
				result := pair{workspace: newNativeID("ws"), run: newNativeID("actionrun"), state: terminal, subject: subject}
				row, err := owner.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: organization, ProjectID: project.project.ID, ID: result.workspace, SubjectWorkItemID: string(subject), Ref: "develop", Requires: []string{workspacesession.CapabilityFiles}, IdleTimeoutSeconds: 3600, ExpiresAt: at.Add(time.Hour), RequestedExpiresAt: at.Add(time.Hour), CreatedBy: creator, CreatedAt: at})
				if err != nil {
					return result, err
				}
				move := func(state string) error {
					var err error
					row, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: organization, ProjectID: project.project.ID, ID: result.workspace, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: state, UpdatedAt: at})
					return err
				}
				if err := move(workspacesession.StateStarting); err != nil {
					return result, err
				}
				run, err := owner.CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: organization, ProjectID: project.project.ID, ID: result.run, ActionID: record.action, WorkspaceID: result.workspace, CreatedBy: creator, CreatedAt: at})
				if err != nil {
					return result, err
				}
				if err := keepWorkspaceArchive(t.Context(), owner, scope, command, "project-action-run", run.ID, at); err != nil {
					return result, err
				}
				finished := at.Add(time.Microsecond)
				if _, err := owner.FinishActionRun(t.Context(), postgres.FinishActionRunInput{OrganizationID: organization, ProjectID: project.project.ID, ID: result.run, ExpectedRevision: run.Revision, Status: workspacesession.RunSucceeded, ExitCode: pgtype.Int4{Int32: 0, Valid: true}, FinishedAt: &finished}); err != nil {
					return result, err
				}
				if err := move(workspacesession.StateClosing); err != nil {
					return result, err
				}
				return result, move(terminal)
			}
			for index, subject := range ownSubjects {
				state := workspacesession.StateClosed
				if index == 1 {
					state = workspacesession.StateFailed
				}
				row, err := seed(subject, started.Add(time.Duration(index)*time.Microsecond), state)
				if err != nil {
					return err
				}
				record.own = append(record.own, row)
			}
			for index := range workspaceListLimit {
				row, err := seed(foreignSubject, started.Add(time.Second+time.Duration(index)*time.Microsecond), workspacesession.StateClosed)
				if err != nil {
					return err
				}
				record.foreign = row
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, record)
	}
	clockMu.Lock()
	clock = started.Add(time.Minute)
	clockMu.Unlock()
	verify := func(t *testing.T, record *tenant, other *tenant) {
		t.Helper()
		f := record.fixture
		listWorkspaces := func(suffix string, want []string) {
			t.Helper()
			response := f.request(t, record.reporter, http.MethodGet, f.base+"/workspaces"+suffix, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page workspaceListResponse
			decodeHubResponse(t, response, &page)
			ids := make([]string, 0, len(page.Workspaces))
			for _, workspace := range page.Workspaces {
				ids = append(ids, workspace.ID)
			}
			if !slices.Equal(ids, want) {
				t.Errorf("workspace list %q=%v, want readable history %v", suffix, ids, want)
			}
		}
		for _, expected := range record.own {
			response := f.request(t, record.reporter, http.MethodGet, f.base+"/workspaces/"+expected.workspace, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var workspace workspacesession.Session
			decodeHubResponse(t, response, &workspace)
			if workspace.ID != expected.workspace || workspace.WorkItemID != string(expected.subject) || workspace.CreatedBy != record.creator || workspace.State != expected.state {
				t.Errorf("readable workspace=%+v", workspace)
			}
			response = f.request(t, record.reporter, http.MethodGet, f.base+"/actions/"+record.action+"/runs/"+expected.run, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var run workspacesession.Run
			decodeHubResponse(t, response, &run)
			if run.ID != expected.run || run.WorkspaceID != expected.workspace || run.CreatedBy != record.creator || run.Status != workspacesession.RunSucceeded || run.Command != "printf example" {
				t.Errorf("readable action run=%+v", run)
			}
			listWorkspaces("?work_item="+string(expected.subject), []string{expected.workspace})
			listWorkspaces("?work_item="+string(expected.subject)+"&state="+expected.state, []string{expected.workspace})
		}
		listWorkspaces("", []string{record.own[1].workspace, record.own[0].workspace})
		listWorkspaces("?state=closed", []string{record.own[0].workspace})
		listWorkspaces("?state=failed", []string{record.own[1].workspace})
		for _, limit := range []int{1, 2} {
			response := f.request(t, record.reporter, http.MethodGet, fmt.Sprintf("%s/actions/%s/runs?limit=%d", f.base, record.action, limit), nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page projectActionRunList
			decodeHubResponse(t, response, &page)
			ids := make([]string, 0, len(page.Items))
			for _, run := range page.Items {
				ids = append(ids, run.ID)
			}
			want := []string{record.own[1].run, record.own[0].run}[:limit]
			if !slices.Equal(ids, want) {
				t.Errorf("action history limit=%d: got=%v want=%v", limit, ids, want)
			}
		}
		for _, denied := range []pair{record.foreign, other.own[0]} {
			requireNativeCode(t, f.request(t, record.reporter, http.MethodGet, f.base+"/workspaces/"+denied.workspace, nil), http.StatusNotFound, "not_found")
			requireNativeCode(t, f.request(t, record.reporter, http.MethodGet, f.base+"/actions/"+record.action+"/runs/"+denied.run, nil), http.StatusNotFound, "not_found")
		}
		requireNativeStatus(t, f.request(t, hostedSecurityUser{token: "invalid-history-session"}, http.MethodGet, f.base+"/workspaces", nil), http.StatusUnauthorized)
	}
	for index, record := range tenants {
		t.Run(fmt.Sprintf("tenant_%d", index), func(t *testing.T) { verify(t, record, tenants[(index+1)%len(tenants)]) })
	}
	for _, record := range tenants {
		record.fixture.service = reopenWorkspaceHub(t, record.fixture.service)
	}
	for index, record := range tenants {
		t.Run(fmt.Sprintf("reopened_tenant_%d", index), func(t *testing.T) { verify(t, record, tenants[(index+1)%len(tenants)]) })
	}
}
