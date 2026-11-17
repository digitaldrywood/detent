package workspacehub

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type lifecycleReader struct{ postgres.WorkspaceReader }

func (r lifecycleReader) WorkspaceOpenSessions(ctx context.Context, after time.Time, id string, limit int) ([]Row, error) {
	rows, err := r.WorkspaceReader.WorkspaceOpenSessions(ctx, after, id, limit)
	if err != nil {
		return nil, err
	}
	var result []Row
	for _, row := range rows {
		result = append(result, Row(row))
	}
	return result, nil
}
func (r lifecycleReader) WorkspaceSession(ctx context.Context, project tracker.ProjectID, id string) (Row, error) {
	row, err := r.WorkspaceReader.WorkspaceSession(ctx, project, id)
	return Row(row), err
}

type lifecycleWriter struct {
	lifecycleReader
	owner postgres.WorkspaceWriter
}

func (w lifecycleWriter) UpdateWorkspaceSession(ctx context.Context, arg Update) (Row, error) {
	row, err := w.owner.UpdateWorkspaceSession(ctx, postgres.WorkspaceUpdate(arg))
	return Row(row), err
}
func (w lifecycleWriter) CloseWorkspaceItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, at time.Time) error {
	return w.owner.CloseWorkspaceItem(ctx, project, item, at)
}
func (w lifecycleWriter) EndWorkspaceOccupancy(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, id, runner string, at time.Time) error {
	return w.owner.EndWorkspaceOccupancy(ctx, postgres.WorkspaceOccupancyEnd{OrganizationID: organization, ProjectID: project, WorkspaceID: id, RunnerID: runner, EndedAt: &at})
}

func TestWorkspaceSweepCursorBoundsTenantPages(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository := hubtest.Repository(t, hubtest.WithClock(func() time.Time { return now }))
	details := hubtest.NewDetails()
	var services []*Service
	var projects []tracker.ProjectID
	for range 2 {
		organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
		project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
		subject := hubtest.Issue(t, repository, details, project, hubtest.IssueOptions{Now: now})
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for index := range 101 {
				if _, err := w.(postgres.WorkspaceWriter).CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: organization, ProjectID: project.ID, ID: fmt.Sprintf("ws_%032x", index+1), SubjectWorkItemID: string(subject.Row.NativeID), Requires: []string{"files"}, IdleTimeoutSeconds: 60, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: "fixture", CreatedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		service := New(Deps{
			Repository: repository, Organization: func() tracker.OrganizationID { return organization }, Now: func() time.Time { return now },
			Reader: func(r tenantstore.Reader) (Reader, error) { return lifecycleReader{r.(postgres.WorkspaceReader)}, nil },
			Writer: func(w tenantstore.Writer) (Writer, error) {
				owner := w.(postgres.WorkspaceWriter)
				return lifecycleWriter{lifecycleReader{owner}, owner}, nil
			},
			PrepareIssueClose: func(ctx context.Context, org tracker.OrganizationID, project tracker.ProjectID, item string, at time.Time) (func(context.Context, tenantstore.Writer) error, error) {
				if item != "" {
					t.Fatalf("unassociated workspace acquired a dispatch item: %q", item)
				}
				return func(context.Context, tenantstore.Writer) error { return nil }, nil
			},
		})
		services = append(services, service)
		projects = append(projects, project.ID)
	}
	for selected, service := range services {
		var first Record
		for page, want := range []int{100, 1, 100} {
			records, err := service.Due(t.Context(), now)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != want {
				t.Fatalf("tenant %d page %d rows=%d want=%d", selected, page, len(records), want)
			}
			for _, record := range records {
				if record.OrganizationID != service.deps.Organization() {
					t.Fatalf("tenant cursor crossed organization: %+v", record)
				}
			}
			switch page {
			case 0:
				first = records[0]
			case 1:
				if records[0].ID != fmt.Sprintf("ws_%032x", 101) {
					t.Fatalf("cursor skipped or repeated an equal-expiry row: %+v", records)
				}
			case 2:
				if records[0].ID != first.ID {
					t.Fatalf("cursor failed to wrap: first=%q got=%q", first.ID, records[0].ID)
				}
			}
		}
		moved, changed, err := service.ApplyDeadline(t.Context(), first, now.Add(time.Minute))
		if err != nil || !changed || moved.State != workspacesession.StateFailed || moved.Reason != workspacesession.ReasonNoRunner {
			t.Fatalf("deadline changed=%t row=%+v err=%v", changed, moved, err)
		}
		again, changed, err := service.ApplyDeadline(t.Context(), first, now.Add(time.Minute))
		if err != nil || changed || again.Revision != moved.Revision {
			t.Fatalf("stale sweep changed=%t row=%+v err=%v", changed, again, err)
		}
		for tenant, other := range services {
			if err := repository.ReadTenant(t.Context(), other.deps.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
				owner, err := other.deps.Reader(r)
				if err != nil {
					return err
				}
				row, err := owner.WorkspaceSession(t.Context(), projects[tenant], first.ID)
				if err != nil {
					return err
				}
				want := workspacesession.StateRequested
				if tenant <= selected {
					want = workspacesession.StateFailed
				}
				if row.State != want {
					t.Fatalf("tenant %d changed another tenant %d: state=%q want=%q", selected, tenant, row.State, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
