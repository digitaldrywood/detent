package hubserver

import (
	"database/sql"
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestImportedAttemptCheckpointHydration(t *testing.T) {
	t.Parallel()
	for _, organization := range []tracker.OrganizationID{"org_execution_import", "org_execution_import_other"} {
		t.Run(string(organization), func(t *testing.T) {
			repository, admin := coreImportPostgresWithAdmin(t, 3)
			objects := &memoryRunDetails{}
			project := tracker.ProjectID("prj_execution_import")
			seedCoreExecutionPayloadTarget(t, repository, admin, organization)
			source := coreimport.CoreExecutionImportSource{BundlePath: coreimporttest.ExecutionWithCheckpoints(t, os.DirFS("../.."), organization, objects), OrganizationID: organization, SourceObjects: objects, Objects: objects}
			if count, err := coreExecutions().Import(t.Context(), repository, source); err != nil || count != 14 {
				t.Fatalf("execution import count=%d error=%v", count, err)
			}
			for _, suffix := range []string{"", "_second"} {
				var snapshot hubrecords.TenantAttemptSnapshot
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					snapshot.Attempt, err = r.Attempt(t.Context(), "attempt_execution_import"+suffix)
					if err != nil {
						return err
					}
					snapshot.Event = eventstore.Event{OrganizationID: organization, ProjectID: project}
					return hubrecords.ReadTenantAttemptCheckpointPayload(t.Context(), r, &snapshot)
				}); err != nil {
					t.Fatal(err)
				}
				reader := &Service{config: Config{TenantRepository: repository, RunDetails: objects}}
				checkpoint, err := reader.readNativeAttemptCheckpointTenant(t.Context(), snapshot)
				if err != nil || checkpoint == nil || checkpoint.PreservesSource() != (suffix == "") {
					t.Fatalf("imported verified format hydration %s: %+v err=%v", suffix, checkpoint, err)
				}
			}
		})
	}
}

func coreExecutionNeutralFixture(t *testing.T, organization tracker.OrganizationID) *neutralimporttest.Fixture {
	t.Helper()
	return coreimporttest.Execution(t, os.DirFS("../.."), organization)
}

func seedCoreExecutionPayloadTarget(t *testing.T, repository *postgres.Repository, admin *sql.DB, organization tracker.OrganizationID) {
	t.Helper()
	coreimporttest.SeedExecutionTarget(t, repository, admin, organization)
}
