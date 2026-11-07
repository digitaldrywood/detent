package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchBackfillRejectsChangedSources(t *testing.T) {
	f := newSearchFixture(t)
	runtime, err := postgres.NewRuntime(t.Context(), f.pool, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, mode := range []string{"revision", "generation", "native publication", "deletion"} {
			t.Run(string(org)+"/"+mode, func(t *testing.T) {
				if _, err := f.admin.ExecContext(t.Context(), `DELETE FROM search_documents WHERE organization_id=$1`, org); err != nil {
					t.Fatal(err)
				}
				f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "sentinel"}, 0)
				f.drain(t, org)
				tx, err := runtime.Begin(t.Context(), org, pgtransport.Options{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				rows, err := pgquery.New(tx).SearchBackfillPage(t.Context(), pgquery.SearchBackfillPageParams{OrganizationID: org, PageLimit: 1})
				if closeErr := tx.Rollback(t.Context()); closeErr != nil {
					t.Fatal(closeErr)
				}
				if err != nil || len(rows) != 1 {
					t.Fatalf("candidates=%d err=%v", len(rows), err)
				}
				mutation := `UPDATE search_documents SET generation=generation+1 WHERE organization_id=$1`
				switch mode {
				case "revision":
					mutation = `UPDATE search_documents SET revision=revision+1,indexed_revision=revision+1 WHERE organization_id=$1`
				case "native publication":
					mutation = `UPDATE search_documents SET search_format=1 WHERE organization_id=$1`
				case "deletion":
					mutation = `DELETE FROM search_documents WHERE organization_id=$1`
				}
				if _, err := f.admin.ExecContext(t.Context(), mutation, org); err != nil {
					t.Fatal(err)
				}
				row := rows[0]
				tx, err = runtime.Begin(t.Context(), org, pgtransport.Options{})
				if err != nil {
					t.Fatal(err)
				}
				err = pgquery.New(tx).SearchBackfillQueue(t.Context(), pgquery.SearchBackfillQueueParams{OrganizationID: org, ID: row.ID, Revision: row.Revision, Generation: row.Generation, PayloadSHA256: row.PayloadSHA256, AfterID: row.AfterID})
				if err != nil {
					t.Fatal(errors.Join(err, tx.Rollback(t.Context())))
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				var pending int
				if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents WHERE organization_id=$1 AND indexed_revision<>revision`, org).Scan(&pending); err != nil || pending != 0 {
					t.Fatalf("stale candidate invalidated current content: pending=%d err=%v", pending, err)
				}
				if _, err := f.admin.ExecContext(t.Context(), `DELETE FROM storage_conversion_cursors WHERE organization_id=$1 AND family='search'`, org); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSearchNativeIndexMigrationResumesAfterInterruption(t *testing.T) {
	db, _ := migrationDatabaseAtVersion(t, 20261107001658)
	if _, err := db.ExecContext(t.Context(), `RESET ROLE;
 CREATE INDEX search_document_index_words_gin ON search_document_index USING gin(word_vector);
 UPDATE pg_index SET indisvalid=false WHERE indexrelid='search_document_index_words_gin'::regclass;
 SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"search_document_index_words_gin", "search_document_index_text_gin", "search_documents_native_backfill_idx"} {
		var valid bool
		if err := db.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid=$1::regclass`, name).Scan(&valid); err != nil || !valid {
			t.Fatalf("index=%s valid=%t err=%v", name, valid, err)
		}
	}
}

func TestSearchNativeSchemaRollbackRemovesBackfillCursors(t *testing.T) {
	db := template.Database(t)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO storage_conversion_cursors(organization_id,family) VALUES('org_first','search'),('org_second','search'); SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 20261106233256); err != nil {
		t.Fatal(err)
	}
	var enabled, forced bool
	if err := db.QueryRowContext(t.Context(), `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid='storage_conversion_cursors'::regclass`).Scan(&enabled, &forced); err != nil || !enabled || !forced {
		t.Fatalf("RLS after rollback: enabled=%t forced=%t err=%v", enabled, forced, err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
}
