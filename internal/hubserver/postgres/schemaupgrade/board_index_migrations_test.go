package schemaupgrade_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/pressly/goose/v3"
)

func TestBoardIndexesBuildOnlineAndRecoverInvalidAttempts(t *testing.T) {
	t.Parallel()
	db, _ := pgstoretest.MigrationDatabaseAtVersion(t, 20261107004841)
	pgstoretest.SeedCore(t, db)
	for _, org := range []string{"org_first", "org_second"} {
		for _, statement := range []string{
			`INSERT INTO native_board_sequences(organization_id,project_id,sequence,replay_floor) VALUES($1,'prj_shared',1,0)`,
			`INSERT INTO native_board_deltas(organization_id,project_id,sequence,work_item_id,current_object_key,current_sha256,current_bytes,current_content_bytes) VALUES($1,'prj_shared',1,'wi_shared',$1||'/retained/board',repeat('a',64),17,29)`,
			`INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes) VALUES($1,'finding_retained','retained','example','flow','work_item','wi_shared',now(),now(),'watch','Retained finding','Inspect',$1||'/retained/health',repeat('b',64),19,31)`,
			`INSERT INTO health_finding_projects(organization_id,finding_id,project_id) VALUES($1,'finding_retained','prj_shared'),($1,'finding_retained','prj_other')`,
		} {
			if _, err := db.ExecContext(t.Context(), statement, org); err != nil {
				t.Fatal(err)
			}
		}
	}
	indexes := []string{"native_board_page_pending_idx", "native_board_page_refresh_due", "native_board_dirty_oldest", "native_board_list_pending_idx", "native_board_search_title_unicode", "native_work_item_search_body_unicode"}
	_, err := db.ExecContext(t.Context(), `CREATE TABLE board_index_locks(name text,mode text);
 CREATE FUNCTION capture_board_index_locks() RETURNS event_trigger LANGUAGE plpgsql AS $$
 BEGIN
 INSERT INTO board_index_locks SELECT c.relname,l.mode FROM pg_event_trigger_ddl_commands() d
 JOIN pg_class c ON c.oid=d.objid JOIN pg_index i ON i.indexrelid=c.oid
 JOIN pg_locks l ON l.relation=i.indrelid AND l.pid=pg_backend_pid()
 WHERE c.relname IN ('native_board_page_pending_idx','native_board_page_refresh_due','native_board_dirty_oldest','native_board_list_pending_idx','native_board_search_title_unicode','native_work_item_search_body_unicode');
 END; $$;
 CREATE EVENT TRIGGER board_index_locks ON ddl_command_end WHEN TAG IN ('CREATE INDEX') EXECUTE FUNCTION capture_board_index_locks();`)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_first", "org_second"} {
		var key, digest, topic, entity string
		var compressed, content, revision, links int64
		if err := db.QueryRowContext(t.Context(), `SELECT current_object_key,current_sha256,current_bytes,current_content_bytes,topic,entity_id FROM native_board_deltas WHERE organization_id=$1 AND project_id='prj_shared' AND sequence=1`, org).Scan(&key, &digest, &compressed, &content, &topic, &entity); err != nil || key != org+"/retained/board" || digest != strings.Repeat("a", 64) || compressed != 17 || content != 29 || topic != "card" || entity != "wi_shared" {
			t.Fatalf("upgrade altered retained board reference: %s %s %d %d %s %s: %v", key, digest, compressed, content, topic, entity, err)
		}
		if err := db.QueryRowContext(t.Context(), `SELECT revision,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes FROM health_findings WHERE organization_id=$1 AND id='finding_retained'`, org).Scan(&revision, &key, &digest, &compressed, &content); err != nil || revision != 1 || key != org+"/retained/health" || digest != strings.Repeat("b", 64) || compressed != 19 || content != 31 {
			t.Fatalf("upgrade altered retained health evidence: revision=%d key=%s error=%v", revision, key, err)
		}
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM health_finding_projects WHERE organization_id=$1 AND finding_id='finding_retained' AND project_id IN ('prj_shared','prj_other')`, org).Scan(&links); err != nil || links != 2 {
			t.Fatalf("upgrade lost typed project scope: links=%d error=%v", links, err)
		}
	}
	for _, name := range indexes {
		var concurrent, blocking bool
		if err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM board_index_locks WHERE name=$1 AND mode='ShareUpdateExclusiveLock'),EXISTS(SELECT 1 FROM board_index_locks WHERE name=$1 AND mode IN ('ShareLock','AccessExclusiveLock'))`, name).Scan(&concurrent, &blocking); err != nil {
			t.Fatal(err)
		}
		if blocking {
			t.Errorf("%s online build=%v blocking lock=%v", name, concurrent, blocking)
		}
	}
	files, err := fs.Glob(os.DirFS("../migrations"), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := fstest.MapFS{}
	for _, file := range files {
		body, err := os.ReadFile("../migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range indexes {
			if strings.Contains(string(body), "CREATE INDEX CONCURRENTLY "+name+" ") {
				source[file] = &fstest.MapFile{Data: body}
			}
		}
	}
	if len(source) == 0 {
		t.Fatal("board index builds have no independently replayable concurrent migration")
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE board_index_retry(value integer); INSERT INTO board_index_retry VALUES(1),(1)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range indexes {
		if _, err := db.ExecContext(t.Context(), "DROP INDEX CONCURRENTLY "+name); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), "CREATE UNIQUE INDEX CONCURRENTLY "+name+" ON board_index_retry(value)"); err == nil {
			t.Fatal("duplicate fixture did not leave an invalid index")
		}
		var valid bool
		if err := db.QueryRowContext(t.Context(), `SELECT indisvalid FROM pg_index WHERE indexrelid=to_regclass($1)`, name).Scan(&valid); err != nil || valid {
			t.Fatalf("invalid attempt %s valid=%v error=%v", name, valid, err)
		}
	}
	replay, err := goose.NewProvider(goose.DialectPostgres, db, source, goose.WithDisableVersioning(true), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := replay.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, name := range indexes {
			var ready bool
			if err := db.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready AND indrelid<>'board_index_retry'::regclass FROM pg_index WHERE indexrelid=to_regclass($1)`, name).Scan(&ready); err != nil || !ready {
				t.Fatalf("replayed %s ready=%v error=%v", name, ready, err)
			}
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		var count int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 2 {
			t.Errorf("tenant %s count=%d error=%v", org, count, err)
		}
	}
}
