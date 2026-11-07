package postgres_test

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestShellPageMigrationsResumeInterruptedDDL(t *testing.T) {
	_, fixture := migrationDatabaseAtVersion(t, 20261107004848)
	for _, tc := range []struct {
		file    string
		indexes []string
	}{
		{"20261107010000_health_page_evidence.sql", []string{"health_page_pending"}},
		{"20261107010001_health_project_page_order.sql", []string{"health_project_page_order", "health_project_page_cold"}},
		{"20261107051906_project_page_read_model.sql", []string{"project_page_active"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			db := fixture.Database(t)
			seedCore(t, db)
			db.SetMaxOpenConns(1)
			if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile("migrations/" + tc.file)
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
			if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
				t.Fatal(err)
			}
			for _, org := range []string{"org_first", "org_second"} {
				var count int
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 2 {
					t.Fatalf("tenant %s count=%d error=%v", org, count, err)
				}
			}
		})
	}
}
