package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type workspaceBindCommandChange struct {
	attachment.DetailStore
	mu        sync.Mutex
	key       string
	afterRead func()
}

func (s *workspaceBindCommandChange) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	raw, err := s.DetailStore.ReadDetail(ctx, organization, ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var afterRead func()
	if ref.Key == s.key {
		afterRead, s.afterRead = s.afterRead, nil
	}
	s.mu.Unlock()
	if afterRead != nil {
		afterRead()
	}
	return raw, nil
}

func TestPostgresWorkspaceBindReadsCreationCommandsBeforeCommit(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Microsecond)
	details := &workspaceClaimCommandFault{DetailStore: &memoryRunDetails{}}
	changes := &workspaceBindCommandChange{DetailStore: details}
	first := newWorkspaceHostedNativeService(t, Config{
		now: func() time.Time { return started }, RunDetails: changes,
		Workspace: &WorkspaceConfig{Enabled: true, maintenanceTicks: make(chan time.Time)},
	})
	config := first.config
	workspace := first.workspaces.config
	workspace.maintenanceTicks = make(chan time.Time)
	config.Workspace = &workspace
	second := openWorkspaceTenantHub(t, config, workspaceTestDB(t, first), "org_first_second")
	hubs := []*Service{first, second}
	type pendingBind struct {
		fixture *workspaceRunnerFixture
		session workspacesession.Session
		lease   tracker.NativeLease
		before  postgres.WorkspaceSession
		action  workspacesession.Action
		command string
		archive string
	}
	var pending []pendingBind
	read := func(t *testing.T, f *workspaceRunnerFixture, id string) (postgres.WorkspaceSession, []postgres.WorkspaceOccupancy) {
		t.Helper()
		var row postgres.WorkspaceSession
		var occupancy []postgres.WorkspaceOccupancy
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			owner := r.(workspaceReader)
			var err error
			row, err = owner.WorkspaceSession(t.Context(), f.project.ID, id)
			if err != nil {
				return err
			}
			occupancy, err = owner.WorkspaceOccupancy(t.Context(), f.project.ID, id, 10)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return row, occupancy
	}
	capabilities := workspacesession.Capabilities{Files: true, Diff: true, Exec: true}
	for tenant, hub := range hubs {
		project := newNativeFixture(t, hub, tracker.OrganizationID(hub.localOrganization), "Workspace bind project")
		policy := hubTestPolicy()
		approveHubTestPolicy(t, hub, project.base+"/policy", policy)
		f := newWorkspaceRunnerFixtureFor(t, workspaceFixture{nativeFixture: project, policy: policy.ID, issue: project.create(t, "Workspace bind subject")})
		f.reportRunner(t, 8, capabilities, workspacesession.IsolationContainer)
		command := fmt.Sprintf("printf example-creation-%d", tenant)
		response := performHubAPIRequest(t, hub, http.MethodPost, f.base+"/actions", f.token, projectActionRequest{
			Mutation: tracker.Mutation{IdempotencyKey: newNativeID("actionkey")}, Name: "Example creation action", Command: command, RunOnWorktreeCreation: true,
		})
		requireNativeStatus(t, response, http.StatusCreated)
		var action workspacesession.Action
		decodeHubResponse(t, response, &action)
		var archiveKey string
		if err := hub.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.(actionstore.Reader).ProjectAction(t.Context(), f.project.ID, action.ID)
			archiveKey = row.Command.Key
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			session, lease := f.requested(t)
			before, occupancy := read(t, f, session.ID)
			if before.State != workspacesession.StateRequested || before.Revision != 1 || before.RunnerID != "" || before.MachineID != "" || before.LeaseID != "" || before.FencingToken != 0 || len(occupancy) != 0 {
				t.Fatalf("unbound workspace=%+v occupancy=%+v", before, occupancy)
			}
			details.arm(archiveKey, 1)
			response := f.workerPost(t, session.ID, "bind", workspaceBindRequest{workspaceWorkerIdentity: workspaceIdentity(lease), Capabilities: capabilities, Isolation: workspacesession.IsolationContainer})
			requireNativeStatus(t, response, http.StatusInternalServerError)
			details.mu.Lock()
			failed := details.failed
			details.mu.Unlock()
			if !failed {
				t.Fatal("bind did not read the creation command archive")
			}
			after, occupancy := read(t, f, session.ID)
			if !reflect.DeepEqual(after, before) || len(occupancy) != 0 {
				t.Errorf("failed bind changed workspace: before=%+v after=%+v occupancy=%+v", before, after, occupancy)
			}
			details.arm("", 0)
			pending = append(pending, pendingBind{fixture: f, session: session, lease: lease, before: before, action: action, command: command, archive: archiveKey})
		}
	}
	for _, hub := range hubs {
		reopened := reopenWorkspaceHub(t, hub)
		for _, record := range pending {
			if record.fixture.project.OrganizationID == tracker.OrganizationID(reopened.localOrganization) {
				record.fixture.service = reopened
			}
		}
	}
	t.Run("creation action changed after preparation", func(t *testing.T) {
		record := pending[0]
		f := record.fixture
		command := record.command + "-edited"
		var changed bool
		changes.mu.Lock()
		changes.key = record.archive
		changes.afterRead = func() {
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/actions/"+record.action.ID, f.token, projectActionPatch{
				Mutation: tracker.Mutation{IdempotencyKey: newNativeID("actionkey")}, ExpectedRevision: tracker.Revision(record.action.Revision), Command: &command,
			})
			requireNativeStatus(t, response, http.StatusOK)
			var action workspacesession.Action
			decodeHubResponse(t, response, &action)
			for index := range pending {
				if pending[index].fixture.project.OrganizationID == f.project.OrganizationID && pending[index].fixture.project.ID == f.project.ID {
					pending[index].action, pending[index].command = action, command
				}
			}
			changed = true
		}
		changes.mu.Unlock()
		response := f.workerPost(t, record.session.ID, "bind", workspaceBindRequest{workspaceWorkerIdentity: workspaceIdentity(record.lease), Capabilities: capabilities, Isolation: workspacesession.IsolationContainer})
		if response.Code != http.StatusConflict {
			t.Errorf("bind after creation action changed status=%d, want %d", response.Code, http.StatusConflict)
		}
		if !changed {
			t.Fatal("creation command was not changed after its archive read")
		}
		after, occupancy := read(t, f, record.session.ID)
		if !reflect.DeepEqual(after, record.before) || len(occupancy) != 0 {
			t.Errorf("stale creation command bind changed workspace: before=%+v after=%+v occupancy=%+v", record.before, after, occupancy)
		}
	})
	for _, record := range pending {
		t.Run(string(record.fixture.project.OrganizationID)+"/"+record.session.ID, func(t *testing.T) {
			f := record.fixture
			after, occupancy := read(t, f, record.session.ID)
			if !reflect.DeepEqual(after, record.before) || len(occupancy) != 0 {
				t.Errorf("reopened failed bind changed workspace: before=%+v after=%+v occupancy=%+v", record.before, after, occupancy)
			}
			response := f.workerPost(t, record.session.ID, "bind", workspaceBindRequest{workspaceWorkerIdentity: workspaceIdentity(record.lease), Capabilities: capabilities, Isolation: workspacesession.IsolationContainer})
			requireNativeStatus(t, response, http.StatusOK)
			var bound workspaceBindResponse
			decodeHubResponse(t, response, &bound)
			wantOwner := workspacesession.Owner{WorkspaceID: record.session.ID, RunnerID: f.runner.binding.RunnerID, MachineID: string(f.runner.binding.MachineID), LeaseID: string(record.lease.ID), FencingToken: int64(record.lease.FencingToken)}
			if bound.Owner != wantOwner || bound.Session.State != workspacesession.StateStarting || bound.Session.Revision != record.session.Revision+1 || bound.Checkout.Worktree != workspacesession.WorktreeFresh || bound.Checkout.ReadOnly {
				t.Fatalf("recovered bind=%+v, want owner=%+v and fresh writable checkout", bound, wantOwner)
			}
			if len(bound.Actions) != 1 || bound.Actions[0].ID != record.action.ID || bound.Actions[0].Command != record.command || !bound.Actions[0].RunOnWorktreeCreation {
				t.Fatalf("recovered creation commands=%+v", bound.Actions)
			}
			after, occupancy = read(t, f, record.session.ID)
			if after.State != workspacesession.StateStarting || after.Revision != record.before.Revision+1 || after.RunnerID != string(wantOwner.RunnerID) || after.MachineID != wantOwner.MachineID || after.LeaseID != wantOwner.LeaseID || after.FencingToken != wantOwner.FencingToken || len(occupancy) != 1 || occupancy[0].EndedAt != nil {
				t.Fatalf("durable recovered bind=%+v occupancy=%+v", after, occupancy)
			}
		})
	}
}
