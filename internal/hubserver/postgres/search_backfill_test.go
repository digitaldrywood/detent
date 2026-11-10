package postgres_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchNativeRepresentationIsolation(t *testing.T) {
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, item := range []string{"wi_first_1", "wi_first_2"} {
			d := f.put(t, searchIssue(org, item), searchstore.Text{Title: "sentinel"}, 0)
			if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_document_index
 (organization_id,document_id,generation,format,content_sha256,word_vector,search_text,segment_ends)
 SELECT organization_id,id,generation,1,payload_sha256,'sentinel'::tsvector,'sentinel',ARRAY[8]
 FROM search_documents WHERE organization_id=$1 AND id=$2`, org, d.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	runtime, err := postgres.NewRuntime(t.Context(), f.pool, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"", "unknown", "org_first", "org_second"} {
		var count int
		if org == "" {
			err = f.pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM search_document_index`).Scan(&count)
		} else {
			tx, beginErr := runtime.Begin(t.Context(), org, pgtransport.Options{ReadOnly: true})
			if beginErr != nil {
				t.Fatal(beginErr)
			}
			err = tx.QueryRow(t.Context(), `SELECT count(*) FROM search_document_index`).Scan(&count)
			if closeErr := tx.Rollback(t.Context()); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		want := 0
		if org == "org_first" || org == "org_second" {
			want = 2
		}
		if err != nil || count != want {
			t.Fatalf("scope %q: count=%d want=%d err=%v", org, count, want, err)
		}
	}
	for _, index := range []string{"search_document_index_words_gin", "search_document_index_text_gin"} {
		var valid bool
		if err := f.admin.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid=$1::regclass`, index).Scan(&valid); err != nil || !valid {
			t.Fatalf("index %s: valid=%t err=%v", index, valid, err)
		}
	}
	var platformAccess, publicAccess bool
	if err := f.admin.QueryRowContext(t.Context(), `SELECT has_table_privilege('detent_platform_v2','search_document_index','SELECT'),
 EXISTS(SELECT 1 FROM pg_class c, LATERAL aclexplode(c.relacl) a WHERE c.oid='search_document_index'::regclass AND a.grantee=0)`).Scan(&platformAccess, &publicAccess); err != nil || platformAccess || publicAccess {
		t.Fatalf("unexpected search ACL: platform=%t public=%t err=%v", platformAccess, publicAccess, err)
	}
	for _, test := range []struct{ name, set string }{
		{"oversize", "search_text='" + strings.Repeat("x", 65537) + "'"},
		{"not folded", "search_text='SENTINEL'"},
		{"bad boundary", "segment_ends=ARRAY[9]"},
		{"unordered boundaries", "segment_ends=ARRAY[8,1,8]"},
		{"foreign parent", "document_id='missing'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_document_index SET `+test.set+` WHERE organization_id='org_first'`); err == nil {
				t.Fatal("invalid representation accepted")
			}
		})
	}
	tx, err := runtime.Begin(t.Context(), "org_first", pgtransport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(t.Context(), `UPDATE search_document_index SET organization_id='org_second' WHERE document_id='issue/wi_first_1'`)
	if closeErr := tx.Rollback(t.Context()); closeErr != nil {
		t.Fatal(closeErr)
	}
	if pgtransport.SQLState(err) != "42501" {
		t.Fatalf("foreign tenant write: %v", err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `DELETE FROM search_documents WHERE organization_id='org_first' AND id='issue/wi_first_1'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_document_index`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("cascade damaged another source: count=%d err=%v", count, err)
	}
}

func TestSearchBackfillResumesBoundedTenantPages(t *testing.T) {
	t.Run("populated conversion family upgrade", func(t *testing.T) {
		db, fixture := migrationDatabaseAtVersion(t, 20261109184650)
		seedCore(t, db)
		families := []string{"runs", "versions", "evidence", "core-work-events"}
		for _, org := range []string{"org_first", "org_second"} {
			for _, family := range families {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO storage_conversion_cursors(organization_id,family,turn,positioned,key_1,key_2,key_3,key_4,sequence,examined_rows,examined_bytes,oversized_rows) VALUES($1,$2,7,true,$1,'project','aggregate','record',17,3,128,1)`, org, family); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,converted_rows,converted_bytes) VALUES($1,'prj_shared',$2,'41',2,64)`, org, family); err != nil {
					t.Fatal(err)
				}
			}
		}
		snapshot := func() []string {
			t.Helper()
			var result []string
			for _, table := range []string{"storage_conversion_cursors", "storage_conversion_progress"} {
				rows, err := db.QueryContext(t.Context(), `SELECT * FROM `+table+` ORDER BY organization_id,family`)
				if err != nil {
					t.Fatal(err)
				}
				columns, err := rows.Columns()
				if err != nil {
					rows.Close()
					t.Fatal(err)
				}
				for rows.Next() {
					values := make([]any, len(columns))
					pointers := make([]any, len(columns))
					for i := range values {
						pointers[i] = &values[i]
					}
					if err := rows.Scan(pointers...); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					result = append(result, table+fmt.Sprint(values))
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					t.Fatal(err)
				}
			}
			return result
		}
		constraint := func() string {
			t.Helper()
			var value string
			if err := db.QueryRowContext(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='storage_conversion_progress'::regclass AND conname='storage_conversion_progress_family_check'`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		before, progressConstraint := snapshot(), constraint()
		provider, err := migrationProvider(db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		if after := snapshot(); !reflect.DeepEqual(before, after) {
			t.Fatal("family repair changed populated cursor or progress rows")
		}
		if constraint() != progressConstraint {
			t.Fatal("cursor repair changed progress family contract")
		}
		repository, pool := coreWorkEventRepository(t, db, fixture)
		orgs := []tracker.OrganizationID{"org_first", "org_second"}
		objects := newHistoryObjects()
		search := searchFixture{repository: repository, content: searchstore.NewContent(objects), objects: objects, admin: db, pool: pool}
		for _, org := range orgs {
			document := searchIssue(org, "wi_shared")
			document.ProjectID = "prj_shared"
			search.put(t, document, searchstore.Text{Title: "sentinel"}, 0)
			search.drain(t, org)
		}
		for index, org := range orgs {
			owner := postgres.NewConversionStorage(repository, org, newHistoryObjects(), convertPayload)
			if _, err := owner.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := owner.Update(t.Context(), storageconversion.FamilyCursor{Family: "core-work-events"}, storageconversion.Reserve, storageconversion.Usage{}); err != nil {
				t.Fatal(err)
			}
			if err := owner.Update(t.Context(), storageconversion.FamilyCursor{Family: "core-work-events"}, storageconversion.Record, storageconversion.Usage{Rows: 1, Bytes: 16}); err != nil {
				t.Fatal(err)
			}
			if n, err := repository.QueueSearchBackfill(t.Context(), org, 1); err != nil || n != 1 {
				t.Fatalf("search cursor operation: n=%d error=%v", n, err)
			}
			for otherIndex, other := range orgs {
				var turn, examined, bytes int
				var search int
				if err := db.QueryRowContext(t.Context(), `SELECT turn,examined_rows,examined_bytes FROM storage_conversion_cursors WHERE organization_id=$1 AND family='core-work-events'`, other).Scan(&turn, &examined, &bytes); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM storage_conversion_cursors WHERE organization_id=$1 AND family='search'`, other).Scan(&search); err != nil {
					t.Fatal(err)
				}
				wantTurn, wantRows, wantBytes, wantSearch := 7, 3, 128, 0
				if otherIndex <= index {
					wantTurn, wantRows, wantBytes, wantSearch = 8, 4, 144, 1
				}
				if turn != wantTurn || examined != wantRows || bytes != wantBytes || search != wantSearch {
					t.Fatalf("tenant cursor operation leaked or failed: org=%s turn=%d rows=%d bytes=%d search=%d", other, turn, examined, bytes, search)
				}
			}
		}
		for _, tc := range []struct{ name, statement, constraint string }{
			{"unsupported cursor", `INSERT INTO storage_conversion_cursors(organization_id,family) VALUES('org_first','unsupported')`, "storage_conversion_cursors_family_check"},
			{"unsupported search progress", `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,converted_rows,converted_bytes) VALUES('org_first','prj_shared','search','41',1,1)`, "storage_conversion_progress_family_check"},
		} {
			_, err := db.ExecContext(t.Context(), tc.statement)
			var rejected *pgconn.PgError
			if !errors.As(err, &rejected) || rejected.Code != "23514" || rejected.ConstraintName != tc.constraint {
				t.Fatalf("%s did not retain family fence: %v", tc.name, err)
			}
		}
	})
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, item := range []string{"wi_first_1", "wi_first_2"} {
			f.put(t, searchIssue(org, item), searchstore.Text{Title: "sentinel"}, 0)
		}
		f.drain(t, org)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for page := 1; page <= 2; page++ {
			visited, err := f.repository.QueueSearchBackfill(t.Context(), org, 1)
			if err != nil || visited != 1 {
				t.Fatalf("page %d: visited=%d err=%v", page, visited, err)
			}
			var pending, examined int
			if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents WHERE organization_id=$1 AND indexed_revision<>revision`, org).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if err := f.admin.QueryRowContext(t.Context(), `SELECT examined_rows FROM storage_conversion_cursors WHERE organization_id=$1 AND family='search'`, org).Scan(&examined); err != nil {
				t.Fatal(err)
			}
			if pending != page || examined != page {
				t.Fatalf("pending=%d examined=%d want=%d", pending, examined, page)
			}
		}
		if n, err := f.repository.QueueSearchBackfill(t.Context(), org, 1); err != nil || n != 0 {
			t.Fatalf("wrap: n=%d err=%v", n, err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET search_format=1 WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if n, err := f.repository.QueueSearchBackfill(t.Context(), org, 1); err != nil || n != 0 {
			t.Fatalf("converted rows revisited: n=%d err=%v", n, err)
		}
	}
	for _, limit := range []int{0, 33} {
		if _, err := f.repository.QueueSearchBackfill(t.Context(), "org_first", limit); err == nil {
			t.Fatal("unbounded page accepted")
		}
	}
}
