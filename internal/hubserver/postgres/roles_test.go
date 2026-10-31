package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestDatabaseRolesHaveSeparateAuthority(t *testing.T) {
	for _, group := range []string{"detent_runtime", "detent_platform", "detent_migration"} {
		t.Run(group, func(t *testing.T) {
			admin := template.Database(t)
			seedCore(t, admin)
			pool := rolePool(t, admin, group, "")
			for _, table := range []string{"projects", "issues", "native_comments"} {
				t.Run(table, func(t *testing.T) {
					var count int
					if err := pool.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
						t.Fatal(err)
					}
					want := 0
					if group == "detent_platform" {
						want = 4
					}
					if count != want {
						t.Fatalf("unscoped %s read %d rows, want %d", group, count, want)
					}
					var owner string
					var enabled, forced bool
					if err := admin.QueryRowContext(t.Context(), `SELECT pg_get_userbyid(relowner),relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, "public."+table).Scan(&owner, &enabled, &forced); err != nil {
						t.Fatal(err)
					}
					if owner != "detent_migration" || !enabled || !forced {
						t.Fatalf("owner=%s enabled=%v forced=%v", owner, enabled, forced)
					}
				})
			}
			for _, target := range []string{"detent_runtime", "detent_platform", "detent_migration"} {
				if target == group {
					continue
				}
				t.Run("cannot assume "+target, func(t *testing.T) {
					tx, err := pool.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					_, err = tx.ExecContext(t.Context(), "SET LOCAL ROLE "+target)
					requireSQLState(t, err, "42501")
				})
			}
			for _, statement := range []string{
				`ALTER TABLE projects DISABLE ROW LEVEL SECURITY`,
				`CREATE TABLE example_migration (id text PRIMARY KEY)`,
				`SELECT setval('public.issues_id_seq', 100)`,
				`SELECT setval('public.native_comments_record_id_seq', 100)`,
			} {
				t.Run(statement, func(t *testing.T) {
					tx, err := pool.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					_, err = tx.ExecContext(t.Context(), statement)
					if group == "detent_migration" {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						requireSQLState(t, err, "42501")
					}
				})
			}
			t.Run("allocator refuses other relations", func(t *testing.T) {
				_, err := pool.ExecContext(t.Context(), `SELECT public.allocate_core_record_id('public.projects'::regclass, 100)`)
				requireSQLState(t, err, "42501")
			})
			if group == "detent_runtime" {
				for _, statement := range []string{
					`TRUNCATE native_comments`,
					`SET SESSION AUTHORIZATION detent_platform`,
					`ALTER ROLE detent_runtime BYPASSRLS`,
				} {
					t.Run(statement, func(t *testing.T) {
						_, err := pool.ExecContext(t.Context(), statement)
						requireSQLState(t, err, "42501")
					})
				}
			}
		})
	}
}

func TestRuntimeRejectsCombinedAuthority(t *testing.T) {
	for _, group := range []string{"detent_platform", "detent_migration"} {
		t.Run(group, func(t *testing.T) {
			admin := template.Database(t)
			pool := rolePool(t, admin, "detent_runtime", "")
			var login string
			if err := pool.QueryRowContext(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "GRANT "+group+" TO "+login+" WITH INHERIT FALSE"); err != nil {
				t.Fatal(err)
			}
			if runtime, err := postgres.NewRuntime(t.Context(), pool, pool); !errors.Is(err, postgres.ErrRuntimeRole) || runtime != nil {
				t.Fatalf("combined authority accepted: runtime=%v error=%v", runtime, err)
			}
		})
	}
}

func TestTenantBindingFailureReleasesConnection(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	pool := rolePool(t, admin, "detent_runtime", "")
	runtime := runtimeFor(t, pool, pool)
	tx, err := runtime.Begin(t.Context(), "org_\x00invalid", nil)
	if tx != nil || err == nil {
		if tx != nil {
			tx.Rollback()
		}
		t.Fatalf("invalid tenant binding returned transaction=%v error=%v", tx, err)
	}
	if stats := pool.Stats(); stats.InUse != 0 {
		t.Fatalf("failed binding retained %d connections", stats.InUse)
	}
	valid := tenantTx(t, runtime, pool, "org_second")
	var count int
	if err := valid.QueryRowContext(t.Context(), `SELECT count(*) FROM issues`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("usable transaction after failed binding returned %d rows: %v", count, err)
	}
	if err := valid.Commit(); err != nil {
		t.Fatal(err)
	}
}
