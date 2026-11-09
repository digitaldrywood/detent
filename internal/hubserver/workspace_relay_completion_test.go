package hubserver

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresWorkspaceRelayCompletionRequiresCurrentLease(t *testing.T) {
	for _, test := range []struct {
		name        string
		wantSuccess bool
	}{
		{name: "live", wantSuccess: true},
		{name: "expired"},
		{name: "released"},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now().UTC().Truncate(time.Microsecond)
			clock := started
			var clockMu sync.Mutex
			now := func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return clock
			}
			first := newWorkspaceHostedNativeService(t, Config{
				now: now,
				Workspace: &WorkspaceConfig{
					Enabled: true, MaxLifetime: time.Hour, IdleTimeout: time.Hour,
					maintenanceTicks: make(chan time.Time),
				},
			})
			config := first.config
			workspace := first.workspaces.config
			workspace.maintenanceTicks = make(chan time.Time)
			config.Workspace = &workspace
			second := openWorkspaceTenantHub(t, config, workspaceTestDB(t, first), "org_first_second")
			hubs := []*Service{first, second}
			type admittedRun struct {
				fixture *relayFixture
				runner  *relayConnection
				stream  string
				run     string
				action  string
				lease   leasestore.Lease
			}
			var admitted []admittedRun
			latestExpiry := started
			for _, hub := range hubs {
				organization := tracker.OrganizationID(hub.localOrganization)
				project := newNativeFixture(t, hub, organization, "Workspace completion project")
				approveHubTestPolicy(t, hub, project.base+"/policy", hubTestPolicy())
				for index := range 2 {
					f := startRelayFixture(t, &relayFixture{nativeFixture: project, clock: started, exec: true})
					runner := f.dialRunner(t)
					person := f.dialPerson(t)
					action := f.action(t, fmt.Sprintf("Completion action %d", index), "printf example")
					run := f.queuedRun(t, action)
					person.send(execRunFrame(run, action))
					forwarded := runner.receive()
					if forwarded.Type != workspacesession.TypeExecRun || forwarded.Stream == "" {
						t.Fatalf("workspace %s admitted frame=%+v", f.workspace, forwarded)
					}
					if row := f.runRow(t, run); row.Status != workspacesession.RunRunning || row.StartedAt == nil {
						t.Fatalf("workspace %s admitted run=%+v", f.workspace, row)
					}
					connection := hub.workspaces.relay.runnerFor(f.workspace)
					if connection == nil {
						t.Fatalf("workspace %s has no admitted runner connection", f.workspace)
					}
					var lease leasestore.Lease
					if err := hub.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						lease, err = r.Lease(t.Context(), string(f.lease.ID))
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if lease.ReleasedAt != nil || !lease.ExpiresAt.After(now()) || lease.FencingToken != int64(f.lease.FencingToken) {
						t.Fatalf("workspace %s admitted lease=%+v", f.workspace, lease)
					}
					if lease.ExpiresAt.After(latestExpiry) {
						latestExpiry = lease.ExpiresAt
					}
					admitted = append(admitted, admittedRun{fixture: f, runner: connection, stream: forwarded.Stream, run: run, action: action.ID, lease: lease})
				}
			}
			switch test.name {
			case "expired":
				clockMu.Lock()
				clock = latestExpiry
				clockMu.Unlock()
			case "released":
				for _, record := range admitted {
					f := record.fixture
					if err := f.service.database.postgres.WriteLeases(t.Context(), f.project.OrganizationID, corestore.Serving, func(w leasestore.Writer) error {
						lease, err := w.Release(t.Context(), leasestore.Fence{LeaseID: record.lease.LeaseID, FencingToken: record.lease.FencingToken}, "released")
						if err == nil && lease.ReleasedAt == nil {
							t.Errorf("workspace %s lease release has no release time", f.workspace)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertCompletion := func(record admittedRun, phase string) {
				t.Helper()
				row := record.fixture.runRow(t, record.run)
				if row.WorkspaceID != record.fixture.workspace || row.ActionID != record.action {
					t.Fatalf("%s run identity=%+v", phase, row)
				}
				if test.wantSuccess {
					if row.Status != workspacesession.RunSucceeded || row.ExitCode == nil || *row.ExitCode != 0 || row.FinishedAt == nil {
						t.Errorf("%s live workspace %s completion=%+v", phase, record.fixture.workspace, row)
					}
					return
				}
				if row.Status == workspacesession.RunSucceeded || row.ExitCode != nil && *row.ExitCode == 0 {
					t.Errorf("%s %s lease published successful completion for workspace %s: %+v", phase, test.name, record.fixture.workspace, row)
				}
			}
			for _, record := range admitted {
				record.fixture.service.workspaces.handleRunnerFrame(t.Context(), record.runner, execExitedFrame(record.stream, 0))
				assertCompletion(record, "before restart")
				if !test.wantSuccess {
					relay := record.fixture.service.workspaces.relay
					relay.mu.Lock()
					room := relay.rooms[record.fixture.workspace]
					retained := room != nil && room.execRuns[record.stream] != nil
					relay.mu.Unlock()
					if !retained {
						t.Errorf("workspace %s stale completion discarded teardown ownership", record.fixture.workspace)
					}
				}
			}
			for tenant, hub := range hubs {
				reopened := reopenWorkspaceHub(t, hub)
				hubs[tenant] = reopened
				for index := range admitted {
					record := &admitted[index]
					if record.fixture.project.OrganizationID == tracker.OrganizationID(reopened.localOrganization) {
						record.fixture.service = reopened
						assertCompletion(*record, "after restart")
						if !test.wantSuccess {
							row := record.fixture.runRow(t, record.run)
							if row.Status != workspacesession.RunFailed || row.FinishedAt == nil || row.Reason != workspacesession.RunReasonStreamClosed {
								t.Errorf("workspace %s stale completion did not retain shutdown cleanup: %+v", record.fixture.workspace, row)
							}
						}
					}
				}
			}
		})
	}
}
