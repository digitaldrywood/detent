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

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func rolePool(t *testing.T, admin *sql.DB, group, attributes string) *sql.DB {
	t.Helper()
	return rolePoolFromTemplate(t, template, admin, group, attributes)
}

func rolePoolFromTemplate(t *testing.T, owner *pgtest.Template, admin *sql.DB, group, attributes string) *sql.DB {
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
	return owner.OpenRole(t, admin, name, password)
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
	pool := rolePool(t, admin, "detent_runtime_v2", "")
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
				writer := rolePool(t, admin, "detent_runtime_v2", "")
				reader := rolePool(t, admin, "detent_runtime_v2", "")
				runtime := runtimeFor(t, reader, writer)
				pool := writer
				if readOnly {
					pool = reader
				}
				if _, err := pool.ExecContext(t.Context(), "SET search_path=pg_catalog"); err != nil {
					t.Fatal(err)
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
					var bound, searchPath string
					var currentPID, count int
					if err := tx.QueryRowContext(t.Context(), `SELECT current_setting('detent.organization_id'),pg_backend_pid(),(SELECT count(*) FROM projects),current_setting('search_path')`).Scan(&bound, &currentPID, &count, &searchPath); err != nil {
						t.Fatal(err)
					}
					if bound != string(organization) || currentPID != pid || count != 2 || searchPath != "public, pg_catalog" {
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
					if err := pool.QueryRowContext(t.Context(), `SELECT COALESCE(current_setting('detent.organization_id',true),''),pg_backend_pid(),(SELECT count(*) FROM public.projects),current_setting('search_path')`).Scan(&bound, &currentPID, &count, &searchPath); err != nil {
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
					if _, err := admin.ExecContext(ctx, `ALTER TABLE projects OWNER TO detent_migration_v2`); err != nil {
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
			cfg := pq.Config{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port), Database: "example", User: "example", Password: "example", SSLMode: pq.SSLMode("disable")}
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
			var refusal *pq.Error
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
