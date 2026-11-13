package postgres_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/schemafingerprint"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func TestMain(m *testing.M) {
	pgstoretest.Main(m)
}

func TestCoreMigrationsFromEmpty(t *testing.T) {
	db, _ := pgstoretest.MigrationDatabaseAtVersion(t, 0)
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"from empty", "already applied"} {
		t.Run(step, func(t *testing.T) {
			if _, err := provider.Up(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows, err := db.QueryContext(t.Context(), `SELECT version_id FROM (SELECT version_id,is_applied,ROW_NUMBER() OVER(PARTITION BY version_id ORDER BY id DESC) AS state_rank FROM goose_db_version WHERE version_id>0) v WHERE state_rank=1 AND is_applied ORDER BY version_id`)
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
			fingerprint, err := postgres.LatestSchemaFingerprint()
			if err != nil {
				t.Fatal(err)
			}
			if fingerprint != schemafingerprint.Versions(applied) {
				t.Fatalf("declared PostgreSQL fingerprint omits applied migrations: %s", fingerprint)
			}
			for _, table := range []string{"projects", "issues", "native_comments", "collaboration_events", "collaboration_versions", "change_requests", "change_issue_links", "change_versions", "change_evidence", "native_commands"} {
				var exists bool
				if err := db.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
					t.Fatal(err)
				}
				if !exists {
					t.Errorf("table %s exists = %v after %s", table, exists, step)
				}
			}
		})
	}
	files, err := fs.Glob(os.DirFS("migrations"), "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migration files = %v, error = %v", files, err)
	}
	for _, file := range files {
		stamp, _, ok := strings.Cut(file, "_")
		if _, err := time.Parse("20060102150405", stamp); err != nil || !ok {
			t.Errorf("migration %s needs a UTC timestamp name", file)
		}
	}
}

func TestTargetOnlyBillingSchemaMatchesContracts(t *testing.T) {
	db := pgstoretest.Template.Database(t)
	catalog, err := storagecatalog.ReadTargetCatalog(os.DirFS("../../.."), "internal/storagecatalog/targets/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	classificationData, err := os.ReadFile("testdata/target_classification.json")
	if err != nil {
		t.Fatal(err)
	}
	var classification map[string]storagecatalog.TargetClassification
	if err := json.Unmarshal(classificationData, &classification); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateClassification(classification); err != nil {
		t.Fatal(err)
	}
	sourceData, err := os.ReadFile("../testdata/hub_classification.json")
	if err != nil {
		t.Fatal(err)
	}
	var sourceTables map[string]json.RawMessage
	if err := json.Unmarshal(sourceData, &sourceTables); err != nil {
		t.Fatal(err)
	}
	sourceInventory := make(map[string]json.RawMessage, len(sourceTables))
	for name, definition := range sourceTables {
		sourceInventory["hub/"+name] = definition
	}
	if err := catalog.ValidateMigrations(os.DirFS("../../.."), sourceInventory); err != nil {
		t.Fatal(err)
	}

	for migration := range catalog.LegacyMigrations {
		t.Run("digest/"+migration, func(t *testing.T) {
			changed := catalog
			changed.LegacyMigrations = maps.Clone(catalog.LegacyMigrations)
			changed.LegacyMigrations[migration] = strings.Repeat("0", 64)
			err := changed.ValidateMigrations(os.DirFS("../../.."), sourceInventory)
			if !errors.Is(err, storagecatalog.ErrCatalog) || !strings.Contains(err.Error(), "digest changed") {
				t.Fatalf("legacy digest was not checked: %v", err)
			}
		})
	}
	for table, contract := range catalog.Tables {
		table := table
		contract := contract
		t.Run(table, func(t *testing.T) {
			schema, name, _ := strings.Cut(table, ".")
			rows, err := db.QueryContext(t.Context(), `SELECT a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod) FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, schema, name)
			if err != nil {
				t.Fatal(err)
			}
			columns := make(map[string]string, len(contract.Columns))
			for rows.Next() {
				var column, kind string
				if err := rows.Scan(&column, &kind); err != nil {
					t.Fatal(err)
				}
				columns[column] = kind
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				t.Fatal(err)
			}
			for column, kind := range columns {
				if kind == "json" || kind == "jsonb" {
					t.Errorf("%s stores an untyped payload", column)
				}
			}
			for column, kind := range contract.Columns {
				if columns[column] != kind {
					t.Errorf("%s type=%q want %q", column, columns[column], kind)
				}
			}
			if contract.Organization == "organization-owned" {
				var rowSecurity, forced bool
				if err := db.QueryRowContext(t.Context(), `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=to_regclass($1)`, table).Scan(&rowSecurity, &forced); err != nil {
					t.Fatal(err)
				}
				if !rowSecurity || !forced {
					t.Errorf("row security enabled=%t forced=%t", rowSecurity, forced)
				}
				var tenantLeading bool
				if err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[0] WHERE i.indrelid=to_regclass($1) AND i.indisprimary AND a.attname='organization_id')`, table).Scan(&tenantLeading); err != nil {
					t.Fatal(err)
				}
				if !tenantLeading {
					t.Error("primary key is not tenant-leading")
				}
			}
		})
	}
}

func TestCoreIdentifiersAreTenantScoped(t *testing.T) {
	db := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, db)
	for _, org := range []string{"org_first", "org_second"} {
		t.Run(org, func(t *testing.T) {
			rows, err := db.QueryContext(t.Context(), `
				SELECT p.id, i.id, i.native_id, c.id, c.body_object_key
				FROM projects p JOIN issues i ON (i.organization_id,i.project_id)=(p.organization_id,p.id)
				JOIN native_comments c ON (c.organization_id,c.project_id,c.work_item_id)=(i.organization_id,i.project_id,i.native_id)
				WHERE p.organization_id=$1 ORDER BY i.id`, org)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for index, suffix := range []string{"shared", "other"} {
				var project, item, comment, body string
				var id int64
				if !rows.Next() {
					t.Fatalf("missing usable core row %s: %v", suffix, rows.Err())
				}
				if err := rows.Scan(&project, &id, &item, &comment, &body); err != nil {
					t.Fatal(err)
				}
				if project != "prj_"+suffix || id != int64(41+index) || item != "wi_"+suffix || comment != "comment_"+suffix || body != org+"/body/"+suffix {
					t.Errorf("identifiers changed: %s %d %s %s %s", project, id, item, comment, body)
				}
			}
			if rows.Next() {
				t.Error("tenant query returned extra rows")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCoreConstraintsRejectInvalidWrites(t *testing.T) {
	db := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, db)
	for _, tc := range []struct {
		name  string
		query string
		state string
	}{
		{"project requires tenant", `UPDATE projects SET organization_id=NULL WHERE organization_id='org_first'`, "23502"},
		{"item requires tenant", `UPDATE issues SET organization_id=NULL WHERE organization_id='org_first'`, "23514"},
		{"comment requires tenant", `UPDATE native_comments SET organization_id=NULL WHERE organization_id='org_first'`, "23502"},
		{"project primary key", `UPDATE projects SET id='prj_shared' WHERE organization_id='org_first' AND id='prj_other'`, "23505"},
		{"project name", `UPDATE projects SET name='shared' WHERE organization_id='org_first' AND id='prj_other'`, "23505"},
		{"project slug", `UPDATE projects SET slug='shared' WHERE organization_id='org_first' AND id='prj_other'`, "23505"},
		{"item primary key", `UPDATE issues SET id=41 WHERE organization_id='org_first' AND id=42`, "23514"},
		{"item creation time immutable", `UPDATE issues SET created_at=created_at+INTERVAL '1 second' WHERE organization_id='org_first' AND id=42`, "23514"},
		{"comment record identity immutable", `UPDATE native_comments SET record_id=record_id+100 WHERE organization_id='org_first' AND id='comment_other'`, "23514"},
		{"comment creation time immutable", `UPDATE native_comments SET created_at=created_at+INTERVAL '1 second' WHERE organization_id='org_first' AND id='comment_other'`, "23514"},
		{"native identifier", `UPDATE issues SET native_id='wi_shared' WHERE organization_id='org_first' AND id=42`, "23514"},
		{"project item number", `INSERT INTO issues (organization_id,native_id,project_id,number,title) VALUES ('org_first','wi_duplicate','prj_shared',1,'Example')`, "23505"},
		{"project source key", `INSERT INTO issues (organization_id,native_id,project_id,number,title,native_source_key) VALUES ('org_first','wi_duplicate','prj_shared',2,'Example','source_shared')`, "23505"},
		{"comment primary key", `UPDATE native_comments SET id='comment_shared' WHERE organization_id='org_first' AND id='comment_other'`, "23505"},
		{"comment sequence", `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id) VALUES ('org_first','comment_new','prj_shared','wi_shared',1,'example/body','human','example')`, "23505"},
		{"comment source key", `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id,source_key) VALUES ('org_first','comment_new','prj_shared','wi_shared',2,'example/body','human','example','source_shared')`, "23505"},
		{"comment project matches item", `UPDATE native_comments SET project_id='prj_other' WHERE organization_id='org_first' AND id='comment_shared'`, "23503"},
		{"item number positive", `UPDATE issues SET number=0 WHERE organization_id='org_first'`, "23514"},
		{"item revision positive", `UPDATE issues SET revision=0 WHERE organization_id='org_first'`, "23514"},
		{"comment revision positive", `UPDATE native_comments SET revision=0 WHERE organization_id='org_first'`, "23514"},
		{"comment sequence positive", `UPDATE native_comments SET sequence=0 WHERE organization_id='org_first'`, "23514"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), tc.query)
			pgstoretest.RequireSQLState(t, err, tc.state)
		})
	}
}

func TestCoreGeneratedIdentifiersAndOptionalKeys(t *testing.T) {
	db := pgstoretest.Template.Database(t)
	for _, org := range []string{"org_first", "org_second"} {
		t.Run(org, func(t *testing.T) {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,CURRENT_TIMESTAMP)`, org); err != nil {
				t.Fatal(err)
			}
			var project, item string
			var previousProject, previousItem string
			var previousID int64
			for _, number := range []int{1, 2} {
				if err := db.QueryRowContext(t.Context(), `INSERT INTO projects (organization_id,name,profile) VALUES ($1,$2,'native') RETURNING id`, org, fmt.Sprintf("Example %d", number)).Scan(&project); err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(project, "prj_") || project == previousProject {
					t.Fatalf("generated project identifier = %q, previous = %q", project, previousProject)
				}
				if number == 1 {
					previousProject = project
				}
				var id int64
				if err := db.QueryRowContext(t.Context(), `INSERT INTO issues (organization_id,project_id,number,title) VALUES ($1,$2,$3,'Example') RETURNING id,native_id`, org, previousProject, number).Scan(&id, &item); err != nil {
					t.Fatal(err)
				}
				if id <= 0 || id == previousID || !strings.HasPrefix(item, "wi_") || item == previousItem {
					t.Fatalf("generated work item identifiers = %d %q, previous = %d %q", id, item, previousID, previousItem)
				}
				if number == 1 {
					previousID, previousItem = id, item
				}
				var comment string
				want := fmt.Sprintf("comment_%d", number)
				if err := db.QueryRowContext(t.Context(), `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id) VALUES ($1,$2,$3,$4,$5,'example/body','human','example') RETURNING id`, org, want, previousProject, previousItem, number).Scan(&comment); err != nil {
					t.Fatal(err)
				}
				if comment != want {
					t.Errorf("comment identifier = %q, want %q", comment, want)
				}
			}
		})
	}
}

func TestCoreForeignKeysDenyOtherTenants(t *testing.T) {
	db := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, db)
	for _, query := range []string{
		`INSERT INTO projects (organization_id,id,name,profile) VALUES ('org_second','prj_private','Private','native')`,
		`INSERT INTO issues (organization_id,native_id,project_id,number,title) VALUES ('org_second','wi_private','prj_private',1,'Private')`,
		`INSERT INTO issues (organization_id,native_id,project_id,number,title) VALUES ('org_second','wi_private_shared','prj_shared',2,'Private')`,
	} {
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, query, state string }{
		{"item insert", `INSERT INTO issues (organization_id,native_id,project_id,number,title) VALUES ('org_first','wi_invalid','prj_private',2,'Example')`, "23503"},
		{"item update", `UPDATE issues SET project_id='prj_private' WHERE organization_id='org_first' AND native_id='wi_other'`, "23514"},
		{"comment insert", `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id) VALUES ('org_first','comment_invalid','prj_private','wi_private',1,'example/body','human','example')`, "23503"},
		{"comment update", `UPDATE native_comments SET project_id='prj_private',work_item_id='wi_private' WHERE organization_id='org_first' AND id='comment_shared'`, "23503"},
		{"comment on shared project", `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id) VALUES ('org_first','comment_invalid','prj_shared','wi_private_shared',1,'example/body','human','example')`, "23503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), tc.query)
			pgstoretest.RequireSQLState(t, err, tc.state)
		})
	}
}

func TestCoreSchemaUsesTenantLeadingTypedColumns(t *testing.T) {
	db := pgstoretest.Template.Database(t)
	for _, tc := range []struct{ name, query string }{
		{"tenant required", `SELECT table_name FROM information_schema.columns WHERE table_schema='public' AND table_name IN ('projects','issues','native_comments','collaboration_events','collaboration_versions','change_requests','change_issue_links','change_versions','change_evidence','native_commands') AND column_name='organization_id' AND (is_nullable<>'NO' OR data_type<>'text')`},
		{"typed columns", `SELECT table_name || '.' || column_name FROM information_schema.columns WHERE table_schema='public' AND table_name IN ('projects','issues','native_comments','collaboration_events','collaboration_versions','change_requests','change_issue_links','change_versions','change_evidence','native_commands') AND (data_type NOT IN ('text','smallint','bigint','boolean','timestamp with time zone') OR column_name LIKE '%\_json' ESCAPE '\' OR column_name IN ('body','workflow_markdown'))`},
		{"tenant leading indexes", `SELECT indexrelid::regclass::text FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=i.indkey[0] WHERE c.relname IN ('projects','issues','native_comments','collaboration_events','collaboration_versions','change_requests','change_issue_links','change_versions','change_evidence','native_commands') AND a.attname<>'organization_id'`},
		{"tenant foreign keys", `SELECT conname FROM pg_constraint WHERE conrelid IN ('projects'::regclass,'issues'::regclass,'native_comments'::regclass,'collaboration_events'::regclass,'collaboration_versions'::regclass,'change_requests'::regclass,'change_issue_links'::regclass,'change_versions'::regclass,'change_evidence'::regclass,'native_commands'::regclass) AND contype='f' AND (array_length(conkey,1)<2 OR (SELECT attname FROM pg_attribute WHERE attrelid=conrelid AND attnum=conkey[1])<>'organization_id' OR (SELECT attname FROM pg_attribute WHERE attrelid=confrelid AND attnum=confkey[1])<>'organization_id')`},
		{"typed timestamps", `SELECT table_name || '.' || column_name FROM information_schema.columns WHERE table_schema='public' AND table_name IN ('projects','issues','native_comments','collaboration_events','collaboration_versions','change_requests','change_issue_links','change_versions','change_evidence','native_commands') AND column_name LIKE '%\_at' ESCAPE '\' AND data_type<>'timestamp with time zone'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(t.Context(), tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var violation string
				if err := rows.Scan(&violation); err != nil {
					t.Fatal(err)
				}
				t.Error(violation)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
