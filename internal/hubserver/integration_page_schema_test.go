package hubserver

import (
	"database/sql"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIntegrationPageQueueIsTenantBound(t *testing.T) {
	service := newPostgresNativeService(t, Config{})
	value, _ := projectRecordDatabases.Load(service.database.postgres)
	admin := value.(*sql.DB)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		newNativeFixture(t, service, org, "Example")
	}
	for _, org := range []string{"", "org_first", "org_first_second"} {
		tx, err := admin.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
			t.Fatal(err)
		}
		var count int
		var organization string
		err = tx.QueryRowContext(t.Context(), "SELECT count(*),coalesce(min(organization_id),'') FROM project_integration_page_dirty").Scan(&count, &organization)
		rollbackErr := tx.Rollback()
		if err != nil || rollbackErr != nil {
			t.Fatalf("query=%v rollback=%v", err, rollbackErr)
		}
		want := 1
		if org == "" {
			want = 0
		}
		if count != want || organization != org {
			t.Fatalf("scope=%q rows=%d organization=%q", org, count, organization)
		}
	}
}
