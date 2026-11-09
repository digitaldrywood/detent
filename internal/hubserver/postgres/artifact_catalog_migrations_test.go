package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactCatalogMigrationBackfillsExistingTenants(t *testing.T) {
	admin, fixture := migrationDatabaseAtVersion(t, 20261108195800)
	now := historyAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, now); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'first','Example','example','example','worker',false,$2,$2)`, org, now); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_catalog_usage(organization_id,reserved_bytes) VALUES($1,4000)`, org); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,slug,profile) VALUES($1,$2,$2,$2,'native')`, org, project); err != nil {
				t.Fatal(err)
			}
			item := "wi_first_1"
			if project == "prj_second" {
				item = "wi_second_1"
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title) VALUES($1,$2,$3,1,'Example')`, org, item, project); err != nil {
				t.Fatal(err)
			}
			for serviceIndex := 1; serviceIndex <= 2; serviceIndex++ {
				service := fmt.Sprintf("service_%032x", serviceIndex)
				mode := "customer"
				if serviceIndex == 2 {
					mode = "hosted"
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_services(organization_id,project_id,id,origin,binding_mode,hosted_opt_in,publisher_token_id) VALUES($1,$2,$3,'https://example.invalid',$4,$5,'first')`, org, project, service, mode, mode == "hosted"); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_catalog_uploads(organization_id,project_id,service_id,id,idempotency_key,request_hash,work_item_id,run_id,attempt_id,version_id,kind,lease_id,fencing_token,requested_bytes,part_limit,reserved_bytes,created_at,upload_deadline,expires_at) VALUES($1,$2,$3,'upload','request',$4,$5,'','','','log','',0,1000,1000,1000,$6,$7,$8)`, org, project, service, artifact.Digest([]byte("request")), item, now, now.Add(time.Hour), now.Add(24*time.Hour)); err != nil {
					t.Fatal(err)
				}
				for sequence := range 2 {
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_catalog_objects(organization_id,project_id,service_id,upload_id,sequence,id,media_type,size,sha256,path,side,byte_offset) VALUES($1,$2,$3,'upload',$4,$5,'text/plain',10,$6,'','',$7)`, org, project, service, sequence, artifact.NewID("object"), artifact.Digest([]byte("pending")), sequence*10); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_catalog_traffic(organization_id,project_id,service_id,minute,upload_bytes,download_bytes,requests) VALUES($1,$2,$3,$4,10,20,1)`, org, project, service, now.Truncate(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
		}
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
	pool := rolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
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
