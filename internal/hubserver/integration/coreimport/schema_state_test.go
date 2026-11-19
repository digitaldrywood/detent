package coreimport_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestPostgresOpenUsesLatestMigrationState(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	config := hubtest.PostgresConfig(t, admin)
	version, err := postgres.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	var earlierVersion int64
	if err := admin.QueryRowContext(t.Context(), "SELECT min(version_id) FROM public.goose_db_version WHERE version_id > 0 AND is_applied").Scan(&earlierVersion); err != nil {
		t.Fatal(err)
	}
	if earlierVersion <= 0 || earlierVersion >= version {
		t.Fatalf("earlier migration = %d, want a positive version before %d", earlierVersion, version)
	}
	for _, changedVersion := range []int64{earlierVersion, version} {
		for _, applied := range []bool{false, true} {
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO public.goose_db_version(version_id,is_applied) VALUES($1,$2)", changedVersion, applied); err != nil {
				t.Fatal(err)
			}
			reopened, err := openHubRepository(t.Context(), config)
			if !applied {
				if reopened != nil {
					if closeErr := reopened.Close(); closeErr != nil {
						t.Error(closeErr)
					}
					t.Error("latest Down state returned an open database")
				}
				if !errors.Is(err, postgres.ErrSchema) {
					t.Fatalf("migration %d latest Down state: %v, want schema refusal", changedVersion, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
