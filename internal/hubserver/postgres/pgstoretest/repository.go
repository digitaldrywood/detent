package pgstoretest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func RepositoryFor(t *testing.T, admin *sql.DB, config postgres.PoolConfig) (*postgres.Repository, *pgxpool.Pool) {
	t.Helper()
	var database string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+fmt.Sprintf(" CONNECTION LIMIT %d", config.MaxConnections+1)); err != nil {
		t.Fatal(err)
	}
	pool := RolePool(t, admin, "detent_runtime_v2", "")
	if int(pool.Config().MaxConns) != config.MaxConnections {
		native := pool.Config()
		pool.Close()
		native.MaxConns = int32(config.MaxConnections)
		var err error
		pool, err = pgxpool.NewWithConfig(t.Context(), native)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository, pool
}

func SmallPool() postgres.PoolConfig {
	return postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}
}

func SeedRepository(t *testing.T, repository corestore.Repository, organization tracker.OrganizationID) {
	t.Helper()
	err := repository.Write(t.Context(), organization, corestore.Serving, func(w corestore.Writer) error {
		if err := SeedCoreOrganization(t.Context(), w, organization); err != nil {
			return err
		}
		for index, suffix := range []string{"first", "second"} {
			want := corestore.Project{OrganizationID: organization, ID: tracker.ProjectID("prj_" + suffix), Name: suffix, Slug: suffix, Profile: "native", RequireDependencies: true, SchedulingRank: int64(index), WorkflowSource: "example", WorkflowSourceRevision: "source-revision", WorkflowObjectKey: new("example/workflow"), StatesObjectKey: new("example/states"), ModelSelectionObjectKey: new("example/models")}
			got, err := w.CreateProject(t.Context(), want)
			if err != nil {
				return err
			}
			if got.CreatedAt.IsZero() {
				t.Error("project creation time is missing")
			}
			want.CreatedAt = got.CreatedAt
			if !reflect.DeepEqual(got, want) {
				t.Errorf("created project = %+v, want %+v", got, want)
			}
			for number := int64(1); number <= 2; number++ {
				item := corestore.WorkItem{OrganizationID: organization, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%s_%d", suffix, number)), ProjectID: got.ID, Number: number, NativeSourceKey: new(fmt.Sprintf("source_%d", number)), Title: "Example title", BodyObjectKey: new("example/body"), IssueContractObjectKey: new("example/contract"), ReportContextObjectKey: new("example/report"), ActorKind: new("human"), ActorPrincipalID: new("example"), ProvenanceProvider: new("example"), ProvenanceExternalID: new("external")}
				created, err := w.CreateWorkItem(t.Context(), item)
				if err != nil {
					return err
				}
				if created.ID <= 0 || created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() || created.NativeCreatedAt.IsZero() || created.NativeUpdatedAt.IsZero() {
					t.Error("work item generated metadata is missing")
				}
				item.ArchiveRestoreKnown = true
				item.LastActivityAt = created.LastActivityAt
				item.ID, item.Revision = created.ID, 1
				item.CreatedAt, item.UpdatedAt, item.NativeCreatedAt, item.NativeUpdatedAt = created.CreatedAt, created.UpdatedAt, created.NativeCreatedAt, created.NativeUpdatedAt
				if !reflect.DeepEqual(created, item) {
					t.Errorf("created work item = %+v, want %+v", created, item)
				}
				for sequence := int64(1); sequence <= 2; sequence++ {
					comment := corestore.Comment{OrganizationID: organization, ID: fmt.Sprintf("comment_%s_%d_%d", suffix, number, sequence), ProjectID: got.ID, WorkItemID: created.NativeID, Sequence: sequence, BodyObjectKey: "example/comment", ActorKind: "human", ActorPrincipalID: "example", EditedByKind: new("human"), EditedByPrincipalID: new("editor"), ProvenanceProvider: new("example"), ProvenanceExternalID: new("external"), SourceKey: new(fmt.Sprintf("source_%d", sequence))}
					stored, err := w.CreateComment(t.Context(), comment)
					if err != nil {
						return err
					}
					if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
						t.Error("comment generated metadata is missing")
					}
					comment.Revision, comment.CreatedAt, comment.UpdatedAt, comment.RecordID = 1, stored.CreatedAt, stored.UpdatedAt, stored.RecordID
					if !reflect.DeepEqual(stored, comment) {
						t.Errorf("created comment = %+v, want %+v", stored, comment)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func SeedCoreOrganization(ctx context.Context, writer corestore.Writer, organization tracker.OrganizationID) error {
	access := writer.(interface {
		accessstore.Reader
		accessstore.Writer
	})
	if _, err := access.AccessOrganization(ctx); !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	return access.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: time.Now().UTC()})
}
