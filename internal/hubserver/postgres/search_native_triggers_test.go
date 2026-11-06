package postgres_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchNativeMetadataLeavesTextRelationsUntouched(t *testing.T) {
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "sentinel"}, 0)
		f.put(t, searchstore.Document{OrganizationID: org, ID: "comment/example", Kind: "comment", SourceID: "comment_first_1_1", ProjectID: "prj_first", WorkItemID: "wi_first_1", Scope: "issue", Revision: 1}, searchstore.Text{Body: "comment sentinel"}, 0)
		f.drain(t, org)
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET search_format=1,content_revision=1,content_sha256=payload_sha256 WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_document_index(organization_id,document_id,generation,format,content_sha256,search_text,segment_ends)
 SELECT organization_id,id,generation,1,content_sha256,'sentinel',ARRAY[8] FROM search_documents WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		var beforeGeneration, afterGeneration int64
		var beforePostings, afterPostings, beforeIndex, afterIndex string
		if err := f.admin.QueryRowContext(t.Context(), `SELECT generation FROM search_documents WHERE organization_id=$1 AND kind='issue'`, org).Scan(&beforeGeneration); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(t.Context(), `SELECT md5(string_agg(ctid::text,',' ORDER BY scope,term)) FROM search_postings WHERE organization_id=$1`, org).Scan(&beforePostings); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(t.Context(), `SELECT md5(string_agg(ctid::text,',' ORDER BY document_id)) FROM search_document_index WHERE organization_id=$1`, org).Scan(&beforeIndex); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET revision=revision+1 WHERE organization_id=$1 AND native_id='wi_first_1';`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET revision=revision+1,updated_at=updated_at+interval '1 second' WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(t.Context(), `SELECT generation FROM search_documents WHERE organization_id=$1 AND kind='issue'`, org).Scan(&afterGeneration); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(t.Context(), `SELECT md5(string_agg(ctid::text,',' ORDER BY scope,term)) FROM search_postings WHERE organization_id=$1`, org).Scan(&afterPostings); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(t.Context(), `SELECT md5(string_agg(ctid::text,',' ORDER BY document_id)) FROM search_document_index WHERE organization_id=$1`, org).Scan(&afterIndex); err != nil {
			t.Fatal(err)
		}
		if beforeGeneration != afterGeneration || beforePostings != afterPostings || beforeIndex != afterIndex {
			t.Fatalf("metadata rewrote text or reset content: generation=%d/%d postings unchanged=%t", beforeGeneration, afterGeneration, beforePostings == afterPostings)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET content_revision=content_revision+1,content_sha256=repeat('b',64) WHERE organization_id=$1 AND kind='comment'`, org); err != nil {
			t.Fatal(err)
		}
		if err := f.admin.QueryRowContext(t.Context(), `SELECT generation FROM search_documents WHERE organization_id=$1 AND kind='issue'`, org).Scan(&afterGeneration); err != nil || afterGeneration <= beforeGeneration {
			t.Fatalf("content edit failed to invalidate parent: before=%d after=%d err=%v", beforeGeneration, afterGeneration, err)
		}
	}
}
