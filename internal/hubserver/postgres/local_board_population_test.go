package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalBoardBillingActivityIndexesRecoverInterruptedBuild(t *testing.T) {
	const version = 20261110042737
	const name = "20261110042737_billing_activity_timestamp_indexes.sql"
	admin, _ := migrationDatabaseAtVersion(t, version-1)
	admin.SetMaxOpenConns(1)
	admin.SetMaxIdleConns(1)
	body, err := os.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(body), "DROP INDEX CONCURRENTLY IF EXISTS hosted_sessions_tenant_created_idx;", "SELECT 1/0;\nDROP INDEX CONCURRENTLY IF EXISTS hosted_sessions_tenant_created_idx;", 1)
	interrupted, err := goose.NewProvider(goose.DialectPostgres, admin, postgres.RoleMigrations(fstest.MapFS{name: &fstest.MapFile{Data: []byte(broken)}}), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := interrupted.UpTo(t.Context(), version); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("index interruption=%v", err)
	}
	var first, second bool
	if err := admin.QueryRowContext(t.Context(), `SELECT to_regclass('collaboration_events_tenant_recorded_idx') IS NOT NULL,to_regclass('hosted_sessions_tenant_created_idx') IS NOT NULL`).Scan(&first, &second); err != nil || !first || second {
		t.Fatalf("interrupted index artifacts=%t/%t: %v", first, second, err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE TABLE fixture_activity_index_retry(value integer); INSERT INTO fixture_activity_index_retry VALUES(1),(1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE UNIQUE INDEX CONCURRENTLY hosted_sessions_tenant_created_idx ON fixture_activity_index_retry(value)`); err == nil {
		t.Fatal("invalid index fixture did not fail")
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), version); err != nil {
		t.Fatal(err)
	}
	for _, index := range []struct{ name, table string }{
		{"collaboration_events_tenant_recorded_idx", "collaboration_events"}, {"hosted_sessions_tenant_created_idx", "hosted_sessions"},
	} {
		var ready bool
		var relation string
		if err := admin.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready,indrelid::regclass::text FROM pg_index WHERE indexrelid=to_regclass($1)`, index.name).Scan(&ready, &relation); err != nil || !ready || relation != index.table {
			t.Fatalf("recovered %s ready=%t source=%s: %v", index.name, ready, relation, err)
		}
	}
}

func TestLocalBoardRetainedEventsInitializeSnapshotDelta(t *testing.T) {
	const activationVersion = 20261110042748
	const initializationVersion = 20261110042758
	admin, fixture := migrationDatabaseAtVersion(t, 20261110042736)
	seedCore(t, admin)
	insert := `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,schema_version,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
VALUES($1,'prj_shared','wi_shared',$2,$3,1,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1)`
	for index, org := range []string{"org_first", "org_second"} {
		for n := 0; n <= index; n++ {
			if _, err := admin.ExecContext(t.Context(), insert, org, fmt.Sprintf("before_%d", n), n+1); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'retained_collaboration_events',999)`, org); err != nil {
			t.Fatal(err)
		}
	}
	admin.SetMaxOpenConns(1)
	admin.SetMaxIdleConns(1)
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), activationVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_first", "org_second"} {
		var value int64
		var ready bool
		if err := admin.QueryRowContext(t.Context(), `SELECT value,initialized FROM hosted_usage_counters WHERE organization_id=$1 AND name='retained_collaboration_events'`, org).Scan(&value, &ready); err != nil || value != 0 || ready {
			t.Fatalf("activation trusted old generic counter %s=%d/%t: %v", org, value, ready, err)
		}
	}
	var database string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `ALTER DATABASE `+pgx.Identifier{database}.Sanitize()+` CONNECTION LIMIT 10`); err != nil {
		t.Fatal(err)
	}
	pool := rolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	admin.SetMaxIdleConns(1)
	listener, err := pgx.ConnectConfig(t.Context(), pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := listener.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	if _, err := listener.Exec(t.Context(), `LISTEN retained_event_snapshot`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_retained_initialization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.name='retained_collaboration_events' AND NEW.organization_id='org_second' AND NEW.initialized AND NOT OLD.initialized THEN RAISE EXCEPTION 'injected retained publication failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_retained_initialization BEFORE UPDATE ON hosted_usage_counters FOR EACH ROW EXECUTE FUNCTION reject_retained_initialization()`); err != nil {
		t.Fatal(err)
	}
	pending := map[string]pgx.Tx{}
	var done chan error
	joined := false
	defer func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		for _, tx := range pending {
			if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error(err)
			}
		}
		if done != nil && !joined {
			<-done
		}
	}()
	for _, org := range []string{"org_first", "org_second"} {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		pending[org] = tx
		if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), insert, org, "during_snapshot", 100); err != nil {
			t.Fatal(err)
		}
	}

	source := os.DirFS("migrations")
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	overlay := fstest.MapFS{}
	for _, name := range files {
		data, err := fs.ReadFile(source, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "20261110042758_initialize_retained_event_population.sql" {
			needle := "IF baseline IS NULL THEN RAISE EXCEPTION 'retained event population baseline unavailable'; END IF;\n  COMMIT;"
			if strings.Count(string(data), needle) != 1 {
				t.Fatal("missing baseline publication boundary")
			}
			data = []byte(strings.Replace(string(data), needle, needle+"\n  PERFORM pg_notify('retained_event_snapshot',org);\n  COMMIT;", 1))
		}
		overlay[name] = &fstest.MapFile{Data: data}
	}
	interleaved, err := goose.NewProvider(goose.DialectPostgres, admin, postgres.RoleMigrations(overlay), goose.WithGoMigrations(postgres.ConversationMigrations(overlay)...), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	var current string
	var superuser bool
	if err := admin.QueryRowContext(t.Context(), `SELECT current_user,rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&current, &superuser); err != nil || current != postgres.MigrationRoleName || superuser {
		t.Fatalf("initializer role=%s superuser=%t error=%v", current, superuser, err)
	}
	notifyContext, stopNotifications := context.WithCancel(t.Context())
	defer stopNotifications()
	done = make(chan error, 1)
	go func() {
		_, err := interleaved.UpTo(t.Context(), initializationVersion)
		done <- err
		stopNotifications()
	}()

	for _, org := range []string{"org_first", "org_second"} {
		notification, err := listener.WaitForNotification(notifyContext)
		if err != nil {
			initializationError := <-done
			joined = true
			t.Fatalf("snapshot notification=%v, initializer=%v", err, initializationError)
		}
		if notification.Payload != org {
			t.Fatalf("snapshot order=%s, want%s", notification.Payload, org)
		}
		if err := pending[org].Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	err = <-done
	joined = true
	if err == nil || !strings.Contains(err.Error(), "injected retained publication failure") {
		t.Fatalf("initializer interruption=%v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE; DROP TRIGGER reject_retained_initialization ON hosted_usage_counters; DROP FUNCTION reject_retained_initialization()`); err != nil {
		t.Fatal(err)
	}
	for index, org := range []string{"org_first", "org_second"} {
		var value, history int64
		var ready bool
		if err := admin.QueryRowContext(t.Context(), `SELECT c.value,c.initialized,h.value FROM hosted_usage_counters c JOIN hosted_usage_counters h USING(organization_id) WHERE c.organization_id=$1 AND c.name='retained_collaboration_events' AND h.name='history_records'`, org).Scan(&value, &ready, &history); err != nil || ready != (index == 0) || value != int64(2-index) || history != int64(index+2) {
			t.Fatalf("interrupted %s=%d/%t history%d error%v", org, value, ready, history, err)
		}
		if _, err := admin.ExecContext(t.Context(), insert, org, "after_interruption", 101); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), initializationVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	for index, org := range []string{"org_first", "org_second"} {
		var value, history int64
		var ready bool
		if err := admin.QueryRowContext(t.Context(), `SELECT c.value,c.initialized,h.value FROM hosted_usage_counters c JOIN hosted_usage_counters h USING(organization_id) WHERE c.organization_id=$1 AND c.name='retained_collaboration_events' AND h.name='history_records'`, org).Scan(&value, &ready, &history); err != nil || !ready || value != int64(index+3) || history != value {
			t.Fatalf("resumed %s=%d/%t history%d error%v", org, value, ready, history, err)
		}
	}
}

func TestLocalBoardPopulationResumesWithoutLosingConcurrentSource(t *testing.T) {
	admin, fixture := migrationDatabaseAtVersion(t, 20261109030623)
	seedCore(t, admin)
	for _, org := range []string{"org_first", "org_second"} {
		for _, statement := range []string{
			`INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) SELECT $1,id,'Done',true,false,1 FROM projects WHERE organization_id=$1`,
			`UPDATE issues SET workflow_state_name='Done' WHERE organization_id=$1 AND native_id='wi_shared'`,
			`INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,project_id,native_id,native_id,1,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM issues WHERE organization_id=$1`,
			`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,schema_version,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,project_id,native_id,native_id,1,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM issues WHERE organization_id=$1`,
		} {
			if _, err := admin.ExecContext(t.Context(), statement, org); err != nil {
				t.Fatal(err)
			}
		}
	}
	admin.SetMaxOpenConns(1)
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 20261109030624); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_population_initialization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.initialized AND NOT OLD.initialized THEN RAISE EXCEPTION 'injected population publication failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_population_initialization BEFORE UPDATE ON hosted_usage_counters FOR EACH ROW EXECUTE FUNCTION reject_population_initialization()`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 20261109030812); err == nil || !strings.Contains(err.Error(), "injected population publication failure") {
		t.Fatalf("publication interruption: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE; DROP TRIGGER reject_population_initialization ON hosted_usage_counters; DROP FUNCTION reject_population_initialization()`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_first", "org_second"} {
		for _, statement := range []string{
			`INSERT INTO issues(organization_id,native_id,project_id,number,title) VALUES($1,'wi_during','prj_shared',2,'Example during initialization')`,
			`UPDATE project_workflow_states SET terminal=false WHERE organization_id=$1 AND project_id='prj_shared'`,
			`INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES($1,'prj_shared','wi_shared','during',1,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1)`,
		} {
			if _, err := admin.ExecContext(t.Context(), statement, org); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	var database string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 4"); err != nil {
		t.Fatal(err)
	}
	pool := rolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	assert := func(t *testing.T, org string, active, history int64) {
		t.Helper()
		err := repository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
			reader := r.(interface {
				LocalBoardImportAllowanceUsage(context.Context) (int64, int64, error)
			})
			a, h, err := reader.LocalBoardImportAllowanceUsage(t.Context())
			if err == nil && (a != active || h != history) {
				return fmt.Errorf("population %s = %d/%d, want %d/%d", org, a, h, active, history)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		assert(t, org, 3, 5)
		for _, test := range []struct {
			name, statement string
			active, history int64
		}{
			{"archive null state", `UPDATE issues SET archived=true WHERE organization_id=$1 AND native_id='wi_other'`, 2, 5},
			{"terminal definition", `UPDATE project_workflow_states SET terminal=true WHERE organization_id=$1 AND project_id='prj_shared'`, 1, 5},
			{"project profile", `UPDATE projects SET profile='github_compatible' WHERE organization_id=$1 AND id='prj_shared'`, 0, 5},
			{"restore profile", `UPDATE projects SET profile='native' WHERE organization_id=$1 AND id='prj_shared'`, 1, 5},
			{"restore issue", `UPDATE issues SET archived=false WHERE organization_id=$1 AND native_id='wi_other'`, 2, 5},
			{"move to terminal", `UPDATE issues SET workflow_state_name='Done' WHERE organization_id=$1 AND native_id='wi_other'`, 1, 5},
			{"leave definition", `UPDATE issues SET workflow_state_name=NULL WHERE organization_id=$1 AND native_id='wi_other'`, 2, 5},
			{"delete definition", `DELETE FROM project_workflow_states WHERE organization_id=$1 AND project_id='prj_other'`, 2, 5},
			{"restore definition", `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) VALUES($1,'prj_other','Done',true,false,1)`, 2, 5},
			{"reenter definition", `UPDATE issues SET workflow_state_name='Done' WHERE organization_id=$1 AND native_id='wi_other'`, 1, 5},
			{"project retirement", `UPDATE projects SET deleted_at=now() WHERE organization_id=$1 AND id='prj_shared'`, 0, 5},
			{"project restoration", `UPDATE projects SET deleted_at=NULL WHERE organization_id=$1 AND id='prj_shared'`, 1, 5},
			{"delete null state", `DELETE FROM issues WHERE organization_id=$1 AND native_id='wi_during'`, 0, 5},
			{"immutable version", `DELETE FROM collaboration_versions WHERE organization_id=$1 AND record_id='during'`, 0, 5},
			{"immutable event", `DELETE FROM collaboration_events WHERE organization_id=$1 AND id='wi_shared'`, 0, 5},
		} {
			t.Run(org+"/"+test.name, func(t *testing.T) {
				_, err := admin.ExecContext(t.Context(), test.statement, org)
				if strings.HasPrefix(test.name, "immutable") {
					var pgError *pgconn.PgError
					if !errors.As(err, &pgError) || pgError.Code != "23514" {
						t.Fatalf("immutable history deletion: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				assert(t, org, test.active, test.history)
				if org == "org_first" {
					assert(t, "org_second", 3, 5)
				}
			})
		}
	}
	for _, org := range []tracker.OrganizationID{"org_attempt_first", "org_attempt_second"} {
		seedLeaseFamily(t, repository, org)
		assert(t, string(org), 2, 0)
		for _, id := range []int64{1, 2} {
			lease := claimLease(t, repository, org, leaseClaim(id))
			for range 2 {
				recordLeaseAttempt(t, repository, org, attemptEvent(lease))
				assert(t, string(org), 2, id)
			}
		}
	}
	for _, query := range []string{
		`INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES('org_first','history_records',0)`,
		`UPDATE hosted_usage_counters SET initialized=false WHERE organization_id='org_first'`,
		`UPDATE archive_usage_counters SET population_revision=0 WHERE organization_id='org_first'`,
		`DELETE FROM archive_usage_counters WHERE organization_id='org_first'`,
		`DELETE FROM local_board_populations WHERE organization_id='org_first'`,
		`SELECT local_board_population_delta('org_first','prj_shared','project',false,'',100)`,
	} {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, "org_first"); err != nil {
			t.Fatal(err)
		}
		_, denied := tx.Exec(t.Context(), query)
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		var pgError *pgconn.PgError
		if !errors.As(denied, &pgError) || pgError.Code != "42501" {
			t.Fatalf("runtime accounting permission through %q: %v", query, denied)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	pool = rolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err = postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, org := range []string{"org_first", "org_second"} {
		assert(t, org, 0, 5)
	}
	for _, org := range []string{"org_attempt_first", "org_attempt_second"} {
		assert(t, org, 2, 2)
	}
}

func TestLocalBoardHistoryPopulationBatchesCounterWrites(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	if _, err := admin.ExecContext(t.Context(), `
CREATE TABLE fixture_history_counter_updates(organization_id text PRIMARY KEY,writes bigint NOT NULL);
CREATE FUNCTION record_fixture_history_counter_update() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 INSERT INTO public.fixture_history_counter_updates(organization_id,writes) VALUES(NEW.organization_id,1)
 ON CONFLICT(organization_id) DO UPDATE SET writes=public.fixture_history_counter_updates.writes+1;
 RETURN NEW;
END $$;
CREATE TRIGGER fixture_history_counter_update AFTER UPDATE OF value ON hosted_usage_counters
FOR EACH ROW WHEN (NEW.name='history_records') EXECUTE FUNCTION record_fixture_history_counter_update();`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, statement string
		history, writes int64
		wantState       string
	}{
		{"bulk events", `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,schema_version,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT organization_id,project_id,native_id,'event_'||native_id||'_'||n,n,1,'orgs/'||organization_id||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
FROM issues CROSS JOIN generate_series(1,10) n`, 20, 1, ""},
		{"bulk versions", `INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT organization_id,project_id,native_id,'version_'||native_id||'_'||n,1,'orgs/'||organization_id||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
FROM issues CROSS JOIN generate_series(1,10) n`, 40, 2, ""},
		{"rejected batch is atomic", `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,schema_version,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT organization_id,project_id,native_id,'repeated_'||native_id,n+10,1,'orgs/'||organization_id||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
FROM issues CROSS JOIN generate_series(1,2) n`, 40, 2, "23505"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := admin.ExecContext(t.Context(), test.statement)
			if test.wantState != "" {
				var pgError *pgconn.PgError
				if !errors.As(err, &pgError) || pgError.Code != test.wantState {
					t.Fatalf("batch error = %v, want SQLSTATE %s", err, test.wantState)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			for _, org := range []string{"org_first", "org_second"} {
				var history, writes int64
				if err := admin.QueryRowContext(t.Context(), `SELECT c.value,u.writes FROM hosted_usage_counters c JOIN fixture_history_counter_updates u USING(organization_id) WHERE c.organization_id=$1 AND c.name='history_records'`, org).Scan(&history, &writes); err != nil {
					t.Fatal(err)
				}
				var retained int64
				if err := admin.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='retained_collaboration_events'`, org).Scan(&retained); err != nil || retained != 20 {
					t.Fatalf("retained batch count=%d error=%v", retained, err)
				}
				if history != test.history || writes != test.writes {
					t.Errorf("%s history=%d counter writes=%d, want %d/%d", org, history, writes, test.history, test.writes)
				}
			}
		})
	}
}

func TestLocalBoardIssuePopulationBatchesCounterWrites(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	if _, err := admin.ExecContext(t.Context(), `
INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank)
SELECT organization_id,id,s.name,s.terminal,NOT s.terminal,s.rank FROM projects CROSS JOIN (VALUES('Todo',false,1),('Done',true,2)) s(name,terminal,rank);
UPDATE projects SET profile='github_compatible' WHERE id='prj_other';
CREATE TABLE fixture_issue_counter_updates(organization_id text PRIMARY KEY,writes bigint NOT NULL);
INSERT INTO fixture_issue_counter_updates SELECT organization_id,0 FROM organizations;
CREATE TABLE fixture_issue_project_updates(organization_id text NOT NULL,project_id text NOT NULL,before_sequence bigint NOT NULL,writes bigint NOT NULL,PRIMARY KEY(organization_id,project_id));
INSERT INTO fixture_issue_project_updates SELECT organization_id,id,duplicate_source_sequence,0 FROM projects;
CREATE FUNCTION record_fixture_issue_counter_update() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 UPDATE public.fixture_issue_counter_updates SET writes=writes+1 WHERE organization_id=NEW.organization_id;
 RETURN NEW;
END $$;
CREATE TRIGGER fixture_issue_counter_update AFTER UPDATE OF value ON hosted_usage_counters FOR EACH ROW WHEN(NEW.name='unarchived_issues') EXECUTE FUNCTION record_fixture_issue_counter_update();
CREATE FUNCTION record_fixture_issue_project_update() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 UPDATE public.fixture_issue_project_updates SET writes=writes+1 WHERE organization_id=NEW.organization_id AND project_id=NEW.id;
 RETURN NEW;
END $$;
CREATE TRIGGER fixture_issue_project_update AFTER UPDATE OF duplicate_source_sequence ON projects FOR EACH ROW EXECUTE FUNCTION record_fixture_issue_project_update();`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, statement                                      string
		active, writes, projectActive, nullState, todo, done int64
		sourceDelta, sourceWrites                            int64
		wantState                                            string
	}{
		{"bulk create", `INSERT INTO issues(organization_id,id,project_id,native_id,number,title,workflow_state_name) SELECT organization_id,CASE WHEN id='prj_shared' THEN 100 ELSE 200 END+n,id,'bulk_'||id||'_'||n,n+10,'Example','Todo' FROM projects CROSS JOIN generate_series(1,10) n`, 11, 1, 11, 1, 10, 0, 10, 1, ""},
		{"terminal transition", `UPDATE issues SET workflow_state_name='Done' WHERE native_id LIKE 'bulk_%'`, 1, 2, 1, 1, 0, 10, 20, 11, ""},
		{"null transition", `UPDATE issues SET workflow_state_name=NULL WHERE native_id LIKE 'bulk_%'`, 11, 3, 11, 11, 0, 0, 30, 21, ""},
		{"archive", `UPDATE issues SET archived=true WHERE native_id LIKE 'bulk_%'`, 1, 4, 1, 1, 0, 0, 40, 31, ""},
		{"unarchive terminal", `UPDATE issues SET archived=false,workflow_state_name='Done' WHERE native_id LIKE 'bulk_%'`, 1, 4, 1, 1, 0, 10, 50, 41, ""},
		{"delete terminal", `DELETE FROM issues WHERE native_id LIKE 'bulk_%'`, 1, 4, 1, 1, 0, 0, 60, 42, ""},
		{"no-op population update", `UPDATE issues SET title='Changed',archived=archived,workflow_state_name=workflow_state_name`, 1, 4, 1, 1, 0, 0, 61, 43, ""},
		{"rejected batch is atomic", `INSERT INTO issues(organization_id,id,project_id,native_id,number,title) SELECT organization_id,CASE WHEN id='prj_shared' THEN 300 ELSE 400 END+n,id,'rejected_'||id||'_'||n,n+30,(1/(n-2))::text FROM projects CROSS JOIN generate_series(1,2) n`, 1, 4, 1, 1, 0, 0, 61, 43, "22012"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := admin.ExecContext(t.Context(), test.statement)
			if test.wantState != "" {
				var pgError *pgconn.PgError
				if !errors.As(err, &pgError) || pgError.Code != test.wantState {
					t.Fatalf("batch error = %v, want SQLSTATE %s", err, test.wantState)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			for _, org := range []string{"org_first", "org_second"} {
				var active, writes int64
				var initialized bool
				if err := admin.QueryRowContext(t.Context(), `SELECT c.value,c.initialized,u.writes FROM hosted_usage_counters c JOIN fixture_issue_counter_updates u USING(organization_id) WHERE c.organization_id=$1 AND c.name='unarchived_issues'`, org).Scan(&active, &initialized, &writes); err != nil {
					t.Fatal(err)
				}
				if active != test.active || writes != test.writes || !initialized {
					t.Errorf("%s active=%d writes=%d initialized=%t, want %d/%d/true", org, active, writes, initialized, test.active, test.writes)
				}
				for _, project := range []string{"prj_shared", "prj_other"} {
					var projectActive, nullState, todo, done, delta, sourceWrites int64
					if err := admin.QueryRowContext(t.Context(), `SELECT
coalesce((SELECT value FROM local_board_populations WHERE organization_id=$1 AND project_id=$2 AND kind='project'),0),
coalesce((SELECT value FROM local_board_populations WHERE organization_id=$1 AND project_id=$2 AND kind='state' AND NOT has_state),0),
coalesce((SELECT value FROM local_board_populations WHERE organization_id=$1 AND project_id=$2 AND kind='state' AND has_state AND state_name='Todo'),0),
coalesce((SELECT value FROM local_board_populations WHERE organization_id=$1 AND project_id=$2 AND kind='state' AND has_state AND state_name='Done'),0),p.duplicate_source_sequence-u.before_sequence,u.writes
FROM projects p JOIN fixture_issue_project_updates u ON u.organization_id=p.organization_id AND u.project_id=p.id WHERE p.organization_id=$1 AND p.id=$2`, org, project).Scan(&projectActive, &nullState, &todo, &done, &delta, &sourceWrites); err != nil {
						t.Fatal(err)
					}
					if projectActive != test.projectActive || nullState != test.nullState || todo != test.todo || done != test.done || delta != test.sourceDelta || sourceWrites != test.sourceWrites {
						t.Errorf("%s/%s project=%d null=%d todo=%d done=%d source=%d/%d, want %d/%d/%d/%d/%d/%d", org, project, projectActive, nullState, todo, done, delta, sourceWrites, test.projectActive, test.nullState, test.todo, test.done, test.sourceDelta, test.sourceWrites)
					}
				}
			}
		})
	}
}
