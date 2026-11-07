package postgres_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchNativeFormatKeepsLegacyMaintenanceUntilCutover(t *testing.T) {
	for _, mode := range []string{"missing index", "wrong format", "wrong digest", "wrong generation", "matching index"} {
		t.Run(mode, func(t *testing.T) {
			f := newSearchFixture(t)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				t.Run(string(org), func(t *testing.T) {
					d := f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "sentinel"}, 0)
					f.put(t, searchstore.Document{OrganizationID: org, ID: "comment/example", Kind: "comment", SourceID: "comment_first_1_1", ProjectID: "prj_first", WorkItemID: "wi_first_1", Scope: "issue", Revision: 1}, searchstore.Text{Body: "comment sentinel"}, 0)
					f.drain(t, org)
					if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET search_format=1,content_revision=1,content_sha256=payload_sha256 WHERE organization_id=$1`, org); err != nil {
						t.Fatal(err)
					}
					if mode != "missing index" {
						format, offset, digest := int16(1), int64(0), ""
						switch mode {
						case "wrong format":
							format = 2
						case "wrong digest":
							digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
						case "wrong generation":
							offset = 1
						}
						if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_document_index(organization_id,document_id,generation,format,content_sha256,search_text,segment_ends)
      SELECT organization_id,id,generation+$2,$3,CASE WHEN $4::text='' THEN content_sha256 ELSE $4 END,'sentinel',ARRAY[8]
      FROM search_documents WHERE organization_id=$1`, org, offset, format, digest); err != nil {
							t.Fatal(err)
						}
					}
					before, after := "", ""
					if err := f.admin.QueryRowContext(t.Context(), `SELECT md5(string_agg(ctid::text,',' ORDER BY scope,term)) FROM search_postings WHERE organization_id=$1`, org).Scan(&before); err != nil {
						t.Fatal(err)
					}
					if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET updated_at=updated_at+interval '1 second' WHERE organization_id=$1 AND kind='issue'`, org); err != nil {
						t.Fatal(err)
					}
					if err := f.admin.QueryRowContext(t.Context(), `SELECT md5(string_agg(ctid::text,',' ORDER BY scope,term)) FROM search_postings WHERE organization_id=$1`, org).Scan(&after); err != nil {
						t.Fatal(err)
					}
					if before == after {
						t.Fatalf("tenant=%s positive native format bypassed legacy posting maintenance", org)
					}
					if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET updated_at=updated_at-interval '1 second' WHERE organization_id=$1 AND kind='issue'`, org); err != nil {
						t.Fatal(err)
					}
					if page := f.find(t, org, searchstore.Query{Text: "sentinel"}); len(page.Items) != 1 {
						t.Fatalf("tenant=%s legacy reader lost the indexed issue", org)
					}
					var generation, next int64
					if err := f.admin.QueryRowContext(t.Context(), `SELECT generation FROM search_documents WHERE organization_id=$1 AND kind='issue'`, org).Scan(&generation); err != nil {
						t.Fatal(err)
					}
					if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET revision=revision+1 WHERE organization_id=$1 AND native_id='wi_first_1'`, org); err != nil {
						t.Fatal(err)
					}
					if err := f.admin.QueryRowContext(t.Context(), `SELECT generation FROM search_documents WHERE organization_id=$1 AND kind='issue'`, org).Scan(&next); err != nil || next <= generation {
						t.Fatalf("tenant=%s native marker bypassed parent invalidation: before=%d after=%d err=%v", org, generation, next, err)
					}
					d.Revision++
					d.UpdatedAt = d.UpdatedAt.Add(time.Second)
					f.put(t, d, searchstore.Text{Title: "changedsentinel"}, 1)
					f.drain(t, org)
					if page := f.find(t, org, searchstore.Query{Text: "changedsentinel"}); len(page.Items) != 1 {
						t.Fatalf("tenant=%s legacy reader lost the changed issue", org)
					}
				})
			}
		})
	}
}
