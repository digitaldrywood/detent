package postgres_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

func TestBoardIndexesBuildOnlineAndRecoverInvalidAttempts(t *testing.T) {
	db, _ := migrationDatabaseAtVersion(t, 20261106215528)
	seedCore(t, db)
	indexes := []string{"native_board_page_pending_idx", "native_board_page_refresh_due", "native_board_dirty_oldest"}
	_, err := db.ExecContext(t.Context(), `CREATE TABLE board_index_locks(name text,mode text);
 CREATE FUNCTION capture_board_index_locks() RETURNS event_trigger LANGUAGE plpgsql AS $$
 BEGIN
 INSERT INTO board_index_locks SELECT c.relname,l.mode FROM pg_event_trigger_ddl_commands() d
 JOIN pg_class c ON c.oid=d.objid JOIN pg_index i ON i.indexrelid=c.oid
 JOIN pg_locks l ON l.relation=i.indrelid AND l.pid=pg_backend_pid()
 WHERE c.relname IN ('native_board_page_pending_idx','native_board_page_refresh_due','native_board_dirty_oldest');
 END; $$;
 CREATE EVENT TRIGGER board_index_locks ON ddl_command_end WHEN TAG IN ('CREATE INDEX') EXECUTE FUNCTION capture_board_index_locks();`)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(t.Context()); err != nil {
		t.Fatal(err)
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
	files, err := fs.Glob(os.DirFS("migrations"), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := fstest.MapFS{}
	for _, file := range files {
		body, err := os.ReadFile("migrations/" + file)
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
