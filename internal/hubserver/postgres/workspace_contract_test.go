package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func workspaceContractFixture(t *testing.T) (*sql.DB, *postgres.Repository, *pgxpool.Pool, time.Time) {
	t.Helper()
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	now := historyAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		for i := int64(1); i <= 2; i++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
			id := fmt.Sprintf("ws_%032x", i)
			runner := fmt.Sprintf("runner_%d", i)
			seedRunnerIdentity(t, repository, org, runner, fmt.Sprintf("machine_%d", i), now, false)
			lease := claimLease(t, repository, org, leaseClaim(i))
			err := repository.WriteWorkspace(t.Context(), org, corestore.Serving, func(w postgres.WorkspaceWriter) error {
				created, err := w.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: org, ProjectID: project, ID: id, SubjectWorkItemID: fmt.Sprintf("wi_%d", i), Ref: "refs/heads/example", Requires: []string{"files", "diff"}, IdleTimeoutSeconds: 300, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: string(org), CreatedAt: now})
				if err != nil {
					return err
				}
				if created.Revision != 1 || created.State != "requested" {
					t.Fatalf("created=%+v", created)
				}
				if err := w.PutWorkspaceItem(t.Context(), claimstore.WorkspaceItem{OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", i)), WorkspaceID: id, CreatedAt: now}); err != nil {
					return err
				}
				update := postgres.WorkspaceUpdate{OrganizationID: org, ProjectID: project, ID: id, ExpectedRevision: 1, State: "starting", RunnerID: runner, MachineID: fmt.Sprintf("machine_%d", i), LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, UpdatedAt: now}
				bound, err := w.UpdateWorkspaceSession(t.Context(), update)
				if err != nil {
					return err
				}
				if bound.Revision != 2 || bound.RunnerID != runner {
					t.Fatalf("bound=%+v", bound)
				}
				occupancy := postgres.WorkspaceOccupancyStart{OrganizationID: org, ProjectID: project, WorkspaceID: id, RunnerID: runner, StartedAt: now}
				if err := w.CreateWorkspaceOccupancy(t.Context(), occupancy); err != nil {
					return err
				}
				return w.CreateWorkspaceOccupancy(t.Context(), occupancy)
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return admin, repository, pool, now
}

func TestWorkspaceContractFencesSessionsAndPersistsOccupancy(t *testing.T) {
	admin, repository, pool, now := workspaceContractFixture(t)
	for _, test := range []struct {
		name string
		arg  postgres.WorkspaceUpdate
		want error
	}{
		{"foreign tenant", postgres.WorkspaceUpdate{OrganizationID: "org_second", ProjectID: "prj_1", ID: fmt.Sprintf("ws_%032x", 1)}, corestore.ErrTenantMismatch},
		{"stale revision", postgres.WorkspaceUpdate{OrganizationID: "org_first", ProjectID: "prj_1", ID: fmt.Sprintf("ws_%032x", 1), ExpectedRevision: 1}, corestore.ErrRevisionConflict},
		{"stale owner", postgres.WorkspaceUpdate{OrganizationID: "org_first", ProjectID: "prj_1", ID: fmt.Sprintf("ws_%032x", 1), ExpectedRevision: 2, ExpectedRunnerID: "foreign"}, corestore.ErrRevisionConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := repository.WriteWorkspace(t.Context(), "org_first", corestore.Serving, func(w postgres.WorkspaceWriter) error {
				_, err := w.UpdateWorkspaceSession(t.Context(), test.arg)
				return err
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
	err := repository.WriteWorkspace(t.Context(), "org_first", corestore.Serving, func(w postgres.WorkspaceWriter) error {
		id := fmt.Sprintf("ws_%032x", 1)
		row, err := w.WorkspaceSession(t.Context(), "prj_1", id)
		if err != nil {
			return err
		}
		_, err = w.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: "org_first", ProjectID: "prj_1", ID: id, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: "ready", RunnerID: row.RunnerID, MachineID: row.MachineID, LeaseID: row.LeaseID, FencingToken: row.FencingToken + 1, UpdatedAt: now, Capabilities: []string{"files"}})
		return err
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("invalid lease fence=%v", err)
	}
	var count int
	if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM workspace_sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbound count=%d err=%v", count, err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadWorkspace(t.Context(), org, corestore.Serving, func(r postgres.WorkspaceReader) error {
			expired, err := r.ExpiringWorkspaces(t.Context(), now.Add(time.Hour), 2)
			if err != nil {
				return err
			}
			if len(expired) != 2 {
				t.Fatalf("expiry=%+v", expired)
			}
			for i := 1; i <= 2; i++ {
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
				id := fmt.Sprintf("ws_%032x", i)
				rows, err := r.WorkspaceSubjectSessions(t.Context(), project, fmt.Sprintf("wi_%d", i), "", 2)
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].OrganizationID != org || rows[0].State != "starting" || rows[0].RunnerID != fmt.Sprintf("runner_%d", i) {
					t.Fatalf("sessions=%+v", rows)
				}
				if _, err := r.WorkspaceSession(t.Context(), "prj_missing", id); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("cross-project read=%v", err)
				}
				usage, err := r.WorkspaceOccupancy(t.Context(), project, id, 2)
				if err != nil {
					return err
				}
				if len(usage) != 1 || usage[0].OrganizationID != org || !usage[0].StartedAt.Equal(now) {
					t.Fatalf("occupancy=%+v", usage)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
