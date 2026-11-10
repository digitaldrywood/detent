package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const upgradeOrderHead int64 = 20261109184650

var upgradeOrderPending = []int64{
	20261108175144,
	20261108215450,
	20261108224926,
	conversationContinuationBackfillVersion,
	20261108232743,
	20261108233605,
	20261109012539,
	20261109030624,
	20261109030812,
	20261109033258,
	20261109033415,
	20261109060057,
	20261109073347,
	20261109092232,
	20261109092607,
	20261109100704,
	20261109123421,
	20261109163830,
	20261109164729,
	20261109182156,
	20261109192211,
	20261109195442,
	20261109212021,
	20261109222640,
	20261109224251,
	20261110013206,
}

var upgradeOrderTenants = []struct {
	organization tracker.OrganizationID
	weight       int64
	continuation bool
}{
	{organization: "org_first", weight: 1, continuation: true},
	{organization: "org_second", weight: 2, continuation: false},
}

func TestMigrateAppliesPendingVersionsBelowPopulatedHead(t *testing.T) {
	const concurrentIndexes = "20261109033415_doctor_proposal_identity_indexes.sql"
	source := os.DirFS("migrations")
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	head := populatedHeadMigrations(t)
	if registered := ConversationMigrations(head); len(registered) != 0 {
		t.Fatalf("populated head registered %d conversation migrations", len(registered))
	}
	if registered := ConversationMigrations(source); len(registered) != 1 || registered[0].Version != conversationContinuationBackfillVersion {
		t.Fatalf("full source conversation migrations = %+v", registered)
	}
	populated := PopulatedHeadTemplate(t, seedUpgradeOrderTenants)
	want := catalogSnapshot(t, migratedTemplate(t, source, nil).Database(t))

	for _, interrupted := range []bool{false, true} {
		name := "upgrade"
		if interrupted {
			name = "resume interrupted concurrent index"
		}
		t.Run(name, func(t *testing.T) {
			db := populated.Database(t)
			config := migrationTestLogin(t, db, MigrationRoleName)
			var count int
			var version int64
			if err := db.QueryRowContext(t.Context(), "SELECT count(*), max(version_id) FROM goose_db_version WHERE version_id > 0 AND is_applied").Scan(&count, &version); err != nil {
				t.Fatal(err)
			}
			if count != 223 || version != upgradeOrderHead {
				t.Fatalf("populated head ledger = %d/%d", count, version)
			}
			if err := validateSchema(t.Context(), db, source); !errors.Is(err, ErrSchema) {
				t.Fatalf("incomplete schema validation = %v", err)
			}
			if slices.Equal(catalogSnapshot(t, db), want) {
				t.Fatal("populated head catalog already matches a fresh install")
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == concurrentIndexes {
						const second = "DROP INDEX CONCURRENTLY IF EXISTS doctor_proposal_identity_state;"
						replaced := strings.Replace(string(data), second, "SELECT 1/0;\n"+second, 1)
						if replaced == string(data) {
							t.Fatal("concurrent index migration has no second build to interrupt")
						}
						data = []byte(replaced)
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "22012" {
					t.Fatalf("partial migration error = %v, want division by zero", err)
				}
				if err := validateSchema(t.Context(), db, source); !errors.Is(err, ErrSchema) {
					t.Fatalf("partial schema validation = %v", err)
				}
				var applied int64
				if err := db.QueryRowContext(t.Context(), "SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1").Scan(&applied); err != nil {
					t.Fatal(err)
				}
				if applied != 20261109033258 {
					t.Fatalf("last version applied before interruption = %d", applied)
				}
				var built, remaining bool
				if err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_index WHERE indexrelid=to_regclass('doctor_proposal_unknown_heads') AND indisvalid), to_regclass('doctor_proposal_identity_state') IS NOT NULL`).Scan(&built, &remaining); err != nil {
					t.Fatal(err)
				}
				if !built || remaining {
					t.Fatalf("interrupted index builds: first=%t second=%t", built, remaining)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE pg_index SET indisvalid=false WHERE indexrelid='doctor_proposal_unknown_heads'::regclass`); err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(t.Context(), config, source); err != nil {
				t.Fatalf("upgrade populated head: %v", err)
			}
			if err := validateSchema(t.Context(), db, source); err != nil {
				t.Fatal(err)
			}
			rows, err := db.QueryContext(t.Context(), "SELECT version_id FROM goose_db_version WHERE is_applied AND id>(SELECT max(id) FROM goose_db_version WHERE version_id=$1) ORDER BY id", upgradeOrderHead)
			if err != nil {
				t.Fatal(err)
			}
			var applied []int64
			for rows.Next() {
				var version int64
				if err := rows.Scan(&version); err != nil {
					t.Fatal(err)
				}
				applied = append(applied, version)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(applied, upgradeOrderPending) {
				t.Fatalf("pending application order = %v, want %v", applied, upgradeOrderPending)
			}
			var before, after int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM goose_db_version").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(t.Context(), config, source); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM goose_db_version").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("idempotent migration changed ledger: %d -> %d", before, after)
			}
			if got := catalogSnapshot(t, db); !slices.Equal(got, want) {
				t.Errorf("upgraded catalog differs from a fresh install:\n%s", catalogDifference(want, got))
			}
			for _, name := range []string{"collaboration_events_visible_history", "project_artifact_retained_uploads", "doctor_proposal_unknown_heads", "doctor_proposal_identity_state", "native_command_operator_project_active", "native_attempts_local_identity_idx", "native_attempts_local_identity_unknown_idx"} {
				var valid bool
				if err := db.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid=$1::regclass`, name).Scan(&valid); err != nil || !valid {
					t.Errorf("index %s valid=%t error=%v", name, valid, err)
				}
			}
			for _, tenant := range upgradeOrderTenants {
				t.Run(string(tenant.organization)+" backfills", func(t *testing.T) {
					requireUpgradeOrderBackfills(t, db, tenant.organization, tenant.weight, tenant.continuation)
				})
			}
			runtime := migrationTestLogin(t, db, RuntimeRoleName)
			repository, err := Open(t.Context(), Config{Connection: runtime, Pool: PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}, source)
			if err != nil {
				t.Fatalf("open upgraded schema: %v", err)
			}
			t.Cleanup(func() {
				if err := repository.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, tenant := range upgradeOrderTenants {
				t.Run(string(tenant.organization)+" typed write", func(t *testing.T) {
					requireUpgradeOrderTypedWrite(t, db, repository, tenant.organization, tenant.weight)
				})
			}
		})
	}
}

func populatedHeadMigrations(t *testing.T) fstest.MapFS {
	t.Helper()
	source := os.DirFS("migrations")
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	head := fstest.MapFS{}
	for _, name := range files {
		version, err := strconv.ParseInt(name[:14], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(upgradeOrderPending, version) || version > upgradeOrderHead {
			continue
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			t.Fatal(err)
		}
		head[name] = &fstest.MapFile{Data: data}
	}
	if len(head) != 223 {
		t.Fatalf("populated head migrations = %d, want 223", len(head))
	}
	return head
}

func PopulatedHeadVersions(t *testing.T) []int64 {
	t.Helper()
	var versions []int64
	for name := range populatedHeadMigrations(t) {
		version, err := strconv.ParseInt(name[:14], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return versions
}

func PopulatedHeadPendingVersions() []int64 {
	return slices.Clone(upgradeOrderPending)
}

func PopulatedHeadTemplate(t *testing.T, seed func(context.Context, *sql.DB) error) *pgtest.Template {
	t.Helper()
	return migratedTemplate(t, populatedHeadMigrations(t), seed)
}

func seedUpgradeOrderTenants(ctx context.Context, db *sql.DB) error {
	for _, tenant := range upgradeOrderTenants {
		for _, statement := range []string{
			`INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,CURRENT_TIMESTAMP)`,
			`INSERT INTO projects(organization_id,id,name,slug,profile) SELECT $1,'prj_'||suffix,suffix,suffix,'native' FROM unnest(ARRAY['shared','other']) suffix`,
			`INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) SELECT $1,p.id,s.name,s.terminal,NOT s.terminal,s.rank FROM projects p CROSS JOIN (VALUES('Todo',false,0),('Done',true,1)) s(name,terminal,rank) WHERE p.organization_id=$1`,
			`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,native_source_key,workflow_state_name,archived)
			 SELECT $1,n,'wi_todo_'||n,'prj_shared',n,'Example','source_todo_'||n,'Todo',false FROM generate_series(1,$2::bigint+1) n
			 UNION ALL SELECT $1,11,'wi_done','prj_shared',11,'Example','source_done','Done',false
			 UNION ALL SELECT $1,12,'wi_archived','prj_shared',12,'Example','source_archived','Todo',true
			 UNION ALL SELECT $1,13,'wi_unset','prj_shared',13,'Example','source_unset',NULL,false
			 UNION ALL SELECT $1,14,'wi_other','prj_other',1,'Example','source_other','Todo',false`,
			`INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			 SELECT $1,project_id,native_id,native_id,1,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM issues WHERE organization_id=$1`,
			`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version)
			 SELECT $1,'prj_shared','wi_todo_1','decision_todo_'||n,n,'scheduler.decision',1,'human','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2 FROM generate_series(1,$2::bigint+1) n
			 UNION ALL SELECT $1,'prj_other','wi_other','decision_other_1',1,'scheduler.decision',1,'human','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2
			 UNION ALL SELECT $1,'prj_shared','wi_done','created_done',1,'issue.created',1,'human','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2`,
			`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) SELECT $1,'machine_shared','example',1,'example',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP`,
			`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) SELECT $1,'lease_shared',1,'machine_shared','session_shared',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP+interval '1 hour'`,
			`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,'run_shared',1)`,
			`INSERT INTO native_attempts(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,started_at,updated_at)
			 SELECT organization_id,'attempt_shared',issue_id,lease_id,fencing_token,'run_shared',1,'succeeded',work_item_revision,0,$1||'/retained_payload',$1||'/retained_checkpoint',acquired_at,renewed_at FROM leases WHERE organization_id=$1 AND lease_id='lease_shared'`,
			`INSERT INTO native_attempt_events(organization_id,attempt_id,sequence,request_hash) SELECT $1,'attempt_shared',n,repeat('c',64) FROM generate_series(1,$2::bigint) n`,
			`INSERT INTO native_command_states(organization_id,actor_id,operation,command_key,request_hash,created_at,response_expired) SELECT $1,'example-history','example.history',n::text,repeat('a',64),CURRENT_TIMESTAMP,true FROM generate_series(1,$2::bigint+1) n`,
			`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) SELECT $1,'continuation_owner','Continuation owner',$1,$1,'operator',true,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP`,
			`INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,event_seq) SELECT $1,c.id,'prj_shared','continuation_owner',c.origin,'shared','active',c.head FROM (VALUES('conversation_continue','worker',$2::bigint),('conversation_idle','worker',0),('conversation_user','user',0)) c(id,origin,head)`,
			`INSERT INTO conversation_commands(organization_id,conversation_id,key,kind,request_hash,receipt_json) SELECT $1,'conversation_continue','continue_original','continue','verified_request','{}'`,
			`INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery,attempt_id,command_key) SELECT $1,'message_continue','conversation_continue',1,'user','continue','delivered','original_attempt','continue_original'`,
			`INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json)
			 SELECT $1,'conversation_continue',1,'message.accepted','{"id":"message_continue","kind":"continue"}'::jsonb
			 UNION ALL SELECT $1,'conversation_continue',2,'execution.updated','{"status":"completed"}'::jsonb WHERE $2::bigint>1`,
		} {
			args := []any{tenant.organization}
			if strings.Contains(statement, "$2") {
				args = append(args, tenant.weight)
			}
			if _, err := db.ExecContext(ctx, statement, args...); err != nil {
				return fmt.Errorf("seed %s populated head: %w", tenant.organization, err)
			}
		}
	}
	return nil
}

func requireUpgradeOrderBackfills(t *testing.T, db *sql.DB, organization tracker.OrganizationID, weight int64, continuation bool) {
	t.Helper()
	text := func(query string, args ...any) string {
		t.Helper()
		var value sql.NullString
		if err := db.QueryRowContext(t.Context(), query, args...).Scan(&value); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return value.String
	}
	for _, check := range []struct{ name, query, want string }{
		{"active population", `SELECT value||' '||initialized FROM hosted_usage_counters WHERE organization_id=$1 AND name='unarchived_issues'`, fmt.Sprintf("%d true", weight+3)},
		{"history population", `SELECT value||' '||initialized FROM hosted_usage_counters WHERE organization_id=$1 AND name='history_records'`, fmt.Sprintf("%d true", (weight+5)+(weight+3)+weight)},
		{"archive accounting", `SELECT count(*)::text FROM archive_usage_counters WHERE organization_id=$1`, "1"},
		{"board populations", `SELECT string_agg(project_id||'/'||kind||'/'||has_state||'/'||state_name||'='||value,' ' ORDER BY project_id,kind,has_state,state_name) FROM local_board_populations WHERE organization_id=$1`,
			fmt.Sprintf("prj_other/project/false/=1 prj_other/state/true/Todo=1 prj_shared/project/false/=%d prj_shared/state/false/=1 prj_shared/state/true/Done=1 prj_shared/state/true/Todo=%d", weight+2, weight+1)},
		{"scheduler heads", `SELECT string_agg(project_id||'/'||work_item_id||'/'||head_kind||'='||id||'@'||sequence,' ' ORDER BY project_id,work_item_id,head_kind) FROM scheduler_event_heads WHERE organization_id=$1`,
			fmt.Sprintf("prj_other/wi_other/latest=decision_other_1@1 prj_other/wi_other/unprojected=decision_other_1@1 prj_shared/wi_todo_1/latest=decision_todo_%[1]d@%[1]d prj_shared/wi_todo_1/unprojected=decision_todo_%[1]d@%[1]d", weight+1)},
		{"event projections", `SELECT count(*)||' '||count(*) FILTER (WHERE NOT history_hidden AND scheduler_outcome='' AND NOT actor_envelope_known AND actor_via_key='' AND actor_display_name='' AND actor_agent='') FROM collaboration_events WHERE organization_id=$1`, fmt.Sprintf("%[1]d %[1]d", weight+3)},
		{"conversation continuation", `SELECT string_agg(id||'='||execution_continuation,' ' ORDER BY id) FROM conversations WHERE organization_id=$1`, fmt.Sprintf("conversation_continue=%t conversation_idle=false conversation_user=false", continuation)},
		{"command custody", `SELECT count(*)||' '||count(*) FILTER (WHERE reservation_project_id IS NULL AND reservation_correlation_id IS NULL AND reservation_claimed_at IS NULL) FROM native_command_states WHERE organization_id=$1`, fmt.Sprintf("%[1]d %[1]d", weight+1)},
		{"attempt facts", `SELECT count(*)||' '||count(*) FILTER (WHERE claim_disposition IS NULL AND local_attempt_id IS NULL AND NOT local_attempt_id_recorded) FROM native_attempts WHERE organization_id=$1`, "1 1"},
		{"doctor identities", `SELECT count(*)||' '||count(*) FILTER (WHERE doctor_identity_revision IS NULL) FROM issues WHERE organization_id=$1`, fmt.Sprintf("%[1]d %[1]d", weight+5)},
	} {
		if got := text(check.query, organization); got != check.want {
			t.Errorf("%s = %q, want %q", check.name, got, check.want)
		}
	}
	for _, check := range []struct{ name, statement, code string }{
		{"continuation required", `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,execution_continuation) VALUES($1,'conversation_unknown','prj_shared','continuation_owner','user','shared','active',NULL)`, "23502"},
		{"command custody", `UPDATE native_command_states SET reservation_project_id='prj_shared' WHERE organization_id=$1`, "23514"},
		{"claim disposition", `UPDATE native_attempts SET claim_disposition='unknown' WHERE organization_id=$1`, "23514"},
		{"local attempt identity", `UPDATE native_attempts SET local_attempt_id=1 WHERE organization_id=$1`, "23514"},
	} {
		_, err := db.ExecContext(t.Context(), check.statement, organization)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != check.code {
			t.Errorf("%s constraint error = %v, want SQLSTATE %s", check.name, err, check.code)
		}
	}
}

func requireUpgradeOrderTypedWrite(t *testing.T, db *sql.DB, repository *Repository, organization tracker.OrganizationID, weight int64) {
	t.Helper()
	state, actorKind, actorID := "Todo", "human", "actor_example"
	now := time.Now().UTC().Truncate(time.Microsecond)
	var sourceBefore int64
	if err := db.QueryRowContext(t.Context(), `SELECT duplicate_source_sequence FROM projects WHERE organization_id=$1 AND id='prj_shared'`, organization).Scan(&sourceBefore); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ID: 100, ProjectID: "prj_shared", NativeID: "wi_upgraded", Number: 100, Title: "Example", IssueType: "bug", WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actorID, CreatedAt: now})
		return err
	}); err != nil {
		t.Fatalf("typed write after upgrade: %v", err)
	}
	var read corestore.WorkItem
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		read, err = r.WorkItem(t.Context(), "prj_shared", "wi_upgraded")
		return err
	}); err != nil {
		t.Fatalf("typed read after upgrade: %v", err)
	}
	if read.OrganizationID != organization || read.Number != 100 || read.WorkflowStateName == nil || *read.WorkflowStateName != "Todo" || read.DoctorIdentityRevision != nil {
		t.Fatalf("typed read after upgrade = %+v", read)
	}
	var active, todo, sourceAfter int64
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='unarchived_issues'),
	 (SELECT value FROM local_board_populations WHERE organization_id=$1 AND project_id='prj_shared' AND kind='state' AND has_state AND state_name='Todo'),
	 (SELECT duplicate_source_sequence FROM projects WHERE organization_id=$1 AND id='prj_shared')`, organization).Scan(&active, &todo, &sourceAfter); err != nil {
		t.Fatal(err)
	}
	if active != weight+4 || todo != weight+2 || sourceAfter <= sourceBefore {
		t.Fatalf("write accounting after upgrade: active=%d todo=%d source=%d->%d", active, todo, sourceBefore, sourceAfter)
	}
}

func catalogSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
WITH n AS (SELECT oid,nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname<>'information_schema'),
acl(kind,owner,item) AS (
 SELECT 'relation',c.oid,a::text FROM pg_class c, unnest(c.relacl) a
 UNION ALL SELECT 'routine',p.oid,a::text FROM pg_proc p, unnest(p.proacl) a
 UNION ALL SELECT 'schema',s.oid,a::text FROM pg_namespace s, unnest(s.nspacl) a
),
grants AS (SELECT kind,owner,string_agg(item,',' ORDER BY item) AS items FROM acl GROUP BY kind,owner)
SELECT entry FROM (
 SELECT format('schema %s owner=%s acl=%s',n.nspname,pg_get_userbyid(s.nspowner),g.items) AS entry
  FROM n JOIN pg_namespace s ON s.oid=n.oid LEFT JOIN grants g ON g.kind='schema' AND g.owner=s.oid
 UNION ALL SELECT format('relation %s.%s kind=%s owner=%s rls=%s force=%s persistence=%s options=%s acl=%s view=%s',n.nspname,c.relname,c.relkind,pg_get_userbyid(c.relowner),c.relrowsecurity,c.relforcerowsecurity,c.relpersistence,c.reloptions,g.items,CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid) END)
  FROM pg_class c JOIN n ON n.oid=c.relnamespace LEFT JOIN grants g ON g.kind='relation' AND g.owner=c.oid WHERE c.relkind IN ('r','p','v','m','S','f')
 UNION ALL SELECT format('column %s.%s.%s position=%s type=%s notnull=%s default=%s identity=%s generated=%s collation=%s acl=%s',n.nspname,c.relname,a.attname,
   row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum),format_type(a.atttypid,a.atttypmod),a.attnotnull,pg_get_expr(d.adbin,d.adrelid),a.attidentity,a.attgenerated,
   (SELECT collname FROM pg_collation WHERE oid=a.attcollation),(SELECT string_agg(i::text,',' ORDER BY i::text) FROM unnest(a.attacl) i))
  FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN n ON n.oid=c.relnamespace LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
  WHERE a.attnum>0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m','f')
 UNION ALL SELECT format('constraint %s.%s.%s %s validated=%s',n.nspname,c.relname,o.conname,pg_get_constraintdef(o.oid),o.convalidated)
  FROM pg_constraint o JOIN pg_class c ON c.oid=o.conrelid JOIN n ON n.oid=c.relnamespace
 UNION ALL SELECT format('index %s.%s %s valid=%s ready=%s',n.nspname,i.relname,pg_get_indexdef(x.indexrelid),x.indisvalid,x.indisready)
  FROM pg_index x JOIN pg_class i ON i.oid=x.indexrelid JOIN n ON n.oid=i.relnamespace
 UNION ALL SELECT format('trigger %s.%s.%s %s enabled=%s',n.nspname,c.relname,t.tgname,pg_get_triggerdef(t.oid),t.tgenabled)
  FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN n ON n.oid=c.relnamespace WHERE NOT t.tgisinternal
 UNION ALL SELECT format('policy %s.%s.%s %s %s roles=%s using=%s check=%s',p.schemaname,p.tablename,p.policyname,p.permissive,p.cmd,(SELECT string_agg(r::text,',' ORDER BY r::text) FROM unnest(p.roles) r),p.qual,p.with_check)
  FROM pg_policies p JOIN n ON n.nspname=p.schemaname
 UNION ALL SELECT format('routine %s.%s(%s) owner=%s acl=%s %s',n.nspname,p.proname,pg_get_function_identity_arguments(p.oid),pg_get_userbyid(p.proowner),g.items,pg_get_functiondef(p.oid))
  FROM pg_proc p JOIN n ON n.oid=p.pronamespace LEFT JOIN grants g ON g.kind='routine' AND g.owner=p.oid WHERE p.prokind IN ('f','p')
 UNION ALL SELECT format('type %s.%s kind=%s base=%s labels=%s',n.nspname,y.typname,y.typtype,CASE WHEN y.typtype='d' THEN format_type(y.typbasetype,y.typtypmod) END,(SELECT string_agg(e.enumlabel,',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid=y.oid))
  FROM pg_type y JOIN n ON n.oid=y.typnamespace WHERE y.typtype IN ('e','d','r','m')
 UNION ALL SELECT format('sequence %s.%s type=%s start=%s min=%s max=%s increment=%s cycle=%s',q.schemaname,q.sequencename,q.data_type,q.start_value,q.min_value,q.max_value,q.increment_by,q.cycle)
  FROM pg_sequences q JOIN n ON n.nspname=q.schemaname
 UNION ALL SELECT format('rule %s.%s.%s %s',r.schemaname,r.tablename,r.rulename,r.definition) FROM pg_rules r JOIN n ON n.nspname=r.schemaname
 UNION ALL SELECT format('extension %s version=%s',extname,extversion) FROM pg_extension
 UNION ALL SELECT format('event trigger %s event=%s function=%s enabled=%s tags=%s',evtname,evtevent,evtfoid::regproc,evtenabled,evttags) FROM pg_event_trigger
 UNION ALL SELECT format('default acl role=%s schema=%s object=%s acl=%s',pg_get_userbyid(defaclrole),defaclnamespace::regnamespace,defaclobjtype,(SELECT string_agg(i::text,',' ORDER BY i::text) FROM unnest(defaclacl) i)) FROM pg_default_acl
) catalog ORDER BY entry COLLATE "C"`)
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("catalog snapshot is empty")
	}
	return entries
}

func catalogDifference(want, got []string) string {
	var difference strings.Builder
	report := func(label string, from, other []string) {
		shown := 0
		for _, entry := range from {
			if _, found := slices.BinarySearch(other, entry); found {
				continue
			}
			if shown++; shown > 20 {
				difference.WriteString(label + " ...\n")
				return
			}
			difference.WriteString(label + " " + entry + "\n")
		}
	}
	report("fresh only:", want, got)
	report("upgraded only:", got, want)
	return difference.String()
}
