package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestDeploymentRejectsUnsupportedRuntimeCapabilities(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
		admin     bool
	}{
		{"row security disabled", `SET row_security = off`, false},
		{"session tenant", `SELECT set_config('detent.organization_id', 'org_first', false)`, false},
		{"missing application search path", `SET search_path = pg_catalog`, false},
		{"schema creation", `GRANT CREATE ON SCHEMA public TO detent_runtime_v2`, true},
		{"public schema creation", `GRANT CREATE ON SCHEMA public TO PUBLIC`, true},
		{"catalog creation", `GRANT CREATE ON SCHEMA pg_catalog TO detent_runtime_v2`, true},
		{"platform access", `GRANT USAGE ON SCHEMA platform TO detent_runtime_v2`, true},
		{"database creation", `GRANT CREATE ON DATABASE CURRENT_DATABASE TO detent_runtime_v2`, true},
		{"truncate", `GRANT TRUNCATE ON projects TO detent_runtime_v2`, true},
		{"missing select", `REVOKE SELECT ON projects FROM detent_runtime_v2`, true},
		{"missing insert", `REVOKE INSERT ON issues FROM detent_runtime_v2`, true},
		{"missing update", `REVOKE UPDATE ON leases FROM detent_runtime_v2`, true},
		{"missing delete", `REVOKE DELETE ON native_attempts FROM detent_runtime_v2`, true},
		{"missing sequence", `REVOKE USAGE ON SEQUENCE issues_id_seq FROM detent_runtime_v2`, true},
		{"missing allocator", `REVOKE EXECUTE ON FUNCTION allocate_core_record_id(regclass,bigint) FROM detent_runtime_v2`, true},
		{"missing conversion", `REVOKE EXECUTE ON FUNCTION convert_history_payload(text,text,text,text,bigint,text,bigint,text,text,bigint,bigint,text,text,bigint,bigint) FROM detent_runtime_v2`, true},
		{"sequence reset", `GRANT UPDATE ON SEQUENCE issues_id_seq TO detent_runtime_v2`, true},
		{"unforced RLS", `ALTER TABLE leases NO FORCE ROW LEVEL SECURITY`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := template.Database(t)
			pool := rolePool(t, admin, "detent_runtime_v2", "")
			target := pool
			statement := test.statement
			if test.admin {
				target = admin
			}
			if test.name == "database creation" {
				var database string
				if err := admin.QueryRowContext(t.Context(), `SELECT quote_ident(current_database())`).Scan(&database); err != nil {
					t.Fatal(err)
				}
				statement = "GRANT CREATE ON DATABASE " + database + " TO detent_runtime_v2"
			}
			if _, err := target.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
			runtime, err := postgres.NewRuntime(t.Context(), pool, pool)
			if runtime != nil || !errors.Is(err, postgres.ErrRuntimeRole) {
				t.Fatalf("unsupported deployment returned runtime=%v error=%v", runtime, err)
			}
			if stats := pool.Stats(); stats.InUse != 0 {
				t.Fatalf("failed startup retained %d connections", stats.InUse)
			}
		})
	}
}
