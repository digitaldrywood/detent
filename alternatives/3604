package schemaupgrade_test

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
)

func TestDesignatedLocalOrganizationUpgradeKeepsOnlyASingleLocalOrganization(t *testing.T) {
	t.Parallel()
	const version = 20261110221710
	_, fixture := pgstoretest.MigrationDatabaseAtVersion(t, version-1)
	organizations := []string{"org_first", "org_second", "org_third"}
	for _, tc := range []struct {
		name  string
		local []string
		want  []string
	}{
		{name: "none"},
		{name: "one", local: []string{"org_second"}, want: []string{"org_second"}},
		{name: "several", local: []string{"org_first", "org_third"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := fixture.Database(t)
			db.SetMaxOpenConns(1)
			for _, organization := range organizations {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,$2,CURRENT_TIMESTAMP)`, organization, slices.Contains(tc.local, organization)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
				t.Fatal(err)
			}
			provider, err := pgstoretest.MigrationProvider(db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.UpTo(t.Context(), version); err != nil {
				t.Fatalf("designated local organization upgrade: %v", err)
			}
			if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
				t.Fatal(err)
			}
			rows, err := db.QueryContext(t.Context(), `SELECT organization_id FROM organizations WHERE local ORDER BY organization_id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var organization string
				if err := rows.Scan(&organization); err != nil {
					t.Fatal(err)
				}
				got = append(got, organization)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("local organizations after upgrade = %v, want %v", got, tc.want)
			}
			var total int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM organizations`).Scan(&total); err != nil || total != len(organizations) {
				t.Errorf("organizations after upgrade = %d, want %d: %v", total, len(organizations), err)
			}
		})
	}
}
