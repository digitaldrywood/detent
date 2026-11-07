package postgres_test

import (
	"io/fs"
	"os"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

func TestShellPageIndexesBuildOnlineAndRecoverInvalidAttempts(t *testing.T) {
	db, _ := migrationDatabaseAtVersion(t, 20261107051229)
	db.SetMaxOpenConns(1)
	seedCore(t, db)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO project_onboarding_page_viewers
 (organization_id,project_id,viewer_key,viewer_native_only,viewer_projects,viewer_blocked_projects)
 SELECT organization_id,id,'example-viewer',false,ARRAY[id],ARRAY[]::text[] FROM projects;
 CREATE TABLE shell_index_locks(name text,mode text);
 GRANT INSERT ON shell_index_locks TO detent_migration_v2;
 CREATE FUNCTION capture_shell_index_locks() RETURNS event_trigger LANGUAGE plpgsql AS $$
 BEGIN
 INSERT INTO shell_index_locks SELECT c.relname,COALESCE(l.mode,'released') FROM pg_event_trigger_ddl_commands() d
 JOIN pg_class c ON c.oid=d.objid JOIN pg_index i ON i.indexrelid=c.oid
 LEFT JOIN pg_locks l ON l.relation=i.indrelid AND l.pid=pg_backend_pid()
 WHERE c.relname IN ('project_integration_page_queue','project_onboarding_page_next');
 END; $$;
 CREATE EVENT TRIGGER shell_index_locks ON ddl_command_end WHEN TAG IN ('CREATE INDEX') EXECUTE FUNCTION capture_shell_index_locks();`); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	indexes := []struct {
		name       string
		definition string
	}{
		{"project_integration_page_queue", "CREATE INDEX project_integration_page_queue ON public.project_integration_page_dirty USING btree (organization_id, attempted_at, project_id)"},
		{"project_onboarding_page_next", "CREATE INDEX project_onboarding_page_next ON public.project_onboarding_page_viewers USING btree (organization_id, attempted_at, project_id, viewer_key)"},
	}
	assertIndexes := func(t *testing.T) {
		t.Helper()
		for _, index := range indexes {
			var online, blocking, valid bool
			var definition string
			if err := db.QueryRowContext(t.Context(), `SELECT
 EXISTS(SELECT 1 FROM shell_index_locks WHERE name=$1 AND mode IN ('ShareUpdateExclusiveLock','released')),
 EXISTS(SELECT 1 FROM shell_index_locks WHERE name=$1 AND mode IN ('ShareLock','AccessExclusiveLock')),
 indisvalid AND indisready,pg_get_indexdef(indexrelid)
 FROM pg_index WHERE indexrelid=to_regclass($1)`, index.name).Scan(&online, &blocking, &valid, &definition); err != nil {
				t.Fatal(err)
			}
			if !online || blocking || !valid || definition != index.definition {
				t.Fatalf("%s online=%v blocking=%v valid=%v definition=%s", index.name, online, blocking, valid, definition)
			}
		}
		for _, organization := range []string{"org_first", "org_second"} {
			var integration, onboarding int
			if err := db.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM project_integration_page_dirty WHERE organization_id=$1),
 (SELECT count(*) FROM project_onboarding_page_viewers WHERE organization_id=$1)`, organization).Scan(&integration, &onboarding); err != nil {
				t.Fatal(err)
			}
			if integration != 2 || onboarding != 2 {
				t.Fatalf("tenant %s integration=%d onboarding=%d, want 2 each", organization, integration, onboarding)
			}
		}
	}
	assertIndexes(t)
	if applied, err := provider.Up(t.Context()); err != nil || len(applied) != 0 {
		t.Fatalf("repeat versioned Up applied=%d error=%v", len(applied), err)
	}
	if _, err := provider.Down(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertIndexes(t)
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertIndexes(t)
	files, err := fs.Glob(os.DirFS("migrations"), "*_shell_page_queue_indexes.sql")
	if err != nil || len(files) != 1 {
		t.Fatalf("concurrent queue migrations=%v error=%v", files, err)
	}
	body, err := os.ReadFile("migrations/" + files[0])
	if err != nil {
		t.Fatal(err)
	}
	replay, err := goose.NewProvider(goose.DialectPostgres, db, fstest.MapFS{files[0]: &fstest.MapFile{Data: body}},
		goose.WithDisableVersioning(true), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE shell_index_retry(value integer);
 ALTER TABLE shell_index_retry OWNER TO detent_migration_v2;
 INSERT INTO shell_index_retry VALUES(1),(1)`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"absent", "invalid", "valid"} {
		t.Run(state, func(t *testing.T) {
			for _, index := range indexes {
				if state == "valid" {
					continue
				}
				if _, err := db.ExecContext(t.Context(), "DROP INDEX CONCURRENTLY "+index.name); err != nil {
					t.Fatal(err)
				}
				if state == "invalid" {
					if _, err := db.ExecContext(t.Context(), "CREATE UNIQUE INDEX CONCURRENTLY "+index.name+" ON shell_index_retry(value)"); err == nil {
						t.Fatal("duplicate fixture did not fail the concurrent build")
					}
					var valid bool
					if err := db.QueryRowContext(t.Context(), `SELECT indisvalid FROM pg_index WHERE indexrelid=to_regclass($1)`, index.name).Scan(&valid); err != nil || valid {
						t.Fatalf("%s invalid fixture valid=%v error=%v", index.name, valid, err)
					}
				}
			}
			if _, err := db.ExecContext(t.Context(), "TRUNCATE shell_index_locks"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := db.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
					t.Fatal(err)
				}
				if _, err := replay.Up(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "RESET ROLE"); err != nil {
					t.Fatal(err)
				}
				assertIndexes(t)
			}
		})
	}
}
