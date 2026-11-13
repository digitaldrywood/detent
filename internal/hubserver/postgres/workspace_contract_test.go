package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func workspaceContractFixture(t *testing.T) (*sql.DB, *postgres.Repository, *pgxpool.Pool, time.Time) {
	t.Helper()
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		for i := int64(1); i <= 2; i++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
			id := fmt.Sprintf("ws_%032x", i)
			runner := fmt.Sprintf("runner_%d", i)
			pgstoretest.SeedRunnerIdentity(t, repository, org, runner, fmt.Sprintf("machine_%d", i), now, false)
			lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(i))
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
	t.Run("open scan pages both projects without crossing tenant boundaries", func(t *testing.T) {
		_, repository, _, now := workspaceContractFixture(t)
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if err := repository.ReadWorkspace(t.Context(), org, corestore.Background, func(r postgres.WorkspaceReader) error {
				first, err := r.WorkspaceOpenSessions(t.Context(), time.Time{}, "", 1)
				if err != nil {
					return err
				}
				if len(first) != 1 || first[0].OrganizationID != org || first[0].ProjectID != "prj_1" || first[0].ID != "ws_00000000000000000000000000000001" || !first[0].ExpiresAt.Equal(now.Add(time.Hour)) {
					t.Fatalf("first page: %+v", first)
				}
				second, err := r.WorkspaceOpenSessions(t.Context(), first[0].ExpiresAt, first[0].ID, 1)
				if err != nil {
					return err
				}
				if len(second) != 1 || second[0].OrganizationID != org || second[0].ProjectID != "prj_2" || second[0].ID != "ws_00000000000000000000000000000002" {
					t.Fatalf("second page: %+v", second)
				}
				end, err := r.WorkspaceOpenSessions(t.Context(), second[0].ExpiresAt, second[0].ID, 1)
				if err != nil {
					return err
				}
				if len(end) != 0 {
					t.Fatalf("scan repeated prior rows: %+v", end)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("expired owner can close without losing the original lease", func(t *testing.T) {
		admin, repository, _, now := workspaceContractFixture(t)
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for index := int64(1); index <= 2; index++ {
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", index))
				id := fmt.Sprintf("ws_%032x", index)
				if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET renewed_at=acquired_at,expires_at=acquired_at+interval '1 microsecond' WHERE organization_id=$1 AND issue_id=$2`, org, index); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteWorkspace(t.Context(), org, corestore.Serving, func(w postgres.WorkspaceWriter) error {
					current, err := w.WorkspaceSession(t.Context(), project, id)
					if err != nil {
						return err
					}
					arg := postgres.WorkspaceUpdate{OrganizationID: org, ProjectID: project, ID: id, ExpectedRevision: current.Revision, ExpectedRunnerID: current.RunnerID, ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken, State: "closing", RunnerID: current.RunnerID, MachineID: current.MachineID, LeaseID: current.LeaseID, FencingToken: current.FencingToken + 1, UpdatedAt: now.Add(time.Hour)}
					if _, err := w.UpdateWorkspaceSession(t.Context(), arg); !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("close replaced expired lease fence: %v", err)
					}
					arg.FencingToken = current.FencingToken
					deadline := arg.UpdatedAt.Add(time.Minute)
					arg.State, arg.Reason, arg.RebindDeadline = "requested", "hub_restarted", &deadline
					recovered, err := w.UpdateWorkspaceSession(t.Context(), arg)
					if err != nil {
						return err
					}
					if recovered.RebindDeadline == nil || !recovered.RebindDeadline.Equal(deadline) || recovered.LeaseID != current.LeaseID || recovered.FencingToken != current.FencingToken {
						t.Fatalf("recovery discarded original cleanup authority: %+v", recovered)
					}
					arg.ExpectedRevision = recovered.Revision
					arg.State, arg.Reason, arg.RebindDeadline = "starting", "", nil
					arg.FencingToken++
					if _, err := w.UpdateWorkspaceSession(t.Context(), arg); !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("expired recovery tuple authorized a new bind: %v", err)
					}
					arg.FencingToken = current.FencingToken
					arg.State = "closing"
					closing, err := w.UpdateWorkspaceSession(t.Context(), arg)
					if err != nil {
						return err
					}
					arg.State, arg.ExpectedRevision = "closed", closing.Revision
					closed, err := w.UpdateWorkspaceSession(t.Context(), arg)
					if err != nil {
						return err
					}
					if closed.LeaseID != current.LeaseID || closed.FencingToken != current.FencingToken || closed.RunnerID != current.RunnerID || closed.MachineID != current.MachineID || closed.State != "closed" {
						t.Fatalf("terminal workspace lost cleanup lease: before=%+v after=%+v", current, closed)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	t.Run("upgrade preserves omitted ref and read only at revision one", func(t *testing.T) {
		admin, repository, _, now := workspaceContractFixture(t)
		if _, err := admin.ExecContext(t.Context(), `DROP INDEX IF EXISTS workspace_subject_state_created; DROP INDEX IF EXISTS workspace_project_state_created; DROP INDEX IF EXISTS workspace_project_created; ALTER TABLE workspace_sessions ADD CONSTRAINT workspace_sessions_ref_check CHECK (btrim(ref)<>'')`); err != nil {
			t.Fatal(err)
		}
		_, err := admin.ExecContext(t.Context(), `INSERT INTO workspace_sessions(organization_id,project_id,id,subject_work_item_id,ref,requires,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at,state) VALUES ('org_first','prj_1','ws_upgrade_rejected','wi_1','',ARRAY['files'],60,$1,$1,'example',$2,$2,'requested')`, now.Add(time.Hour), now)
		if err == nil || !strings.Contains(err.Error(), "workspace_sessions_ref_check") {
			t.Fatalf("prior deployed ref constraint did not refuse omitted ref: %v", err)
		}
		raw, err := os.ReadFile("migrations/20261111194120_workspace_optional_ref.sql")
		if err != nil {
			t.Fatal(err)
		}
		up, _, ok := strings.Cut(string(raw), "-- +goose Down")
		if !ok {
			t.Fatal("workspace ref migration has no Down boundary")
		}
		upgrade, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = upgrade.Rollback() })
		if _, err := upgrade.ExecContext(t.Context(), up); err != nil {
			t.Fatal(err)
		}
		if err := upgrade.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for index := int64(1); index <= 2; index++ {
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", index))
				id := fmt.Sprintf("ws_%032x", 100+index)
				err := repository.WriteWorkspace(t.Context(), org, corestore.Serving, func(w postgres.WorkspaceWriter) error {
					created, err := w.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: org, ProjectID: project, ID: id, SubjectWorkItemID: fmt.Sprintf("wi_%d", index), Ref: "", ReadOnly: index == 2, Requires: []string{"files"}, IdleTimeoutSeconds: 60, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: fmt.Sprintf("creator_%d", index), CreatedAt: now})
					if err != nil {
						return err
					}
					if created.Ref != "" || created.ReadOnly != (index == 2) || created.Revision != 1 || created.State != "requested" {
						t.Fatalf("upgraded workspace changed original request contract: %+v", created)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.WriteWorkspace(t.Context(), org, corestore.Serving, func(w postgres.WorkspaceWriter) error {
				for index := int64(1); index <= 2; index++ {
					row, err := w.WorkspaceSession(t.Context(), tracker.ProjectID(fmt.Sprintf("prj_%d", index)), fmt.Sprintf("ws_%032x", 100+index))
					if err != nil {
						return err
					}
					if row.Ref != "" || row.ReadOnly != (index == 2) || row.Revision != 1 {
						t.Fatalf("second entity overwrote first workspace: %+v", row)
					}
					total, mine, err := w.WorkspaceOpenCounts(t.Context(), fmt.Sprintf("creator_%d", index), 20)
					if err != nil {
						return err
					}
					if total != 4 || mine != 1 {
						t.Fatalf("workspace admission crossed entity scope: total=%d mine=%d", total, mine)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
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
	repository, pool = pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
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

	t.Run("activity preserves lifecycle and current ownership", func(t *testing.T) {
		counter := new(pgstoretest.ReportCountingTracer)
		counted, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, counter), pgstoretest.SmallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := counted.Close(); err != nil {
				t.Error(err)
			}
		})
		type scope struct {
			organization tracker.OrganizationID
			project      tracker.ProjectID
			id           string
		}
		expected := make(map[scope]postgres.WorkspaceSession)
		update := func(key scope, current postgres.WorkspaceSession, state string, at time.Time) postgres.WorkspaceSession {
			t.Helper()
			var row postgres.WorkspaceSession
			if err := repository.WriteWorkspace(t.Context(), key.organization, corestore.Serving, func(w postgres.WorkspaceWriter) error {
				var err error
				row, err = w.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{
					OrganizationID: key.organization, ProjectID: key.project, ID: key.id,
					ExpectedRevision: current.Revision, ExpectedRunnerID: current.RunnerID,
					ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken,
					State: state, Reason: "example-bound", RunnerID: current.RunnerID,
					MachineID: current.MachineID, LeaseID: current.LeaseID, FencingToken: current.FencingToken,
					HeadSHA: strings.Repeat("a", 40), Capabilities: []string{"files", "diff"}, Isolation: "container",
					Worktree: "retained", MachineHostname: "example-host", WorktreePath: "/workspace/example",
					ReadOnly: true, OpenedAt: &now, LastActivityAt: current.LastActivityAt,
					LastHeartbeatAt: new(now.Add(time.Second)), RebindDeadline: new(now.Add(time.Minute)), UpdatedAt: at,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return row
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for i := 1; i <= 2; i++ {
				key := scope{org, tracker.ProjectID(fmt.Sprintf("prj_%d", i)), fmt.Sprintf("ws_%032x", i)}
				var row postgres.WorkspaceSession
				if err := repository.ReadWorkspace(t.Context(), org, corestore.Serving, func(r postgres.WorkspaceReader) error {
					var err error
					row, err = r.WorkspaceSession(t.Context(), key.project, key.id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if row.LastActivityAt != nil {
					t.Fatalf("unobserved lifecycle activity=%v, want nil", row.LastActivityAt)
				}
				row.LastActivityAt = new(now.Add(time.Second))
				expected[key] = update(key, row, "ready", now.Add(2*time.Second))
			}
			key := scope{org, "prj_1", fmt.Sprintf("ws_%032x", 3)}
			if err := repository.WriteWorkspace(t.Context(), org, corestore.Serving, func(w postgres.WorkspaceWriter) error {
				var err error
				expected[key], err = w.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: org, ProjectID: key.project, ID: key.id, SubjectWorkItemID: "wi_1", Ref: "refs/heads/example", Requires: []string{"files"}, IdleTimeoutSeconds: 300, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: string(org), CreatedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		assertAll := func() {
			t.Helper()
			for key, want := range expected {
				if err := repository.ReadWorkspace(t.Context(), key.organization, corestore.Serving, func(r postgres.WorkspaceReader) error {
					got, err := r.WorkspaceSession(t.Context(), key.project, key.id)
					if err == nil && !reflect.DeepEqual(got, want) {
						t.Fatalf("%+v activity changed lifecycle or another scope: got=%+v want=%+v", key, got, want)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		touch := func(key scope, at time.Time, want error) {
			t.Helper()
			counter.Count.Store(0)
			err := counted.WriteWorkspace(t.Context(), key.organization, corestore.Serving, func(w postgres.WorkspaceWriter) error {
				before := counter.Count.Load()
				err := w.TouchWorkspaceActivity(t.Context(), key.project, key.id, at)
				statements := int64(1)
				if at.IsZero() {
					statements = 0
				}
				if got := counter.Count.Load() - before; got != statements {
					t.Fatalf("activity owner statements=%d want=%d", got, statements)
				}
				return err
			})
			if !errors.Is(err, want) {
				t.Fatalf("activity error=%v want=%v", err, want)
			}
			statements := int64(2)
			if at.IsZero() {
				statements = 1
			}
			if got := counter.Count.Load(); got != statements {
				t.Fatalf("activity tenant bind and owner statements=%d want=%d", got, statements)
			}
			assertAll()
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for i := 1; i <= 3; i++ {
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
				if i == 3 {
					project = "prj_1"
				}
				key := scope{org, project, fmt.Sprintf("ws_%032x", i)}
				before := expected[key]
				at := now.Add(time.Minute)
				want := before
				want.LastActivityAt = &at
				expected[key] = want
				touch(key, at, nil)
				touch(key, at, nil)
				touch(key, at.Add(-time.Second), nil)
				touch(key, time.Time{}, corestore.ErrInvalidPage)
				touch(scope{org, "prj_missing", key.id}, at.Add(time.Second), nil)
				touch(scope{org, project, "ws_missing"}, at.Add(time.Second), nil)
				if i < 3 {
					if i == 2 {
						before.LastActivityAt = nil
					}
					expected[key] = update(key, before, "idle", at.Add(time.Second))
					after := expected[key]
					if after.Revision != before.Revision+1 || after.State != "idle" || !after.UpdatedAt.Equal(at.Add(time.Second)) || after.LastActivityAt == nil || !after.LastActivityAt.Equal(at) {
						t.Fatalf("lifecycle lost activity or did not advance once: before=%+v after=%+v want activity=%v", before, after, at)
					}
					assertAll()
				}
			}
			for i := 1; i <= 2; i++ {
				key := scope{org, tracker.ProjectID(fmt.Sprintf("prj_%d", i)), fmt.Sprintf("ws_%032x", i)}
				current := expected[key]
				var lease leasestore.Lease
				if err := repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
					var err error
					lease, err = r.Lease(t.Context(), current.LeaseID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				touch(key, lease.ExpiresAt, nil)
				if i == 1 {
					if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
						_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "example")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					for _, state := range []string{"closing", "closed"} {
						current = update(key, current, state, now.Add(2*time.Minute))
					}
					expected[key] = current
				}
				touch(key, now.Add(3*time.Minute), nil)
			}
		}
	})
}
