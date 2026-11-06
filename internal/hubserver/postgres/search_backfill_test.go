package postgres_test

import (
	"strings"
	"testing"

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
