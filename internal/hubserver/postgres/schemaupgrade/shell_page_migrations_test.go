package schemaupgrade_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestShellPageMigrationsResumeInterruptedDDL(t *testing.T) {
	t.Parallel()
	_, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261107062507)
	for _, tc := range []struct {
		file    string
		indexes []string
		usage   bool
	}{
		{"20261107062508_health_page_evidence.sql", []string{"health_page_pending"}, false},
		{"20261107062509_health_project_page_order.sql", []string{"health_project_page_order", "health_project_page_cold"}, false},
		{"20261107062510_project_page_read_model.sql", []string{"project_page_active"}, false},
		{"20261112032820_usage_cumulative_indexes.sql", []string{"attempt_usage_unclassified", "attempt_usage_currency_key", "attempt_usage_known_total", "attempt_usage_known_currency_total", "native_attempt_usage_pending", "attempt_usage_origin"}, true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			var db *sql.DB
			owner := fixture
			if tc.usage {
				_, older := pgstoretest.RuntimeMigrationDatabaseAtVersion(t, 20261112031629)
				db = older.Database(t)
				owner = older
			} else {
				db = fixture.Database(t)
			}
			pgstoretest.SeedCore(t, db)
			var usageRepository *postgres.Repository
			if tc.usage {
				pool := pgstoretest.RolePoolFromTemplate(t, owner, db, "detent_runtime_v2", "")
				var err error
				usageRepository, err = postgres.NewRepository(t.Context(), pool, pgstoretest.SmallPool())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := usageRepository.Close(); err != nil {
						t.Error(err)
					}
				})
				for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
					for _, statement := range []string{
						`INSERT INTO projects(organization_id,id,name,slug,profile,created_at) SELECT $1,'prj_'||n,'example-'||n,'example-'||n,'native',$2 FROM generate_series(1,2)n`,
						`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,created_at,updated_at) SELECT $1,n,'wi_'||n,'prj_'||n,1,'Example',$2,$2 FROM generate_series(1,2)n`,
						`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,'machine_1','example',1,'example',$2,$2,$2)`,
					} {
						if _, err := db.ExecContext(t.Context(), statement, org, pgstoretest.HistoryAt()); err != nil {
							t.Fatal(err)
						}
					}
					var fence int64
					if err := db.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,project_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) VALUES($1,'lease_1',1,'prj_1','machine_1','session_1',1,$2,$2,$2::timestamptz+interval '1 hour') RETURNING fencing_token`, org, pgstoretest.HistoryAt()).Scan(&fence); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,'usage_upgrade_run',1)`, org); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at) VALUES($1,'prj_1','usage_upgrade',1,$2,$3,'usage_upgrade_run',1,'running',1,1,'fixture/upgrade',$4,$4)`, org, "lease_1", fence, pgstoretest.HistoryAt()); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,'prj_1','usage_upgrade',$2::timestamptz+n*interval '1 hour','example','legacy-model',1,0,0,1,'usd',$2 FROM generate_series(1,101)n`, org, pgstoretest.HistoryAt()); err != nil {
						t.Fatal(err)
					}
				}
			}
			db.SetMaxOpenConns(1)
			if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile("../migrations/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			apply := func(source []byte) error {
				p, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(fstest.MapFS{tc.file: &fstest.MapFile{Data: source}}), goose.WithDisableVersioning(true), goose.WithDisableGlobalRegistry(true))
				if err != nil {
					return err
				}
				_, err = p.Up(t.Context())
				return err
			}
			if tc.usage {
				schema, err := os.ReadFile("../migrations/20261112031630_attempt_usage_cumulative_totals.sql")
				if err != nil {
					t.Fatal(err)
				}
				cut := strings.Index(string(schema), "ALTER TABLE native_attempts")
				if cut < 0 {
					t.Fatal("missing attempt schema statement")
				}
				if err := apply([]byte(string(schema[:cut]) + "SELECT 1/0;\n")); err == nil || !strings.Contains(err.Error(), "division by zero") {
					t.Fatalf("expected schema interruption: %v", err)
				}
				for range 2 {
					if err := apply(schema); err != nil {
						t.Fatal(err)
					}
				}
			}
			cut := strings.Index(string(body), "CREATE INDEX CONCURRENTLY")
			if cut < 0 {
				t.Fatal("missing concurrent index")
			}
			interrupted := string(body[:cut]) + "SELECT 1/0;\n"
			if err := apply([]byte(interrupted)); err == nil || !strings.Contains(err.Error(), "division by zero") {
				t.Fatalf("expected injected interruption: %v", err)
			}
			if err := apply(body); err != nil {
				t.Fatalf("resume committed DDL: %v", err)
			}
			if _, err := db.ExecContext(t.Context(), `CREATE TABLE failed_page_index(value integer); INSERT INTO failed_page_index VALUES(1),(1)`); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.indexes {
				if _, err := db.ExecContext(t.Context(), "DROP INDEX CONCURRENTLY "+name); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "CREATE UNIQUE INDEX CONCURRENTLY "+name+" ON failed_page_index(value)"); err == nil {
					t.Fatal("expected invalid concurrent index")
				}
				var invalid bool
				if err := db.QueryRowContext(t.Context(), `SELECT NOT indisvalid FROM pg_index WHERE indexrelid=to_regclass($1)`, name).Scan(&invalid); err != nil || !invalid {
					t.Fatalf("interrupted index %s invalid=%v error=%v", name, invalid, err)
				}
			}
			for range 2 {
				if err := apply(body); err != nil {
					t.Fatalf("resume incomplete index: %v", err)
				}
				for _, name := range tc.indexes {
					var valid bool
					if err := db.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready AND indrelid<>'failed_page_index'::regclass FROM pg_index WHERE indexrelid=to_regclass($1)`, name).Scan(&valid); err != nil || !valid {
						t.Fatalf("index %s valid=%v error=%v", name, valid, err)
					}
				}
			}
			if tc.usage {
				for _, name := range []string{"20261112032821_usage_cumulative_primary_key.sql", "20261112032822_usage_cumulative_validate.sql"} {
					data, err := os.ReadFile("../migrations/" + name)
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if err := apply(data); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
					t.Fatal(err)
				}
				for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
					var count, unknown int
					var input, cost int64
					if err := db.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE usage_input_total IS NULL),sum(input),sum(cost_estimate)::bigint FROM attempt_usage WHERE organization_id=$1 AND currency='usd' AND usage_origin='import'`, org).Scan(&count, &unknown, &input, &cost); err != nil || count != 101 || unknown != 101 || input != 101 || cost != 101 {
						t.Fatalf("populated upgrade scope=%s rows=%d unknown=%d totals=%d/%d error=%v", org, count, unknown, input, cost, err)
					}
					for _, want := range []int{100, 1, 0} {
						if after, count, err := usageRepository.RefreshReportUsagePage(t.Context(), org, pgstoretest.NewHistoryObjects(), "", 100); err != nil || count != want || after != "" {
							t.Fatalf("bounded upgrade scope=%s count=%d want=%d after=%q error=%v", org, count, want, after, err)
						}
					}
					if err := db.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE usage_input_total IS NULL),sum(input),sum(cost_estimate)::bigint FROM attempt_usage WHERE organization_id=$1 AND currency='USD' AND usage_origin='import'`, org).Scan(&count, &unknown, &input, &cost); err != nil || count != 101 || unknown != 0 || input != 101 || cost != 101 {
						t.Fatalf("upgraded usage scope=%s rows=%d unknown=%d totals=%d/%d error=%v", org, count, unknown, input, cost, err)
					}
				}
			}
			if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
				t.Fatal(err)
			}
			for _, org := range []string{"org_first", "org_second"} {
				want := 2
				if tc.usage {
					want = 4
				}
				var count int
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1`, org).Scan(&count); err != nil || count != want {
					t.Fatalf("tenant %s count=%d error=%v", org, count, err)
				}
			}
		})
	}
}
