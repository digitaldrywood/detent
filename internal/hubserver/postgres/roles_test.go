package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv/pgtest"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestDatabaseRolesHaveSeparateAuthority(t *testing.T) {
	db := template.Database(t)
	var versionedRoles, legacyRoles int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_roles
		WHERE rolname = ANY($1)`, []string{postgres.MigrationRoleName, postgres.RuntimeRoleName, postgres.PlatformRoleName}).Scan(&versionedRoles); err != nil {
		t.Fatal(err)
	}
	if versionedRoles != 3 {
		t.Fatalf("versioned role count = %d, want 3", versionedRoles)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_roles
		WHERE rolname = ANY($1)`, []string{"detent_migration", "detent_runtime", "detent_platform"}).Scan(&legacyRoles); err != nil {
		t.Fatal(err)
	}
	if legacyRoles != 0 {
		t.Fatalf("legacy role count = %d, want 0", legacyRoles)
	}
	var ledgerOwner string
	if err := db.QueryRowContext(t.Context(), `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.goose_db_version'::regclass`).Scan(&ledgerOwner); err != nil {
		t.Fatal(err)
	}
	if ledgerOwner != postgres.MigrationRoleName {
		t.Fatalf("migration ledger owner = %q", ledgerOwner)
	}

	for _, group := range []string{"detent_runtime_v2", "detent_platform_v2", "detent_migration_v2"} {
		t.Run(group, func(t *testing.T) {
			admin := template.Database(t)
			seedCore(t, admin)
			pool := rolePool(t, admin, group, "")
			for _, table := range []string{"projects", "issues", "native_comments"} {
				t.Run(table, func(t *testing.T) {
					var count int
					if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
						t.Fatal(err)
					}
					want := 0
					if group == "detent_platform_v2" {
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
					if owner != "detent_migration_v2" || !enabled || !forced {
						t.Fatalf("owner=%s enabled=%v forced=%v", owner, enabled, forced)
					}
				})
			}
			for _, target := range []string{"detent_runtime_v2", "detent_platform_v2", "detent_migration_v2"} {
				if target == group {
					continue
				}
				t.Run("cannot assume "+target, func(t *testing.T) {
					tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					_, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+target)
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
					tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					_, err = tx.Exec(pgtest.UnboundProbe(t.Context()), statement)
					if group == "detent_migration_v2" {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						requireSQLState(t, err, "42501")
					}
				})
			}
			t.Run("allocator refuses other relations", func(t *testing.T) {
				_, err := pool.Exec(t.Context(), `SELECT public.allocate_core_record_id('public.projects'::regclass, 100)`)
				requireSQLState(t, err, "42501")
			})
			if group == "detent_runtime_v2" {
				for _, statement := range []string{
					`TRUNCATE native_comments`,
					`SET SESSION AUTHORIZATION detent_platform_v2`,
					`ALTER ROLE detent_runtime_v2 BYPASSRLS`,
				} {
					t.Run(statement, func(t *testing.T) {
						_, err := pool.Exec(t.Context(), statement)
						requireSQLState(t, err, "42501")
					})
				}
			}
		})
	}
}

func TestRuntimeRejectsCombinedAuthority(t *testing.T) {
	for _, group := range []string{"detent_platform_v2", "detent_migration_v2"} {
		t.Run(group, func(t *testing.T) {
			admin := template.Database(t)
			pool := rolePool(t, admin, "detent_runtime_v2", "")
			var login string
			if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
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
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	runtime := runtimeFor(t, pool, pool)
	tx, err := runtime.Begin(t.Context(), "org_\x00invalid", pgtransport.Options{})
	if tx != nil || err == nil {
		if tx != nil {
			tx.Rollback(t.Context())
		}
		t.Fatalf("invalid tenant binding returned transaction=%v error=%v", tx, err)
	}
	if stats := pool.Stat(); stats.AcquiredConns() != 0 {
		t.Fatalf("failed binding retained %d connections", stats.AcquiredConns())
	}
	valid := tenantTx(t, runtime, pool, "org_second")
	var count int
	if err := valid.QueryRow(t.Context(), `SELECT count(*) FROM issues`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("usable transaction after failed binding returned %d rows: %v", count, err)
	}
	if err := valid.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
