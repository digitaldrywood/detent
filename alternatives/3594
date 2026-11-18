package schemaupgrade_test

import (
	"io/fs"
	"os"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/pressly/goose/v3"
)

func TestActivityHistoryCutoffIndexBuildsOnlineAndRecoversInvalidAttempts(t *testing.T) {
	t.Parallel()
	const migrationVersion = 20261118021624
	const cutoff = "collaboration_events_activity_history_cutoff_idx"
	const previous = "collaboration_events_activity_history_idx"
	const cutoffDefinition = "CREATE INDEX collaboration_events_activity_history_cutoff_idx ON public.collaboration_events USING btree (organization_id, project_id, work_item_id, sequence DESC, recorded_at) INCLUDE (type, from_state, to_state) WHERE (type = ANY (ARRAY['issue.created'::text, 'workflow.transitioned'::text]))"
	const previousDefinition = "CREATE INDEX collaboration_events_activity_history_idx ON public.collaboration_events USING btree (organization_id, project_id, work_item_id, sequence DESC) INCLUDE (type, from_state, to_state, recorded_at) WHERE (type = ANY (ARRAY['issue.created'::text, 'workflow.transitioned'::text]))"
	db, _ := pgstoretest.MigrationDatabaseAtVersion(t, 20261117043721)
	db.SetMaxOpenConns(1)
	pgstoretest.SeedCore(t, db)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT o.organization_id,'prj_shared','wi_shared','event_'||n,n,CASE WHEN n=1 THEN 'issue.created' ELSE 'workflow.transitioned' END,1,'human','example','Todo','Done',now()-n*interval '1 minute','orgs/'||o.organization_id||'/blobs/sha256/00/'||lpad(to_hex(n),64,'0'),lpad(to_hex(n),64,'0'),17,29
 FROM (VALUES('org_first'),('org_second')) o(organization_id) CROSS JOIN generate_series(1,2) n;
 CREATE TABLE activity_index_locks(name text,mode text);
 GRANT INSERT ON activity_index_locks TO detent_migration_v2;
 CREATE FUNCTION capture_activity_index_locks() RETURNS event_trigger LANGUAGE plpgsql AS $$
 BEGIN
 INSERT INTO activity_index_locks SELECT c.relname,COALESCE(l.mode,'released') FROM pg_event_trigger_ddl_commands() d
 JOIN pg_class c ON c.oid=d.objid JOIN pg_index i ON i.indexrelid=c.oid
 LEFT JOIN pg_locks l ON l.relation=i.indrelid AND l.pid=pg_backend_pid()
 WHERE c.relname IN ('collaboration_events_activity_history_cutoff_idx','collaboration_events_activity_history_idx');
 END; $$;
 CREATE EVENT TRIGGER activity_index_locks ON ddl_command_end WHEN TAG IN ('CREATE INDEX') EXECUTE FUNCTION capture_activity_index_locks();`); err != nil {
		t.Fatal(err)
	}
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	asMigration := func(t *testing.T, run func() error) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
			t.Fatal(err)
		}
		runErr := run()
		if _, err := db.ExecContext(t.Context(), "RESET ROLE"); err != nil {
			t.Fatal(err)
		}
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	assertIndex := func(t *testing.T, present, absent, definition string) {
		t.Helper()
		var online, blocking, valid bool
		var got string
		if err := db.QueryRowContext(t.Context(), `SELECT
 EXISTS(SELECT 1 FROM activity_index_locks WHERE name=$1 AND mode IN ('ShareUpdateExclusiveLock','released')),
 EXISTS(SELECT 1 FROM activity_index_locks WHERE name=$1 AND mode IN ('ShareLock','AccessExclusiveLock')),
 indisvalid AND indisready,pg_get_indexdef(indexrelid)
 FROM pg_index WHERE indexrelid=to_regclass($1)`, present).Scan(&online, &blocking, &valid, &got); err != nil {
			t.Fatal(err)
		}
		if !online || blocking || !valid || got != definition {
			t.Fatalf("%s online=%v blocking=%v valid=%v definition=%s", present, online, blocking, valid, got)
		}
		var leftover bool
		if err := db.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, absent).Scan(&leftover); err != nil || leftover {
			t.Fatalf("%s still present=%v error=%v", absent, leftover, err)
		}
		for _, organization := range []string{"org_first", "org_second"} {
			var events int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id='wi_shared' AND id IN ('event_1','event_2')`, organization).Scan(&events); err != nil || events != 2 {
				t.Fatalf("tenant %s events=%d error=%v", organization, events, err)
			}
		}
	}
	asMigration(t, func() error { _, err := provider.UpTo(t.Context(), migrationVersion); return err })
	assertIndex(t, cutoff, previous, cutoffDefinition)
	if applied, err := provider.UpTo(t.Context(), migrationVersion); err != nil || len(applied) != 0 {
		t.Fatalf("repeat versioned Up applied=%d error=%v", len(applied), err)
	}
	asMigration(t, func() error { _, err := provider.Down(t.Context()); return err })
	assertIndex(t, previous, cutoff, previousDefinition)
	asMigration(t, func() error { _, err := provider.UpTo(t.Context(), migrationVersion); return err })
	assertIndex(t, cutoff, previous, cutoffDefinition)
	files, err := fs.Glob(os.DirFS("../migrations"), "*_activity_history_cutoff.sql")
	if err != nil || len(files) != 1 {
		t.Fatalf("activity history cutoff migrations=%v error=%v", files, err)
	}
	body, err := os.ReadFile("../migrations/" + files[0])
	if err != nil {
		t.Fatal(err)
	}
	replay, err := goose.NewProvider(goose.DialectPostgres, db, fstest.MapFS{files[0]: &fstest.MapFile{Data: body}},
		goose.WithDisableVersioning(true), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE activity_index_retry(value integer);
 ALTER TABLE activity_index_retry OWNER TO detent_migration_v2;
 INSERT INTO activity_index_retry VALUES(1),(1)`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"absent", "invalid", "valid"} {
		t.Run(state, func(t *testing.T) {
			if state != "valid" {
				if _, err := db.ExecContext(t.Context(), "DROP INDEX CONCURRENTLY "+cutoff); err != nil {
					t.Fatal(err)
				}
			}
			if state == "invalid" {
				if _, err := db.ExecContext(t.Context(), "CREATE UNIQUE INDEX CONCURRENTLY "+cutoff+" ON activity_index_retry(value)"); err == nil {
					t.Fatal("duplicate fixture did not fail the concurrent build")
				}
				var valid bool
				if err := db.QueryRowContext(t.Context(), `SELECT indisvalid FROM pg_index WHERE indexrelid=to_regclass($1)`, cutoff).Scan(&valid); err != nil || valid {
					t.Fatalf("invalid fixture valid=%v error=%v", valid, err)
				}
			}
			if _, err := db.ExecContext(t.Context(), "TRUNCATE activity_index_locks"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				asMigration(t, func() error { _, err := replay.Up(t.Context()); return err })
				assertIndex(t, cutoff, previous, cutoffDefinition)
			}
		})
	}
}
