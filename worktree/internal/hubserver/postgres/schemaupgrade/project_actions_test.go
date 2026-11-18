package schemaupgrade_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresProjectActionDeletionLifecycle(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	now := time.Now().UTC()
	details := hubtest.NewDetails()
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: now})
		for _, sameID := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same_id_%t", organization, sameID), func(t *testing.T) {
				first := tracker.ProjectID(fmt.Sprintf("first_%t", sameID))
				second := tracker.ProjectID(fmt.Sprintf("second_%t", sameID))
				ref, err := details.StoreDetail(t.Context(), string(organization), []byte(fmt.Sprintf("command %t", sameID)))
				if err != nil {
					t.Fatal(err)
				}
				var object artifactstore.ArchiveObject
				write := func(fn func(tenantstore.Writer) error) {
					t.Helper()
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, fn); err != nil {
						t.Fatal(err)
					}
				}
				write(func(w tenantstore.Writer) error {
					for i, project := range []tracker.ProjectID{first, second} {
						if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native", CreatedAt: now}); err != nil {
							return err
						}
						id := fmt.Sprintf("action_%d", i)
						if sameID {
							id = "action_shared"
						}
						record := actionstore.Record{Action: workspacesession.Action{ID: id, OrganizationID: string(organization), ProjectID: string(project), Name: "Example", CreatedBy: "operator", Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: ref}
						if err := w.(actionstore.Writer).CreateProjectAction(t.Context(), record); err != nil {
							return err
						}
						var err error
						object, err = w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: hubapi.NewID("att"), Reference: ref, CreatedAt: now})
						if err != nil {
							return err
						}
						if err := w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: organization, ArchiveID: object.ID, Kind: "project-action", ID: actionstore.ArchiveOwnerID(project, id), CreatedAt: now}); err != nil {
							return err
						}
					}
					return nil
				})
				assertOwners := func(want int, bytes int64) {
					t.Helper()
					var owners int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2 AND owner_kind='project-action'`, organization, object.ID).Scan(&owners); err != nil {
						t.Fatal(err)
					}
					if owners != want {
						t.Fatalf("shared command owners=%d want %d", owners, want)
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						actual, err := r.ArchiveBytes(t.Context())
						if err == nil && actual != bytes {
							t.Fatalf("archive bytes=%d want %d", actual, bytes)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				assertOwners(2, ref.CompressedSize)
				write(func(w tenantstore.Writer) error { return w.DeleteProject(t.Context(), first) })
				assertOwners(1, ref.CompressedSize)
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.(actionstore.Reader).ProjectActions(t.Context(), first)
					if err != nil {
						return err
					}
					if len(rows) != 0 {
						t.Fatal("physical deletion retained action rows")
					}
					rows, err = r.(actionstore.Reader).ProjectActions(t.Context(), second)
					if err != nil {
						return err
					}
					if len(rows) != 1 {
						t.Fatal("physical deletion lost other project's action")
					}
					raw, err := details.ReadDetail(t.Context(), string(organization), rows[0].Command)
					if err == nil && len(raw) == 0 {
						t.Fatal("shared command unavailable")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				write(func(w tenantstore.Writer) error { return w.DeleteProject(t.Context(), second) })
				assertOwners(0, 0)
			})
		}
	}
}

func TestPostgresProjectActionsRollbackRefusesRetainedData(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	now := time.Now().UTC()
	details := hubtest.NewDetails()
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: now})
		ref, err := details.StoreDetail(t.Context(), string(organization), []byte("retained command"))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			project := tracker.ProjectID("retained_project")
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, States: hubtest.States()}); err != nil {
				return err
			}
			if err := w.(actionstore.Writer).CreateProjectAction(t.Context(), actionstore.Record{Action: workspacesession.Action{ID: "retained_action", OrganizationID: string(organization), ProjectID: string(project), Name: "Example", CreatedBy: "operator", Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: ref}); err != nil {
				return err
			}
			object, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: hubapi.NewID("att"), Reference: ref, CreatedAt: now})
			if err != nil {
				return err
			}
			if err := w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: organization, ArchiveID: object.ID, Kind: "project-action", ID: actionstore.ArchiveOwnerID(project, "retained_action"), CreatedAt: now}); err != nil {
				return err
			}
			return w.RetireProject(t.Context(), project, 1, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		var owners int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='project-action'`, organization).Scan(&owners); err != nil {
			t.Fatal(err)
		}
		if owners != 1 {
			t.Fatalf("soft retirement retained owners=%d want 1", owners)
		}
	}
	conn, err := admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(ctx, `RESET ROLE`); err != nil {
			t.Error(err)
		}
	}()
	var visible int
	if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM project_actions`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("migration role sees tenant rows without scope: %d", visible)
	}
	raw, err := os.ReadFile("../migrations/20261104002716_project_actions.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("missing Down migration")
	}
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, rollbackErr := tx.ExecContext(t.Context(), down)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "project action records require an explicit retained-data migration") {
		t.Fatalf("rollback refusal: %v", rollbackErr)
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			project, err := r.Project(t.Context(), "retained_project")
			if err != nil {
				return err
			}
			if project.DeletedAt == nil {
				t.Fatal("soft retirement did not retain retired project")
			}
			rows, err := r.(actionstore.Reader).ProjectActions(t.Context(), "retained_project")
			if err != nil {
				return err
			}
			if len(rows) != 1 {
				t.Fatal("rollback lost retained action")
			}
			bytes, err := r.ArchiveBytes(t.Context())
			if err == nil && bytes != rows[0].Command.CompressedSize {
				t.Fatal("rollback changed archive usage")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
