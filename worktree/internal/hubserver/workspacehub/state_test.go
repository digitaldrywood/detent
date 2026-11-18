package workspacehub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type workspaceTransitionTestWriter struct {
	postgres.WorkspaceWriter
	closingWrites   int
	repeatedClosing error
}

func (w *workspaceTransitionTestWriter) UpdateWorkspaceSession(ctx context.Context, arg Update) (Row, error) {
	if arg.State == workspacesession.StateClosing {
		w.closingWrites++
		if w.closingWrites > 1 {
			return Row{}, w.repeatedClosing
		}
	}
	row, err := w.WorkspaceWriter.UpdateWorkspaceSession(ctx, postgres.WorkspaceUpdate(arg))
	return Row(row), err
}

func TestWorkspaceStateRejectsIllegalDetourWithoutCommitting(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository := hubtest.Repository(t, hubtest.WithClock(func() time.Time { return now }))
	details := hubtest.NewDetails()
	for range 2 {
		organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
		project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
		runner := hubtest.Runner(t, repository, organization, hubtest.RunnerOptions{Projects: []tracker.ProjectID{project.ID}, Now: now})
		for _, test := range []struct{ name, from, target string }{
			{"starting cannot reach idle through closing", workspacesession.StateStarting, workspacesession.StateIdle},
			{"closing cannot return to ready", workspacesession.StateClosing, workspacesession.StateReady},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				subject := hubtest.Issue(t, repository, details, project, hubtest.IssueOptions{Now: now})
				item := hubtest.Issue(t, repository, details, project, hubtest.IssueOptions{Now: now})
				lease := hubtest.Lease(t, repository, runner, item, hubtest.LeaseOptions{TTL: time.Hour, Now: now})
				var before postgres.WorkspaceSession
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					owner := w.(postgres.WorkspaceWriter)
					var err error
					before, err = owner.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{
						OrganizationID: organization, ProjectID: project.ID, ID: hubapi.NewID("ws"), SubjectWorkItemID: string(subject.Row.NativeID), Requires: []string{"files"}, IdleTimeoutSeconds: 1800, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: "fixture", CreatedAt: now,
					})
					if err != nil {
						return err
					}
					if err := owner.PutWorkspaceItem(t.Context(), claimstore.WorkspaceItem{OrganizationID: organization, ProjectID: project.ID, WorkItemID: item.Row.NativeID, WorkspaceID: before.ID, CreatedAt: now}); err != nil {
						return err
					}
					arg := TenantUpdate(Row(before), workspacesession.StateStarting, "", now)
					arg.RunnerID, arg.MachineID, arg.LeaseID, arg.FencingToken = runner.Identity.RunnerID, string(runner.Identity.MachineID), string(lease.ID), int64(lease.FencingToken)
					before, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate(arg))
					if err != nil {
						return err
					}
					if err := owner.CreateWorkspaceOccupancy(t.Context(), postgres.WorkspaceOccupancyStart{OrganizationID: organization, ProjectID: project.ID, WorkspaceID: before.ID, RunnerID: before.RunnerID, StartedAt: now}); err != nil {
						return err
					}
					if before.State != test.from {
						before, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate(TenantUpdate(Row(before), test.from, "", now)))
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var expires time.Time
				var released bool
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					current, err := r.Lease(t.Context(), string(lease.ID))
					expires, released = current.ExpiresAt, current.ReleasedAt != nil
					return err
				}); err != nil {
					t.Fatal(err)
				}
				repeated := errors.New("workspace repeated the closing detour")
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					owner := &workspaceTransitionTestWriter{WorkspaceWriter: w.(postgres.WorkspaceWriter), repeatedClosing: repeated}
					planned := Row(before)
					planned.HeadSHA = "uncommitted-example-head"
					_, err := UpdateState(t.Context(), owner, planned, Row(before), test.target, "", now)
					return err
				})
				var refusal *hubapi.Error
				if !errors.As(err, &refusal) || refusal.Code != "stale_execution" {
					t.Fatalf("illegal target returned %v, want stale_execution", err)
				}
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					owner := r.(postgres.WorkspaceReader)
					after, err := owner.WorkspaceSession(t.Context(), project.ID, before.ID)
					if err != nil {
						return err
					}
					if after.State != before.State || after.Revision != before.Revision || after.HeadSHA != before.HeadSHA || after.LeaseID != before.LeaseID || after.FencingToken != before.FencingToken {
						t.Fatalf("illegal transition committed changes: before=%+v after=%+v", before, after)
					}
					current, err := r.Lease(t.Context(), string(lease.ID))
					if err != nil {
						return err
					}
					if !current.ExpiresAt.Equal(expires) || (current.ReleasedAt != nil) != released {
						t.Fatal("illegal transition changed the lease")
					}
					dispatch, err := owner.WorkspaceDispatchItem(t.Context(), project.ID, before.ID)
					if err != nil {
						return err
					}
					if dispatch.ClosedAt != nil {
						t.Fatal("illegal transition closed the dispatch association")
					}
					occupancy, err := owner.WorkspaceOccupancy(t.Context(), project.ID, before.ID, 10)
					if err != nil {
						return err
					}
					open := 0
					for _, row := range occupancy {
						if row.EndedAt == nil {
							open++
						}
					}
					if len(occupancy) != 1 || open != 1 {
						t.Fatalf("illegal transition changed occupancy: rows=%d open=%d", len(occupancy), open)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
