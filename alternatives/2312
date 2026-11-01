package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func rolePool(t *testing.T, admin *sql.DB, group, attributes string) *sql.DB {
	t.Helper()
	admin.SetMaxIdleConns(1)
	var database string
	var limit int
	if err := admin.QueryRowContext(t.Context(), `SELECT datname,datconnlimit FROM pg_database WHERE datname=current_database()`).Scan(&database, &limit); err != nil {
		t.Fatal(err)
	}
	if limit >= 0 && limit < 3 {
		if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pq.QuoteIdentifier(database)+" CONNECTION LIMIT 3"); err != nil {
			t.Fatal(err)
		}
	}
	name := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pq.QuoteIdentifier(name)
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pq.QuoteLiteral(password)+" "+attributes); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("PostgreSQL tests require a process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	if group != "" {
		if _, err := admin.ExecContext(t.Context(), "GRANT "+pq.QuoteIdentifier(group)+" TO "+quoted); err != nil {
			t.Fatal(err)
		}
	}
	return template.OpenRole(t, admin, name, password)
}

func runtimeFor(t *testing.T, reader, writer *sql.DB) *postgres.Runtime {
	t.Helper()
	runtime, err := postgres.NewRuntime(t.Context(), reader, writer)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func tenantTx(t *testing.T, runtime *postgres.Runtime, pool *sql.DB, organization string) *sql.Tx {
	t.Helper()
	var tx *sql.Tx
	var err error
	if organization == "" {
		tx, err = pool.BeginTx(t.Context(), nil)
	} else {
		tx, err = runtime.Begin(t.Context(), tracker.OrganizationID(organization), nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

func TestCoreRowSecurity(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	pool := rolePool(t, admin, "detent_runtime", "")
	for _, scope := range []struct {
		name         string
		organization string
		count        int
	}{
		{"missing scope", "", 0},
		{"unknown scope", "org_unknown", 0},
		{"first tenant", "org_first", 2},
		{"second tenant", "org_second", 2},
	} {
		t.Run(scope.name, func(t *testing.T) {
			tx, err := pool.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if scope.organization != "" {
				if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, scope.organization); err != nil {
					t.Fatal(err)
				}
			}
			for _, table := range []string{"projects", "issues", "native_comments"} {
				t.Run(table, func(t *testing.T) {
					rows, err := tx.QueryContext(t.Context(), "SELECT organization_id FROM "+pq.QuoteIdentifier(table))
					if err != nil {
						t.Fatal(err)
					}
					defer rows.Close()
					count := 0
					for rows.Next() {
						var organization string
						if err := rows.Scan(&organization); err != nil {
							t.Fatal(err)
						}
						if organization != scope.organization {
							t.Errorf("unfiltered query returned organization %q under scope %q", organization, scope.organization)
						}
						count++
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					if count != scope.count {
						t.Errorf("unfiltered query returned %d rows, want %d", count, scope.count)
					}
					other := "org_first"
					if scope.organization == other {
						other = "org_second"
					}
					if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+pq.QuoteIdentifier(table)+" WHERE organization_id=$1", other).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Errorf("wrong tenant filter returned %d rows", count)
					}
				})
			}
		})
	}
}

func TestCoreRowSecurityRejectsCrossTenantWrites(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	pool := rolePool(t, admin, "detent_runtime", "")
	runtime := runtimeFor(t, pool, pool)
	for _, tc := range []struct {
		table  string
		insert string
		update string
	}{
		{"projects", `INSERT INTO projects (organization_id,id,name,profile) VALUES ($1,'prj_new','New','native')`, `UPDATE projects SET name=name`},
		{"issues", `INSERT INTO issues (organization_id,project_id,number,title) VALUES ($1,'prj_shared',2,'New')`, `UPDATE issues SET title=title`},
		{"native_comments", `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id) VALUES ($1,'comment_new','prj_shared','wi_shared',2,'example/body','human','example')`, `UPDATE native_comments SET body_object_key=body_object_key`},
	} {
		for _, organization := range []string{"", "org_unknown", "org_first", "org_second"} {
			t.Run(tc.table+"/"+organization, func(t *testing.T) {
				if organization == "org_first" || organization == "org_second" {
					t.Run("insert own tenant", func(t *testing.T) {
						tx := tenantTx(t, runtime, pool, organization)
						if _, err := tx.ExecContext(t.Context(), tc.insert, organization); err != nil {
							t.Fatal(err)
						}
						var count int
						if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+pq.QuoteIdentifier(tc.table)).Scan(&count); err != nil || count != 3 {
							t.Fatalf("own tenant has %d rows, want 3: %v", count, err)
						}
					})
				}
				t.Run("unfiltered update", func(t *testing.T) {
					tx := tenantTx(t, runtime, pool, organization)
					result, err := tx.ExecContext(t.Context(), tc.update)
					if err != nil {
						t.Fatal(err)
					}
					want := int64(0)
					if organization == "org_first" || organization == "org_second" {
						want = 2
					}
					if affected, err := result.RowsAffected(); err != nil || affected != want {
						t.Fatalf("updated %d rows, want %d: %v", affected, want, err)
					}
				})
				target := "org_first"
				if organization == target {
					target = "org_second"
				}
				for _, operation := range []string{"insert", "move tenant", "delete wrong tenant"} {
					t.Run(operation, func(t *testing.T) {
						tx := tenantTx(t, runtime, pool, organization)
						switch operation {
						case "insert":
							_, err := tx.ExecContext(t.Context(), tc.insert, target)
							requireSQLState(t, err, "42501")
						case "move tenant":
							result, err := tx.ExecContext(t.Context(), "UPDATE "+pq.QuoteIdentifier(tc.table)+" SET organization_id=$1", target)
							if organization == "org_first" || organization == "org_second" {
								requireSQLState(t, err, "42501")
							} else if err != nil {
								t.Fatal(err)
							} else if affected, err := result.RowsAffected(); err != nil || affected != 0 {
								t.Fatalf("moved %d rows without scope: %v", affected, err)
							}
						case "delete wrong tenant":
							result, err := tx.ExecContext(t.Context(), "DELETE FROM "+pq.QuoteIdentifier(tc.table)+" WHERE organization_id=$1", target)
							if err != nil {
								t.Fatal(err)
							}
							if affected, err := result.RowsAffected(); err != nil || affected != 0 {
								t.Fatalf("deleted %d rows from another tenant: %v", affected, err)
							}
						}
					})
				}
			})
		}
	}
}

func TestTenantTransactionsDoNotLeakContext(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		for _, completion := range []string{"commit", "rollback", "statement error"} {
			t.Run(fmt.Sprintf("readOnly=%v/%s", readOnly, completion), func(t *testing.T) {
				admin := template.Database(t)
				seedCore(t, admin)
				writer := rolePool(t, admin, "detent_runtime", "")
				reader := rolePool(t, admin, "detent_runtime", "")
				runtime := runtimeFor(t, reader, writer)
				pool := writer
				if readOnly {
					pool = reader
				}
				var pid int
				if err := pool.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					t.Fatal(err)
				}
				for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
					tx, err := runtime.Begin(t.Context(), organization, &sql.TxOptions{ReadOnly: readOnly})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					var bound string
					var currentPID, count int
					if err := tx.QueryRowContext(t.Context(), `SELECT current_setting('detent.organization_id'),pg_backend_pid(),(SELECT count(*) FROM projects)`).Scan(&bound, &currentPID, &count); err != nil {
						t.Fatal(err)
					}
					if bound != string(organization) || currentPID != pid || count != 2 {
						t.Fatalf("scope=%q pid=%d rows=%d, want %q pid=%d rows=2", bound, currentPID, count, organization, pid)
					}
					if !readOnly {
						if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET title='Changed' WHERE native_id='wi_shared'`); err != nil {
							t.Fatal(err)
						}
					}
					switch completion {
					case "commit":
						err = tx.Commit()
					case "rollback":
						err = tx.Rollback()
					case "statement error":
						_, statementErr := tx.ExecContext(t.Context(), `SELECT 1/0`)
						requireSQLState(t, statementErr, "22012")
						err = tx.Rollback()
					}
					if err != nil {
						t.Fatal(err)
					}
					if err := pool.QueryRowContext(t.Context(), `SELECT COALESCE(current_setting('detent.organization_id',true),''),pg_backend_pid(),(SELECT count(*) FROM projects)`).Scan(&bound, &currentPID, &count); err != nil {
						t.Fatal(err)
					}
					if bound != "" || currentPID != pid || count != 0 {
						t.Fatalf("pooled connection leaked scope=%q pid=%d rows=%d, want empty pid=%d rows=0", bound, currentPID, count, pid)
					}
					var title string
					if err := admin.QueryRowContext(t.Context(), `SELECT title FROM issues WHERE organization_id=$1 AND native_id='wi_shared'`, organization).Scan(&title); err != nil {
						t.Fatal(err)
					}
					want := "shared"
					if completion == "commit" && !readOnly {
						want = "Changed"
					}
					if title != want {
						t.Fatalf("title after %s = %q, want %q", completion, title, want)
					}
				}
				for _, organization := range []tracker.OrganizationID{"", " ", "\t\n"} {
					if tx, err := runtime.Begin(t.Context(), organization, nil); !errors.Is(err, postgres.ErrTenantRequired) || tx != nil {
						t.Fatalf("missing organization returned transaction=%v error=%v", tx, err)
					}
				}
			})
		}
	}
}

func TestRuntimeRejectsPrivilegedPools(t *testing.T) {
	admin := template.Database(t)
	valid := rolePool(t, admin, "detent_runtime", "")
	for _, tc := range []struct{ name, group, attributes string }{
		{"migration", "detent_migration", ""},
		{"platform", "detent_platform", ""},
		{"ungranted", "", ""},
		{"bypass RLS", "detent_runtime", "BYPASSRLS"},
		{"superuser", "detent_runtime", "SUPERUSER"},
		{"role administrator", "detent_runtime", "CREATEROLE"},
		{"table owner", "detent_runtime", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := rolePool(t, admin, tc.group, tc.attributes)
			if tc.name == "table owner" {
				var login string
				if err := pool.QueryRowContext(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE projects OWNER TO "+pq.QuoteIdentifier(login)); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					deadline, ok := t.Deadline()
					if !ok {
						t.Error("PostgreSQL tests require a process deadline")
						return
					}
					ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
					defer cancel()
					if _, err := admin.ExecContext(ctx, `ALTER TABLE projects OWNER TO detent_migration`); err != nil {
						t.Error(err)
					}
				})
			}
			for _, pools := range [][2]*sql.DB{{pool, valid}, {valid, pool}} {
				if runtime, err := postgres.NewRuntime(t.Context(), pools[0], pools[1]); !errors.Is(err, postgres.ErrRuntimeRole) || runtime != nil {
					t.Fatalf("privileged pool accepted: runtime=%v error=%v", runtime, err)
				}
			}
		})
	}
	for _, pool := range []*sql.DB{nil, admin} {
		if runtime, err := postgres.NewRuntime(t.Context(), pool, valid); !errors.Is(err, postgres.ErrRuntimeRole) || runtime != nil {
			t.Fatalf("invalid pool accepted: runtime=%v error=%v", runtime, err)
		}
	}
}
