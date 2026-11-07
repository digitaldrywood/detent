package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func rolePool(t *testing.T, admin *sql.DB, group, attributes string) *pgxpool.Pool {
	t.Helper()
	return rolePoolFromTemplate(t, template, admin, group, attributes)
}

func rolePoolFromTemplate(t *testing.T, owner *pgtest.Template, admin *sql.DB, group, attributes string) *pgxpool.Pool {
	t.Helper()
	admin.SetMaxIdleConns(1)
	var database string
	var limit int
	if err := admin.QueryRowContext(t.Context(), `SELECT datname,datconnlimit FROM pg_database WHERE datname=current_database()`).Scan(&database, &limit); err != nil {
		t.Fatal(err)
	}
	if limit >= 0 && limit < 3 {
		if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 3"); err != nil {
			t.Fatal(err)
		}
	}
	name := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" "+attributes); err != nil {
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
		if _, err := admin.ExecContext(t.Context(), "GRANT "+pgx.Identifier{group}.Sanitize()+" TO "+quoted); err != nil {
			t.Fatal(err)
		}
	}
	config := owner.NativeConfig(t, admin, name, password)
	config.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func runtimeFor(t *testing.T, reader, writer *pgxpool.Pool) *postgres.Runtime {
	t.Helper()
	runtime, err := postgres.NewRuntime(t.Context(), reader, writer)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

type testTenantTx interface {
	pgtransport.Executor
	Commit(context.Context) error
	Rollback(context.Context) error
}
type unboundTransaction struct{ pgx.Tx }

func (t unboundTransaction) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	return t.Tx.Exec(pgtest.UnboundProbe(ctx), query, args...)
}
func (t unboundTransaction) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	return t.Tx.Query(pgtest.UnboundProbe(ctx), query, args...)
}
func (t unboundTransaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return t.Tx.QueryRow(pgtest.UnboundProbe(ctx), query, args...)
}
func tenantTx(t *testing.T, runtime *postgres.Runtime, pool *pgxpool.Pool, organization string) testTenantTx {
	t.Helper()
	var tx testTenantTx
	var err error
	if organization == "" {
		var raw pgx.Tx
		raw, err = pool.BeginTx(t.Context(), pgx.TxOptions{})
		tx = unboundTransaction{raw}
	} else {
		tx, err = runtime.Begin(t.Context(), tracker.OrganizationID(organization), pgtransport.Options{})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(t.Context()) })
	return tx
}

func TestCoreRowSecurity(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	pool := rolePool(t, admin, "detent_runtime_v2", "")
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
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if scope.organization != "" {
				if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, scope.organization); err != nil {
					t.Fatal(err)
				}
			}
			for _, table := range []string{"projects", "issues", "native_comments"} {
				t.Run(table, func(t *testing.T) {
					scopeCtx := t.Context()
					if scope.organization == "" {
						scopeCtx = pgtest.UnboundProbe(scopeCtx)
					}
					rows, err := tx.Query(scopeCtx, "SELECT organization_id FROM "+pgx.Identifier{table}.Sanitize())
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
					if err := tx.QueryRow(scopeCtx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()+" WHERE organization_id=$1", other).Scan(&count); err != nil {
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
	for _, organization := range []string{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues (organization_id,id,native_id,project_id,number,title) VALUES ($1,43,'wi_retained','prj_shared',3,'Example')`, organization); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `DELETE FROM issues WHERE organization_id=$1 AND id=43`, organization); err != nil {
			t.Fatal(err)
		}
	}
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	runtime := runtimeFor(t, pool, pool)
	for _, organization := range []string{"", "org_unknown", "org_first", "org_second"} {
		for _, identity := range []struct {
			name     string
			id       int64
			nativeID string
		}{
			{"absent", 99, "wi_absent"},
			{"live numeric identifier", 41, "wi_absent"},
			{"live native identifier", 99, "wi_shared"},
			{"retained numeric identifier", 43, "wi_absent"},
			{"retained native identifier", 99, "wi_retained"},
		} {
			t.Run("identity existence/"+organization+"/"+identity.name, func(t *testing.T) {
				target := "org_first"
				if organization == target {
					target = "org_second"
				}
				tx := tenantTx(t, runtime, pool, organization)
				_, err := tx.Exec(t.Context(), `INSERT INTO issues (organization_id,id,native_id,project_id,number,title) VALUES ($1,$2,$3,'prj_shared',99,'Example')`, target, identity.id, identity.nativeID)
				requireSQLState(t, err, "42501")
			})
		}
	}
	for _, organization := range []string{"org_first", "org_second"} {
		for _, tc := range []struct {
			name       string
			query      string
			identities int
		}{
			{"same statement child", `WITH item AS (INSERT INTO issues (organization_id,id,native_id,project_id,number,title) VALUES ($1,99,'wi_new','prj_shared',99,'Example') RETURNING organization_id,id) INSERT INTO native_attempt_runs (organization_id,run_id,issue_id) SELECT organization_id,'run_new',id FROM item`, 4},
			{"conflict ignored", `INSERT INTO issues (organization_id,id,native_id,project_id,number,title) VALUES ($1,41,'wi_shared','prj_shared',1,'Example') ON CONFLICT (organization_id,id) DO NOTHING`, 3},
			{"conflict updated", `INSERT INTO issues (organization_id,id,native_id,project_id,number,title) VALUES ($1,41,'wi_shared','prj_shared',1,'Example') ON CONFLICT (organization_id,id) DO UPDATE SET title=EXCLUDED.title`, 3},
		} {
			t.Run("authorized identity/"+organization+"/"+tc.name, func(t *testing.T) {
				tx := tenantTx(t, runtime, pool, organization)
				if _, err := tx.Exec(t.Context(), tc.query, organization); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM work_item_identities`).Scan(&count); err != nil || count != tc.identities {
					t.Fatalf("captured identities=%d, want %d: %v", count, tc.identities, err)
				}
			})
		}
	}
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
						if _, err := tx.Exec(t.Context(), tc.insert, organization); err != nil {
							t.Fatal(err)
						}
						var count int
						if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{tc.table}.Sanitize()).Scan(&count); err != nil || count != 3 {
							t.Fatalf("own tenant has %d rows, want 3: %v", count, err)
						}
					})
				}
				t.Run("unfiltered update", func(t *testing.T) {
					tx := tenantTx(t, runtime, pool, organization)
					result, err := tx.Exec(t.Context(), tc.update)
					if err != nil {
						t.Fatal(err)
					}
					want := int64(0)
					if organization == "org_first" || organization == "org_second" {
						want = 2
					}
					if affected := result.RowsAffected(); affected != want {
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
							_, err := tx.Exec(t.Context(), tc.insert, target)
							requireSQLState(t, err, "42501")
						case "move tenant":
							result, err := tx.Exec(t.Context(), "UPDATE "+pgx.Identifier{tc.table}.Sanitize()+" SET organization_id=$1", target)
							if organization == "org_first" || organization == "org_second" {
								state := "42501"
								if tc.table == "issues" {
									state = "23514"
								}
								requireSQLState(t, err, state)
							} else if err != nil {
								t.Fatal(err)
							} else if affected := result.RowsAffected(); affected != 0 {
								t.Fatalf("moved %d rows without scope: %v", affected, err)
							}
						case "delete wrong tenant":
							result, err := tx.Exec(t.Context(), "DELETE FROM "+pgx.Identifier{tc.table}.Sanitize()+" WHERE organization_id=$1", target)
							if err != nil {
								t.Fatal(err)
							}
							if affected := result.RowsAffected(); affected != 0 {
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
				writer := rolePool(t, admin, "detent_runtime_v2", "")
				reader := rolePool(t, admin, "detent_runtime_v2", "")
				runtime := runtimeFor(t, reader, writer)
				pool := writer
				if readOnly {
					pool = reader
				}
				if _, err := pool.Exec(t.Context(), "SET search_path=pg_catalog"); err != nil {
					t.Fatal(err)
				}
				var pid int
				if err := pool.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					t.Fatal(err)
				}
				for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
					tx, err := runtime.Begin(t.Context(), organization, pgtransport.Options{ReadOnly: readOnly})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					var bound, searchPath string
					var currentPID, count int
					if err := tx.QueryRow(t.Context(), `SELECT current_setting('detent.organization_id'),pg_backend_pid(),(SELECT count(*) FROM projects),current_setting('search_path')`).Scan(&bound, &currentPID, &count, &searchPath); err != nil {
						t.Fatal(err)
					}
					if bound != string(organization) || currentPID != pid || count != 2 || searchPath != "public, pg_catalog" {
						t.Fatalf("scope=%q pid=%d rows=%d, want %q pid=%d rows=2", bound, currentPID, count, organization, pid)
					}
					if !readOnly {
						if _, err := tx.Exec(t.Context(), `UPDATE issues SET title='Changed' WHERE native_id='wi_shared'`); err != nil {
							t.Fatal(err)
						}
					}
					switch completion {
					case "commit":
						err = tx.Commit(t.Context())
					case "rollback":
						err = tx.Rollback(t.Context())
					case "statement error":
						_, statementErr := tx.Exec(t.Context(), `SELECT 1/0`)
						requireSQLState(t, statementErr, "22012")
						err = tx.Rollback(t.Context())
					}
					if err != nil {
						t.Fatal(err)
					}
					if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT COALESCE(current_setting('detent.organization_id',true),''),pg_backend_pid(),(SELECT count(*) FROM public.projects),current_setting('search_path')`).Scan(&bound, &currentPID, &count, &searchPath); err != nil {
						t.Fatal(err)
					}
					if bound != "" || currentPID != pid || count != 0 || searchPath != "pg_catalog" {
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
					if tx, err := runtime.Begin(t.Context(), organization, pgtransport.Options{}); !errors.Is(err, postgres.ErrTenantRequired) || tx != nil {
						t.Fatalf("missing organization returned transaction=%v error=%v", tx, err)
					}
				}
			})
		}
	}
}

func TestRuntimeRejectsPrivilegedPools(t *testing.T) {
	admin := template.Database(t)
	valid := rolePool(t, admin, "detent_runtime_v2", "")
	for _, tc := range []struct{ name, group, attributes string }{
		{"migration", "detent_migration_v2", ""},
		{"platform", "detent_platform_v2", ""},
		{"ungranted", "", ""},
		{"bypass RLS", "detent_runtime_v2", "BYPASSRLS"},
		{"superuser", "detent_runtime_v2", "SUPERUSER"},
		{"role administrator", "detent_runtime_v2", "CREATEROLE"},
		{"table owner", "detent_runtime_v2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := rolePool(t, admin, tc.group, tc.attributes)
			if tc.name == "table owner" {
				var login string
				if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE projects OWNER TO "+pgx.Identifier{login}.Sanitize()); err != nil {
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
					if _, err := admin.ExecContext(ctx, `ALTER TABLE projects OWNER TO detent_migration_v2`); err != nil {
						t.Error(err)
					}
				})
			}
			for _, pools := range [][2]*pgxpool.Pool{{pool, valid}, {valid, pool}} {
				if runtime, err := postgres.NewRuntime(t.Context(), pools[0], pools[1]); !errors.Is(err, postgres.ErrRuntimeRole) || runtime != nil {
					t.Fatalf("privileged pool accepted: runtime=%v error=%v", runtime, err)
				}
			}
		})
	}
	adminPool := template.OpenNativeRole(t, admin, "", "")
	for _, pool := range []*pgxpool.Pool{nil, adminPool} {
		if runtime, err := postgres.NewRuntime(t.Context(), pool, valid); !errors.Is(err, postgres.ErrRuntimeRole) || runtime != nil {
			t.Fatalf("invalid pool accepted: runtime=%v error=%v", runtime, err)
		}
	}
}

func TestPostgresStartupSeparatesRuntimeAndMigrationParameters(t *testing.T) {
	for _, migration := range []bool{false, true} {
		t.Run(fmt.Sprintf("migration=%v", migration), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			type startupResult struct {
				parameters map[string]string
				err        error
			}
			ready := make(chan startupResult, 1)
			joined := make(chan struct{})
			var connection net.Conn
			accepted := make(chan struct{})
			go func() {
				defer close(joined)
				conn, err := listener.Accept()
				connection = conn
				close(accepted)
				if err != nil {
					ready <- startupResult{err: err}
					return
				}
				defer conn.Close()
				var header [4]byte
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					ready <- startupResult{err: err}
					return
				}
				size := binary.BigEndian.Uint32(header[:])
				if size < 8 || size > 65536 {
					ready <- startupResult{err: errors.New("invalid startup frame size")}
					return
				}
				body := make([]byte, int(size)-4)
				if _, err := io.ReadFull(conn, body); err != nil {
					ready <- startupResult{err: err}
					return
				}
				values := bytes.Split(body[4:], []byte{0})
				parameters := map[string]string{}
				for i := 0; i+1 < len(values); i += 2 {
					parameters[string(values[i])] = string(values[i+1])
				}
				payload := []byte("SFATAL\x00VFATAL\x00C0A000\x00Mexample startup inspected\x00\x00")
				message := make([]byte, 5+len(payload))
				message[0] = 'E'
				binary.BigEndian.PutUint32(message[1:5], uint32(4+len(payload)))
				copy(message[5:], payload)
				_, err = conn.Write(message)
				ready <- startupResult{parameters: parameters, err: err}
			}()
			t.Cleanup(func() {
				listener.Close()
				<-accepted
				if connection != nil {
					connection.Close()
				}
				<-joined
			})
			cfg := pgtransport.Connection{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port), Database: "example", User: "example", Password: "example", SSLMode: "disable"}
			if migration {
				err = postgres.Migrate(t.Context(), cfg, os.DirFS("migrations"))
			} else {
				var repository *postgres.Repository
				repository, err = postgres.Open(t.Context(), postgres.Config{Connection: cfg, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}, os.DirFS("migrations"))
				if repository != nil {
					repository.Close()
				}
			}
			if err == nil {
				t.Fatal("test server refusal was ignored")
			}
			var refusal *pgconn.PgError
			if !errors.As(err, &refusal) || refusal.Message != "example startup inspected" {
				t.Fatalf("startup did not reach the test server: %v", err)
			}
			result := <-ready
			if result.err != nil {
				t.Fatal(result.err)
			}
			if migration {
				if result.parameters["search_path"] != "public, pg_catalog" || result.parameters["role"] != postgres.MigrationRoleName {
					t.Fatalf("migration startup authority=%v", result.parameters)
				}
			} else if _, exists := result.parameters["search_path"]; exists {
				t.Fatal("runtime sent transaction-pool-incompatible startup search_path")
			}
		})
	}
}
