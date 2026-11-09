package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type workspaceClaimCommandFault struct {
	attachment.DetailStore
	mu     sync.Mutex
	key    string
	failAt int
	reads  int
	failed bool
}

func (s *workspaceClaimCommandFault) arm(key string, failAt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key, s.failAt, s.reads, s.failed = key, failAt, 0, false
}

func (s *workspaceClaimCommandFault) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	s.mu.Lock()
	if ref.Key == s.key {
		s.reads++
		if s.reads == s.failAt {
			s.failed = true
			s.mu.Unlock()
			return nil, errors.New("injected command archive read failure")
		}
	}
	s.mu.Unlock()
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestPostgresWorkspaceRelayClaimReadsCommandBeforeCommit(t *testing.T) {
	for _, source := range []string{"automatic", "person"} {
		t.Run(source, func(t *testing.T) {
			started := time.Now().UTC().Truncate(time.Microsecond)
			details := &workspaceClaimCommandFault{DetailStore: &memoryRunDetails{}}
			first := newWorkspaceHostedNativeService(t, Config{
				now: func() time.Time { return started }, RunDetails: details,
				Workspace: &WorkspaceConfig{Enabled: true, maintenanceTicks: make(chan time.Time)},
			})
			config := first.config
			workspace := first.workspaces.config
			workspace.maintenanceTicks = make(chan time.Time)
			config.Workspace = &workspace
			second := openWorkspaceTenantHub(t, config, workspaceTestDB(t, first), "org_first_second")
			hubs := []*Service{first, second}
			type savedRun struct {
				fixture *relayFixture
				run     string
				command string
				output  string
			}
			var saved []savedRun
			readRun := func(f *relayFixture, id string) postgres.ActionRun {
				t.Helper()
				var row postgres.ActionRun
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.(workspaceReader).ActionRunByID(t.Context(), id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return row
			}
			for tenant, hub := range hubs {
				organization := tracker.OrganizationID(hub.localOrganization)
				project := newNativeFixture(t, hub, organization, "Workspace claim project")
				approveHubTestPolicy(t, hub, project.base+"/policy", hubTestPolicy())
				for index := range 2 {
					f := startRelayFixture(t, &relayFixture{nativeFixture: project, clock: started, exec: true})
					runner := f.dialRunner(t)
					person := f.dialPerson(t)
					command := fmt.Sprintf("printf example-%d-%d", tenant, index)
					action := f.action(t, "Example action", command)
					run := f.queuedRun(t, action)
					queued := readRun(f, run)
					changed := command + "-changed"
					response := performHubAPIRequest(t, hub, http.MethodPatch, f.base+"/actions/"+action.ID, f.token, projectActionPatch{
						Mutation: tracker.Mutation{IdempotencyKey: newNativeID("actionkey")}, ExpectedRevision: tracker.Revision(action.Revision), Command: &changed,
					})
					requireNativeStatus(t, response, http.StatusOK)
					relay := hub.workspaces.relay
					relay.mu.Lock()
					connection := relay.rooms[f.workspace].runner
					var actor *relayConnection
					for _, candidate := range relay.rooms[f.workspace].people {
						actor = candidate
					}
					relay.mu.Unlock()
					if connection == nil || actor == nil {
						t.Fatal("workspace has no attached runner or person")
					}
					workspace := f.record(t)
					dispatch := func() {
						if source == "person" {
							hub.workspaces.handlePersonFrame(t.Context(), actor, workspace, execRunFrame(run, action))
							return
						}
						hub.workspaces.dispatchActionRun(t.Context(), actionRunFromRecord(queued))
					}
					failAt := 1
					if source == "person" {
						failAt = 2
					}
					details.arm(queued.CommandObjectKey, failAt)
					dispatch()
					details.mu.Lock()
					failed := details.failed
					details.mu.Unlock()
					if !failed {
						t.Error("dispatch did not reach the injected command read failure")
					}
					if source == "person" {
						if refusal := errorPayload(t, person.receive()); refusal.Code != workspacesession.CodeStaleExecution {
							t.Errorf("failed command preparation refusal=%+v", refusal)
						}
					}
					details.arm("", 0)
					if row := readRun(f, run); row.Status != workspacesession.RunQueued || row.StartedAt != nil || row.ClaimedBy != "" || row.ClaimedStreamID != "" || row.Revision != queued.Revision {
						t.Errorf("command read failure committed an execution claim: %+v", row)
						continue
					}
					dispatch()
					forwarded := runner.receive()
					var payload workspacesession.ExecRun
					decodeFramePayload(t, forwarded, &payload)
					if forwarded.Type != workspacesession.TypeExecRun || payload.RunID != run || payload.Command != command {
						t.Fatalf("recovered dispatch changed the committed command snapshot: %+v / %+v", forwarded, payload)
					}
					output := fmt.Sprintf("completed-%d-%d", tenant, index)
					hub.workspaces.handleRunnerFrame(t.Context(), connection, execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{Data: output}))
					hub.workspaces.handleRunnerFrame(t.Context(), connection, execExitedFrame(forwarded.Stream, 0))
					if row := f.runRow(t, run); row.Status != workspacesession.RunSucceeded || row.Command != command || row.Output != output {
						t.Errorf("recovered execution=%+v", row)
					}
					saved = append(saved, savedRun{fixture: f, run: run, command: command, output: output})
				}
			}
			for _, hub := range hubs {
				reopened := reopenWorkspaceHub(t, hub)
				for _, record := range saved {
					if record.fixture.project.OrganizationID != tracker.OrganizationID(reopened.localOrganization) {
						continue
					}
					record.fixture.service = reopened
					if row := record.fixture.runRow(t, record.run); row.Status != workspacesession.RunSucceeded || row.Command != record.command || row.Output != record.output {
						t.Errorf("reopened recovered execution=%+v", row)
					}
				}
			}
		})
	}
}
