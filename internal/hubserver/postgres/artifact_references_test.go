package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactReferencesAreImmutableAndTenantScoped(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	organizations := make([]tracker.OrganizationID, 2)
	references := make([]artifact.Reference, len(organizations))
	for index := range organizations {
		suffix := fmt.Sprintf("%032x", index+1)
		organization := tracker.OrganizationID("org_" + suffix)
		project := tracker.ProjectID("prj_" + fmt.Sprintf("%032x", index+101))
		item := tracker.NativeWorkItemID("wi_" + fmt.Sprintf("%032x", index+201))
		organizations[index] = organization
		seedTenantAccess(t, repository, organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native", WorkflowSource: "example", WorkflowSourceRevision: "revision", WorkflowObjectKey: new("example/workflow"), StatesObjectKey: new("example/states"), ModelSelectionObjectKey: new("example/models")}); err != nil {
				return err
			}
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, NativeID: item, ProjectID: project, Number: 1, NativeSourceKey: new("source_" + suffix), Title: "Example item", BodyObjectKey: new("example/body"), IssueContractObjectKey: new("example/contract"), ReportContextObjectKey: new("example/report"), ActorKind: new("human"), ActorPrincipalID: new("example"), ProvenanceProvider: new("example"), ProvenanceExternalID: new("external")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		references[index] = artifact.Reference{
			SchemaVersion: 1,
			Scope:         artifact.Scope{OrganizationID: string(organization), ProjectID: string(project), WorkItemID: string(item), RunID: "run_" + suffix, AttemptID: "attempt_" + suffix},
			ServiceID:     "service_" + suffix, ArtifactID: "artifact_" + suffix, ManifestID: "manifest_" + suffix, Revision: 1,
			SHA256: artifact.Digest([]byte(suffix)), Kind: "diff", State: "complete", Availability: "available",
			Bytes: 12, Objects: 1, ExpiresAt: now.Add(time.Hour), ObservedAt: now,
		}
	}
	for index, reference := range references {
		organization := organizations[index]
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			created, err := w.AppendArtifactReference(t.Context(), reference)
			if err != nil {
				return err
			}
			created.ExpiresAt = created.ExpiresAt.UTC()
			created.ObservedAt = created.ObservedAt.UTC()
			if !reflect.DeepEqual(created, reference) {
				t.Fatalf("created reference = %+v, want %+v", created, reference)
			}
			_, err = w.AppendArtifactReference(t.Context(), reference)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, reference := range references {
		organization := organizations[index]
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.ArtifactReference(t.Context(), tracker.ProjectID(reference.ProjectID), tracker.NativeWorkItemID(reference.WorkItemID), reference.ArtifactID, reference.Revision)
			if err != nil {
				return err
			}
			got.ExpiresAt = got.ExpiresAt.UTC()
			got.ObservedAt = got.ObservedAt.UTC()
			if !reflect.DeepEqual(got, reference) {
				t.Fatalf("stored reference = %+v, want %+v", got, reference)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	changed := references[0]
	changed.SHA256 = artifact.Digest([]byte("changed"))
	err := repository.WriteTenant(t.Context(), organizations[0], corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.AppendArtifactReference(t.Context(), changed)
		return err
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("changed immutable reference error = %v, want revision conflict", err)
	}
	if err := repository.ReadTenant(t.Context(), organizations[1], corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.ArtifactReference(t.Context(), "prj_first", "wi_first_1", references[0].ArtifactID, 1)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("cross-tenant reference lookup = %+v, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestViewedFilesUseTypedTenantRecords(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, organization := range organizations {
		seedRepository(t, repository, organization)
	}
	for index, organization := range organizations {
		suffix := fmt.Sprintf("viewed_%d", index+1)
		fixture := seedHistory(t, repository, organization, suffix)
		actor := "reviewer_" + suffix
		record := eventstore.ViewedFile{
			OrganizationID: organization, ProjectID: fixture.change.ProjectID, ChangeID: fixture.change.ID,
			VersionID: fixture.version.ID, PrincipalID: actor, ManifestSHA256: artifact.Digest([]byte("manifest_" + suffix)),
			FileSHA256: artifact.Digest([]byte("file_" + suffix)), Viewed: true, UpdatedAt: historyAt(),
		}
		if err := repository.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
			stored, err := w.SetViewedFile(t.Context(), record)
			stored.UpdatedAt = stored.UpdatedAt.UTC()
			if err == nil && !reflect.DeepEqual(stored, record) {
				t.Errorf("stored viewed file = %+v, want %+v", stored, record)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
			rows, err := r.ViewedFiles(t.Context(), fixture.version.ID, actor, 2)
			if err != nil {
				return err
			}
			if len(rows) == 1 {
				rows[0].UpdatedAt = rows[0].UpdatedAt.UTC()
			}
			if len(rows) != 1 || !reflect.DeepEqual(rows[0], record) {
				t.Errorf("viewed files = %+v, want %+v", rows, record)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostedArtifactUsageEnforcesTenantIsolation(t *testing.T) {
	db := template.Database(t)
	seedCore(t, db)
	organizations := []string{"org_first", "org_second"}
	for _, org := range organizations {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO hosted_artifact_usage(organization_id,service_id,observed_at,retained_bytes,reserved_bytes) VALUES($1,'service_example',$2,12,34)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
	}
	pool := rolePool(t, db, "detent_runtime_v2", "")
	for index, org := range organizations {
		t.Run(org, func(t *testing.T) {
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				deadline, _ := t.Deadline()
				cleanup, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
				defer cancel()
				if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Error(err)
				}
			})
			if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, org); err != nil {
				t.Fatal(err)
			}
			var owner string
			var count, retained int64
			if err := tx.QueryRow(t.Context(), `SELECT count(*),min(organization_id),min(retained_bytes) FROM hosted_artifact_usage`).Scan(&count, &owner, &retained); err != nil {
				t.Fatal(err)
			}
			if count != 1 || owner != org || retained != 12 {
				t.Fatalf("visible usage = %d/%s/%d", count, owner, retained)
			}
			_, err = tx.Exec(t.Context(), `INSERT INTO hosted_artifact_usage(organization_id,service_id,observed_at,retained_bytes,reserved_bytes) VALUES($1,'service_example',$2,12,34)`, organizations[1-index], historyAt())
			var denied *pgconn.PgError
			if !errors.As(err, &denied) || denied.Code != "42501" {
				t.Fatalf("foreign usage write error = %v", err)
			}
		})
	}
}

type artifactPublisherQuery struct {
	pgx.Tx
	query string
	args  []any
}

func (p *artifactPublisherQuery) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	p.query, p.args = query, args
	return p.Tx.QueryRow(ctx, query, args...)
}

func TestHostedArtifactPublisherLookupBoundsBindingReads(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	for _, organization := range []string{"org_first", "org_second"} {
		for _, statement := range []string{
			`INSERT INTO projects (organization_id,id,name,profile) SELECT $1,'prj_binding_'||n,'Binding '||n,'native' FROM generate_series(1,100) n`,
			`INSERT INTO api_tokens (organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES ($1,'publisher','Publisher','hash','fingerprint','worker',true,now(),now())`,
			`INSERT INTO token_grants (organization_id,token_id,project_id) SELECT $1,'publisher','prj_binding_'||n FROM generate_series(1,100) n`,
			`INSERT INTO artifact_services (organization_id,project_id,id,origin,binding_mode,hosted_opt_in,publisher_token_id) SELECT $1,'prj_binding_'||((n-1)/100+1),'service_'||n,'https://artifacts.example.test','hosted',true,'publisher' FROM generate_series(1,10000) n`,
		} {
			if _, err := admin.ExecContext(t.Context(), statement, organization); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := admin.ExecContext(t.Context(), "ANALYZE artifact_services, token_grants, api_tokens"); err != nil {
		t.Fatal(err)
	}
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	for _, organization := range []string{"org_first", "org_second"} {
		t.Run(organization, func(t *testing.T) {
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id', $1, true)", organization); err != nil {
				t.Fatal(err)
			}
			probe := &artifactPublisherQuery{Tx: tx}
			for _, service := range []string{"service_10000", "service_missing"} {
				bound, err := pgquery.New(probe).HostedArtifactPublisherBound(t.Context(), pgquery.HostedArtifactPublisherBoundParams{OrganizationID: tracker.OrganizationID(organization), ServiceID: service, PublisherID: "publisher", Now: time.Now().UTC()})
				if err != nil || bound != (service == "service_10000") {
					t.Fatalf("bound=%v error=%v", bound, err)
				}
				var raw []byte
				if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.query, probe.args...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s: %s", service, raw)
				var plans []struct{ Plan map[string]any }
				if err := json.Unmarshal(raw, &plans); err != nil {
					t.Fatal(err)
				}
				visited := false
				var inspect func(map[string]any)
				inspect = func(node map[string]any) {
					if node["Relation Name"] == "artifact_services" {
						visited = true
						blocks, _ := node["Shared Hit Blocks"].(float64)
						reads, _ := node["Shared Read Blocks"].(float64)
						removed, _ := node["Rows Removed by Filter"].(float64)
						if blocks+reads > 32 || removed > 10 {
							t.Errorf("binding lookup scanned %.0f blocks and filtered %.0f unrelated rows", blocks+reads, removed)
						}
					}
					children, _ := node["Plans"].([]any)
					for _, child := range children {
						inspect(child.(map[string]any))
					}
				}
				inspect(plans[0].Plan)
				if !visited {
					t.Fatal("binding relation was not executed")
				}
			}
		})
	}
}
