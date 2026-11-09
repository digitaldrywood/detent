package hubserver

import (
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceHubDispatchSharesBoundedActiveRunPage(t *testing.T) {
	for _, test := range []struct {
		name       string
		running    int
		runnerless int
		queued     int
		wantFirst  int
		wantSecond int
	}{
		{name: "dispatch cap", queued: 21, wantFirst: 20, wantSecond: 21},
		{name: "active run cursor", running: 100, queued: 1, wantSecond: 1},
		{name: "runnerless work cannot starve a live runner", runnerless: 20, queued: 1, wantSecond: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRelayFixture(t, withExec)
			f.dialRunner(t)
			action := f.action(t, "Example bounded dispatch", "printf example")
			var runnerless *relayFixture
			if test.runnerless > 0 {
				runnerless = startRelayFixture(t, &relayFixture{nativeFixture: f.nativeFixture, clock: f.at(), exec: true})
				if session := runnerless.record(t); session.State != workspacesession.StateReady || session.ProjectID != f.project.ID || f.service.workspaces.relay.runnerFor(runnerless.workspace) != nil {
					t.Fatalf("runnerless workspace fixture is not ready in the same project: %+v", session)
				}
			}
			now := f.at()
			lastRun := ""
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(workspaceWriter)
				for index := range test.running + test.runnerless + test.queued {
					workspace := f.workspace
					if index >= test.running && index < test.running+test.runnerless {
						workspace = runnerless.workspace
					}
					id := fmt.Sprintf("actionrun_%032x", index+1)
					row, err := owner.CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: id, ActionID: action.ID, WorkspaceID: workspace, CreatedBy: "test_hub_admin", CreatedAt: now})
					if err != nil {
						return err
					}
					ref := workspaceArchive(row.CommandObjectKey, row.CommandSha256, row.CommandBytes, row.CommandContentBytes)
					if err := keepWorkspaceArchive(t.Context(), owner, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, ref, "project-action-run", row.ID, now); err != nil {
						return err
					}
					if index < test.running {
						if _, err := owner.ClaimActionRun(t.Context(), postgres.ClaimActionRunInput{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: id, RunnerID: f.runner.binding.RunnerID, LeaseID: string(f.lease.ID), FencingToken: int64(f.lease.FencingToken), StartedAt: &now, ClaimedStreamID: "example:" + id}); err != nil {
							return err
						}
					}
					lastRun = id
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f.waitedOutTheGrace()
			running := func() int {
				t.Helper()
				var count int
				if err := workspaceTestDB(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM project_action_runs WHERE organization_id=$1 AND project_id=$2 AND action_id=$3 AND status='running'`, f.project.OrganizationID, f.project.ID, action.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			if count := running(); count != test.running {
				t.Fatalf("fixture has %d running runs, want %d", count, test.running)
			}
			f.dispatch(t)
			if count := running() - test.running; count != test.wantFirst {
				t.Fatalf("first dispatch started %d new runs, want %d", count, test.wantFirst)
			}
			f.dispatch(t)
			if count := running() - test.running; count != test.wantSecond {
				t.Fatalf("second dispatch left %d new running runs, want %d", count, test.wantSecond)
			}
			if last := f.runRow(t, lastRun); last.Status != workspacesession.RunRunning || last.StartedAt == nil {
				t.Fatalf("last live-runner run did not start: %+v", last)
			}
		})
	}
}
