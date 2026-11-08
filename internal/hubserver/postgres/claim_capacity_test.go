package postgres_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimCapacityGroupsNativeLandingLeases(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, setup := range []struct {
		name     string
		github   bool
		occupied claimstore.CapacityUsage
		joining  claimstore.CapacityUsage
	}{
		{name: "native", occupied: claimstore.CapacityUsage{Host: 1, Runner: 1, Project: 1}},
		{name: "github", github: true, occupied: claimstore.CapacityUsage{Host: 2, Runner: 2, Project: 2}, joining: claimstore.CapacityUsage{Host: 2, Runner: 2, Project: 2}},
	} {
		for _, tenant := range []tracker.OrganizationID{"org_first", "org_second"} {
			org := tracker.OrganizationID(string(tenant) + "_" + setup.name)
			seedRepository(t, repository, org)
			history := seedHistory(t, repository, org, "capacity")
			now := time.Now().UTC()
			seedRunnerIdentity(t, repository, org, "runner_capacity", "machine_capacity", now, true)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				revision := policyTestRevision(org, "prj_first", "policy", now)
				*revision.GitHubPullRequest = setup.github
				if err := w.ApproveProjectPolicy(t.Context(), revision, ""); err != nil {
					return err
				}
				history.change.CurrentVersionID = new(history.version.ID)
				if _, err := w.UpdateChange(t.Context(), history.change); err != nil {
					return err
				}
				return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", ChangeID: history.change.ID, CreatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,operator_only,board_rank) VALUES($1,'prj_first','Merging',false,true,false,0)`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET workflow_state_name='Merging' WHERE organization_id=$1 AND project_id='prj_first'`, org); err != nil {
				t.Fatal(err)
			}
			for _, id := range []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"} {
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), "prj_first", id)
					if err != nil {
						return err
					}
					lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_capacity", SessionID: "landing/batch/" + string(id), LeaseID: "lease_" + string(id), TTL: time.Hour})
					if err != nil {
						return err
					}
					return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: "runner_capacity", LeaseID: lease.LeaseID, AssignedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, test := range []struct {
				name  string
				batch string
				now   time.Time
				want  claimstore.CapacityUsage
			}{
				{name: "batch occupies one slot", now: now, want: setup.occupied},
				{name: "joining batch", batch: "batch", now: now, want: setup.joining},
				{name: "another batch cannot reuse slot", batch: "other", now: now, want: setup.occupied},
				{name: "expired leases free slot", now: now.Add(2 * time.Hour)},
			} {
				t.Run(string(org)+"/"+test.name, func(t *testing.T) {
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						usage, err := r.ClaimCapacityUsage(t.Context(), "machine_capacity", "runner_capacity", "prj_first", test.batch, test.now)
						if err == nil && usage != test.want {
							t.Errorf("capacity = %+v, want %+v", usage, test.want)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
