package schemaupgrade_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRecoveryInterruptedMigration(t *testing.T) {
	t.Parallel()
	fixture := hubPostgresMigrationFixture(t)
	restoration := pgtest.NewTemplate(func(context.Context, *sql.DB) error { return nil })
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("PostgreSQL recovery tests require a process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := restoration.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	files := fstest.MapFS{}
	entries, err := os.ReadDir("../migrations")
	if err != nil {
		t.Fatal(err)
	}
	var target, currentVersion, previousVersion int64
	var targetName string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../migrations", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		version, err := strconv.ParseInt(strings.SplitN(entry.Name(), "_", 2)[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = &fstest.MapFile{Data: data}
		currentVersion = max(currentVersion, version)
		if !bytes.Contains(data, []byte("-- +goose NO TRANSACTION")) && version > target {
			target, targetName = version, entry.Name()
		}
	}
	before := fstest.MapFS{}
	for name, file := range files {
		version, err := strconv.ParseInt(strings.SplitN(name, "_", 2)[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if version < target {
			before[name] = file
			previousVersion = max(previousVersion, version)
		}
	}
	if previousVersion == 0 || targetName == "" {
		t.Fatal("PostgreSQL recovery needs an earlier migration boundary")
	}
	for _, stage := range []string{"schema", "version"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			admin := fixture.Database(t)
			migration := roleConnection(t, fixture, admin, "detent_migration_v2")
			if err := postgres.Migrate(t.Context(), migration, before); err != nil {
				t.Fatal(err)
			}
			for _, organization := range []string{"org_recovery_first", "org_recovery_second"} {
				for _, statement := range []string{
					`INSERT INTO organizations(organization_id,name,slug,local,created_at) VALUES($1,$1,$1,false,clock_timestamp())`,
					`INSERT INTO projects(organization_id,id,name,profile) VALUES($1,'prj_recovery','Recovered project','native')`,
				} {
					if _, err := admin.ExecContext(t.Context(), statement, organization); err != nil {
						t.Fatal(err)
					}
				}
			}
			ledger := hubPostgresMigrationLedger(t, admin)
			interrupted := fstest.MapFS{}
			for name, file := range before {
				interrupted[name] = file
			}
			raw := string(files[targetName].Data)
			up, down, _ := strings.Cut(raw, "-- +goose Down")
			fault := "\nCREATE TABLE recovery_interruption_probe(id integer PRIMARY KEY);\n"
			if stage == "schema" {
				fault += "SELECT recovery_interrupt_session();\n"
			} else {
				fault += fmt.Sprintf("-- +goose StatementBegin\nCREATE FUNCTION recovery_interrupt_version() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM recovery_interrupt_session(); RETURN NEW; END $$;\n-- +goose StatementEnd\nCREATE TRIGGER recovery_interrupt_version AFTER INSERT ON goose_db_version FOR EACH ROW WHEN (NEW.version_id=%d AND NEW.is_applied) EXECUTE FUNCTION recovery_interrupt_version();\n", target)
			}
			interrupted[targetName] = &fstest.MapFile{Data: []byte(up + fault + "-- +goose Down" + down)}
			if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION recovery_interrupt_session() RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS 'SELECT pg_terminate_backend(pg_backend_pid())'`); err != nil {
				t.Fatal(err)
			}
			err := postgres.Migrate(t.Context(), migration, interrupted)
			var terminated *pgconn.PgError
			if !errors.As(err, &terminated) || terminated.Code != "57P01" {
				t.Fatalf("interruption helper: expected terminated PostgreSQL session: %v", err)
			}
			if _, err := admin.ExecContext(t.Context(), `DROP FUNCTION recovery_interrupt_session()`); err != nil {
				t.Fatal(err)
			}
			var probe bool
			if err := admin.QueryRowContext(t.Context(), "SELECT to_regclass('public.recovery_interruption_probe') IS NOT NULL").Scan(&probe); err != nil {
				t.Fatal(err)
			}
			if got := hubPostgresMigrationLedger(t, admin); probe || !reflect.DeepEqual(got, ledger) || recoveryPostgresVersion(t, admin) != previousVersion {
				t.Fatalf("interrupted migration did not roll back: probe=%t ledger=%+v", probe, got)
			}
			tools := recoveryPostgresTools(t, admin)
			sourceAdmin := fixture.Connection(t, admin, "", "")
			sourceBefore := recoveryPostgresCanonicalDump(recoveryPostgresTool(t, tools, sourceAdmin, "pg_dump", "--format=plain", "--no-owner"))
			backup := filepath.Join(t.TempDir(), "pre-upgrade.dump")
			recoveryPostgresTool(t, tools, sourceAdmin, "pg_dump", "--format=custom", "--file="+backup)
			backupBefore, err := os.ReadFile(backup)
			if err != nil || len(backupBefore) == 0 {
				t.Fatalf("backup is unavailable: %v", err)
			}
			recoveryPostgresTool(t, tools, sourceAdmin, "pg_restore", "--no-owner", "--file="+os.DevNull, backup)
			if recoveryPostgresVersion(t, admin) != previousVersion {
				t.Fatal("backup migrated the source")
			}
			sourceAfter := recoveryPostgresCanonicalDump(recoveryPostgresTool(t, tools, sourceAdmin, "pg_dump", "--format=plain", "--no-owner"))
			if !bytes.Equal(sourceBefore, sourceAfter) {
				t.Fatal("verify or backup changed legacy source")
			}
			restored := restoration.Database(t)
			restoreAdmin := restoration.Connection(t, restored, "", "")
			recoveryPostgresTool(t, tools, restoreAdmin, "pg_restore", "--exit-on-error", "--dbname="+restoreAdmin.Database, backup)
			restoreConnection := roleConnection(t, restoration, restored, "detent_migration_v2")
			if recoveryPostgresVersion(t, restored) != previousVersion || !reflect.DeepEqual(hubPostgresMigrationLedger(t, restored), ledger) {
				t.Fatal("backup did not restore the preserved migration state")
			}
			if err := postgres.Migrate(t.Context(), restoreConnection, files); err != nil {
				t.Fatal(err)
			}
			backupAfter, err := os.ReadFile(backup)
			if err != nil || !bytes.Equal(backupBefore, backupAfter) {
				t.Fatalf("restore changed source: %v", err)
			}
			if got := recoveryPostgresVersion(t, restored); got != currentVersion {
				t.Fatalf("migration retry: got=%d want=%d", got, currentVersion)
			}
			runtime := roleConnection(t, restoration, restored, "detent_runtime_v2")
			repository, err := postgres.Open(t.Context(), postgres.Config{Connection: runtime, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}, files)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := repository.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, organization := range []string{"org_recovery_first", "org_recovery_second"} {
				if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
					project, err := r.Project(t.Context(), "prj_recovery")
					if err == nil && project.Name != "Recovered project" {
						return errors.New("recovered project differs")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func recoveryPostgresVersion(t *testing.T, admin *sql.DB) int64 {
	t.Helper()
	var version int64
	if err := admin.QueryRowContext(t.Context(), "SELECT coalesce(max(version_id),0) FROM goose_db_version WHERE is_applied").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func recoveryPostgresTools(t *testing.T, admin *sql.DB) string {
	t.Helper()
	init, err := exec.LookPath("initdb")
	if err != nil {
		t.Fatal("PostgreSQL fixture tool directory is unavailable")
	}
	init, err = filepath.EvalSymlinks(init)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(init)
	var server int
	if err := admin.QueryRowContext(t.Context(), "SELECT current_setting('server_version_num')::integer / 10000").Scan(&server); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"initdb", "pg_dump", "pg_restore"} {
		path, err := exec.LookPath(filepath.Join(directory, tool))
		if err != nil {
			t.Fatalf("PostgreSQL fixture tool %s is unavailable", tool)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || filepath.Dir(resolved) != directory {
			t.Fatalf("PostgreSQL fixture tool %s is outside its declared directory", tool)
		}
		output, err := exec.CommandContext(t.Context(), resolved, "--version").Output()
		if err != nil || !strings.Contains(string(output), fmt.Sprintf("(PostgreSQL) %d.", server)) {
			t.Fatalf("PostgreSQL fixture tool %s does not match server major %d", tool, server)
		}
	}
	return directory
}

func recoveryPostgresTool(t *testing.T, directory string, connection pgtransport.Connection, tool string, args ...string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), filepath.Join(directory, tool), args...)
	environment := slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "PG") })
	command.Env = append(environment, "PGHOST="+connection.Host, "PGPORT="+strconv.Itoa(int(connection.Port)), "PGDATABASE="+connection.Database, "PGUSER="+connection.User, "PGPASSWORD="+connection.Password, "PGSSLMODE="+connection.SSLMode)
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := command.Run(); err != nil {
		t.Fatalf("PostgreSQL fixture tool %s failed: %v: %s", tool, err, diagnostic.String())
	}
	return output.Bytes()
}

func recoveryPostgresCanonicalDump(raw []byte) []byte {
	var canonical bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("\\restrict ")) || bytes.HasPrefix(line, []byte("\\unrestrict ")) {
			continue
		}
		canonical.Write(line)
		canonical.WriteByte('\n')
	}
	return canonical.Bytes()
}
