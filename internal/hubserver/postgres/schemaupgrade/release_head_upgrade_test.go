package schemaupgrade_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

const releaseHead int64 = 20261107232145

var releaseHeadPending = []int64{
	20261107020944,
	20261107023038,
	20261107051229,
	20261107060339,
	20261107060935,
	20261107062508,
	20261107062509,
	20261107062510,
	20261107073742,
	20261107133116,
	20261107160930,
	20261107170921,
	20261107173004,
	20261107212206,
}

func seedReleaseHeadTenants(ctx context.Context, db *sql.DB) error {
	for _, organization := range []string{"org_first", "org_second"} {
		for _, statement := range []string{
			`INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,true,CURRENT_TIMESTAMP)`,
			`INSERT INTO projects(organization_id,id,name,slug,profile) SELECT $1,'prj_'||suffix,suffix,suffix,'native' FROM unnest(ARRAY['shared','other']) suffix`,
			`INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) SELECT $1,p.id,s.name,s.terminal,NOT s.terminal,s.rank FROM projects p CROSS JOIN (VALUES('Todo',false,0),('Done',true,1)) s(name,terminal,rank) WHERE p.organization_id=$1`,
			`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,native_source_key,workflow_state_name,archived)
			 SELECT $1,n,'wi_'||n,CASE WHEN n=3 THEN 'prj_other' ELSE 'prj_shared' END,n,'Example','source_'||n,'Todo',false FROM generate_series(1,3) n`,
			`INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			 SELECT $1,project_id,native_id,native_id,1,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM issues WHERE organization_id=$1`,
			`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version)
			 SELECT $1,project_id,native_id,'decision_'||native_id,1,'scheduler.decision',1,'human','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2 FROM issues WHERE organization_id=$1 AND id IN (1,3)`,
			`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'continuation_owner','Continuation owner',$1,$1,'operator',true,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
			`INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,event_seq,execution_status)
			 VALUES($1,'conversation_compacted','prj_shared','continuation_owner','worker','shared','settled',6,'completed'),($1,'conversation_owned','prj_shared','continuation_owner','worker','shared','active',4,'interrupted')`,
			`INSERT INTO conversation_commands(organization_id,conversation_id,key,kind,request_hash,receipt_json) SELECT $1,id,'continue_original','continue','verified_request','{}' FROM conversations WHERE organization_id=$1`,
			`INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery,command_key) SELECT $1,'message_'||id,id,1,'user','continue','delivered','continue_original' FROM conversations WHERE organization_id=$1`,
			`INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json) VALUES
			 ($1,'conversation_compacted',1,'execution.updated','{"status":"running"}'),
			 ($1,'conversation_compacted',3,'message.accepted','{"id":"message_conversation_compacted","kind":"continue"}'),
			 ($1,'conversation_compacted',5,'execution.updated','{"status":"completed"}'),
			 ($1,'conversation_owned',1,'execution.updated','{"status":"completed"}'),
			 ($1,'conversation_owned',3,'message.accepted','{"id":"message_conversation_owned","kind":"continue"}'),
			 ($1,'conversation_owned',4,'execution.updated','{"status":"interrupted"}')`,
		} {
			if _, err := db.ExecContext(ctx, statement, organization); err != nil {
				return fmt.Errorf("seed %s release head: %w", organization, err)
			}
		}
	}
	return nil
}

func TestMigrateFromReleaseHeadBackfillsTenantRows(t *testing.T) {
	t.Parallel()
	migrations := hubMigrationFiles(t)
	head := fstest.MapFS{}
	for name, file := range migrations {
		version, err := strconv.ParseInt(name[:14], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if version <= releaseHead && !slices.Contains(releaseHeadPending, version) {
			head[name] = file
		}
	}
	if len(head) != 179 {
		t.Fatalf("release head migrations = %d, want 179", len(head))
	}
	fixture := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		if _, err := db.ExecContext(ctx, postgres.RolesSQL()); err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "SET ROLE "+postgres.MigrationRoleName); err != nil {
			return err
		}
		provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(head), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
		if err != nil {
			return err
		}
		if _, err := provider.Up(ctx); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "RESET ROLE"); err != nil {
			return err
		}
		return seedReleaseHeadTenants(ctx, db)
	})
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := fixture.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	db := fixture.Database(t)
	if err := postgres.Migrate(t.Context(), roleConnection(t, fixture, db, postgres.MigrationRoleName), migrations); err != nil {
		t.Fatalf("migrate populated release head as the restricted migration login: %v", err)
	}
	for _, organization := range []string{"org_first", "org_second"} {
		var identities, heads int
		if err := db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM work_item_identities w JOIN issues i USING(organization_id,id,project_id,native_id) WHERE w.organization_id=$1),
			(SELECT count(DISTINCT id) FROM scheduler_event_heads WHERE organization_id=$1)`, organization).Scan(&identities, &heads); err != nil {
			t.Fatal(err)
		}
		if identities != 3 || heads != 2 {
			t.Errorf("%s retained identities=%d scheduler heads=%d, want 3 and 2", organization, identities, heads)
		}
		for conversation, want := range map[string]bool{"conversation_compacted": false, "conversation_owned": true} {
			var got bool
			if err := db.QueryRowContext(t.Context(), `SELECT execution_continuation FROM conversations WHERE organization_id=$1 AND id=$2`, organization, conversation).Scan(&got); err != nil || got != want {
				t.Errorf("%s %s continuation=%t want=%t error=%v", organization, conversation, got, want, err)
			}
		}
	}
	var local, policies int
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM organizations WHERE local),
		(SELECT count(*) FROM pg_policies WHERE policyname IN ('work_item_identity_backfill','designated_local_organization_repair'))`).Scan(&local, &policies); err != nil {
		t.Fatal(err)
	}
	if local != 0 || policies != 0 {
		t.Errorf("local organizations=%d temporary policies=%d, want 0 and 0", local, policies)
	}
}
