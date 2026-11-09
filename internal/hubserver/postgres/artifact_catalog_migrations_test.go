package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactCatalogMigrationBackfillsExistingTenants(t *testing.T) {
	admin, repository, _, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			for serviceIndex := 1; serviceIndex <= 2; serviceIndex++ {
				service := fmt.Sprintf("service_%032x", serviceIndex)
				mode := "customer"
				if serviceIndex == 2 {
					mode = "hosted"
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: service, Origin: "https://example.invalid", Mode: mode, HostedOptIn: true, PublisherTokenID: "first"})
					}); err != nil {
						t.Fatal(err)
					}
				}
				request := catalogReservation(org, project, service, now)
				request.Upload.BindingMode, request.Upload.HostedOptIn = mode, mode == "hosted"
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
					if _, err := w.ReserveCatalogUpload(t.Context(), request); err != nil {
						return err
					}
					for sequence := range 2 {
						if _, err := w.PutCatalogObject(t.Context(), postgres.CatalogObject{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: request.Upload.ID, Sequence: int32(sequence), ID: artifact.NewID("object"), Size: 10, ByteOffset: int64(sequence * 10), MediaType: "text/plain", Sha256: artifact.Digest([]byte("pending"))}, now); err != nil {
							return err
						}
					}
					return w.RecordCatalogTrafficNow(t.Context(), postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project, ServiceID: service, ID: "example", UploadBytes: 10, DownloadBytes: 20, Requests: 1}, now)
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	admin.SetMaxOpenConns(1)
	admin.SetMaxIdleConns(1)
	if _, err := admin.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	var role string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != postgres.MigrationRoleName {
		t.Fatal("backfill fixture did not select the migration role", role, err)
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 20261108195800); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != postgres.MigrationRoleName {
		t.Fatal("migration ran without the production role", role, err)
	}
	if _, err := admin.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"artifact_catalog_uploads", "artifact_catalog_objects", "artifact_catalog_manifest_intents", "artifact_catalog_traffic", "artifact_services"} {
		var enabled, forced bool
		if err := admin.QueryRowContext(t.Context(), "SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass", table).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Fatal("migration did not restore forced tenant isolation", table, enabled, forced, err)
		}
	}
	repository, _ = repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
			for _, service := range []string{"", fmt.Sprintf("service_%032x", 1), fmt.Sprintf("service_%032x", 2)} {
				usage, err := r.CatalogServiceUsage(t.Context(), postgres.CatalogServiceUsageInput{OrganizationID: org, ServiceID: service})
				if err != nil {
					return err
				}
				if usage.ReservedBytes != 2000 || usage.RetainedBytes != 0 {
					t.Fatal("migration lost scoped reservations", org, service, usage)
				}
				traffic, err := r.CatalogServiceTraffic(t.Context(), postgres.CatalogServiceTrafficInput{OrganizationID: org, ServiceID: service, Minute: now.Truncate(time.Minute), Minute_2: now.Add(time.Minute)})
				if err != nil {
					return err
				}
				if traffic.RelayBytes != 60 {
					t.Fatal("migration lost service traffic or charged customer traffic to hosted allowance", org, service, traffic)
				}
			}
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				service := fmt.Sprintf("service_%032x", 2)
				upload, err := r.CatalogUpload(t.Context(), postgres.CatalogUploadInput{OrganizationID: org, ProjectID: project, ServiceID: service, ID: "upload"})
				if err != nil {
					return err
				}
				if upload.BindingMode != "hosted" || !upload.HostedOptIn {
					t.Fatal("migration changed admitted custody", org, project)
				}
				objects, err := r.CatalogObjects(t.Context(), postgres.CatalogObjectsInput{OrganizationID: org, ProjectID: project, ServiceID: service, UploadID: upload.ID, Limit: 2})
				if err != nil {
					return err
				}
				if len(objects) != 2 {
					t.Fatal("migration lost pending descriptors", len(objects))
				}
				for _, object := range objects {
					if object.WriteState != "uncertain" || !artifact.ValidID(object.WriteID, "write") {
						t.Fatal("migration released or stranded unresolved physical writes", object.WriteState, object.WriteID)
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
