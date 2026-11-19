package searchrecords_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardSearchCoversProjectsUpToTheCeiling(t *testing.T) {
	t.Parallel()
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	clone := func(table, org, body string, first, count int) {
		t.Helper()
		columns, err := f.admin.QueryContext(ctx, "SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND is_generated='NEVER' ORDER BY ordinal_position", table)
		if err != nil {
			t.Fatal(err)
		}
		var names, values []string
		for columns.Next() {
			var name string
			if err := columns.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
			switch name {
			case "organization_id":
				values = append(values, "'"+org+"'")
			case "work_item_id":
				values = append(values, "'wi_window_'||n")
			case "number":
				values = append(values, "n+10")
			case "identifier":
				values = append(values, "'window#'||n")
			case "body":
				values = append(values, body)
			default:
				values = append(values, "b."+name)
			}
		}
		if err := columns.Close(); err != nil {
			t.Fatal(err)
		}
		query := "INSERT INTO " + table + "(" + strings.Join(names, ",") + ") SELECT " + strings.Join(values, ",") + " FROM " + table + " b CROSS JOIN generate_series($1::integer,$2::integer) n WHERE b.organization_id='" + org + "' AND b.work_item_id='wi_first_1'"
		if _, err := f.admin.ExecContext(ctx, query, first, first+count-1); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(org string, first, count, matchEvery int) {
		t.Helper()
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name)
 SELECT $1,'prj_first','wi_window_'||n,n+10,'Example','human','example','Todo' FROM generate_series($2::integer,$3::integer) n`, org, first, first+count-1); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("(CASE WHEN n %% %d = 0 THEN 'magazine count reason code' ELSE 'runner review lease' END)", matchEvery)
		clone("native_board_items", org, "''", first, count)
		clone("native_work_item_pages", org, body, first, count)
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_search(organization_id,project_id,work_item_id,number,terms)
 SELECT organization_id,project_id,work_item_id,number,setweight(to_tsvector('pg_catalog.english',lower(title COLLATE pg_catalog.pg_c_utf8)),'A')||strip(to_tsvector('pg_catalog.english',lower(body COLLATE pg_catalog.pg_c_utf8)))
 FROM native_work_item_pages WHERE organization_id=$1 AND work_item_id LIKE 'wi_window_%' ON CONFLICT DO NOTHING`, org); err != nil {
			t.Fatal(err)
		}
	}
	const text = "magazine count reason code"
	matching := func(org tracker.OrganizationID) int {
		t.Helper()
		var want int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM native_work_item_search WHERE organization_id=$1 AND project_id='prj_first' AND terms @@ to_tsquery('pg_catalog.english',$2)`, org, searchstore.WindowQuery(text)).Scan(&want); err != nil {
			t.Fatal(err)
		}
		return want
	}
	search := func(org tracker.OrganizationID, reporter bool, fingerprint string) (int, int) {
		t.Helper()
		seen := map[tracker.NativeWorkItemID]bool{}
		var after *viewstore.BoardPosition
		windows, requests := 1, 0
		for {
			requests++
			if requests > 1000 {
				t.Fatal("search cursor did not advance")
			}
			selection := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: text, Fingerprint: fingerprint, SearchRanked: true, Limit: 51, After: after, Reporter: reporter, ReporterPrincipalID: "example"}
			var page viewstore.BoardModelPage
			err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(interface {
					BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
				}).BoardModelPages(ctx, selection, nil, accessstore.Authority{})
				return err
			})
			if err != nil {
				t.Fatalf("tenant %s request %d: %v", org, requests, err)
			}
			records := page.Records
			if len(records) > 50 {
				records = records[:50]
			}
			for index, row := range records {
				if row.OrganizationID != org || seen[row.WorkItemID] {
					t.Fatalf("tenant %s returned foreign or repeated item %s", org, row.WorkItemID)
				}
				seen[row.WorkItemID] = true
				if index > 0 {
					previous := records[index-1].Position
					if row.Position.SearchRank > previous.SearchRank || row.Position.SearchRank == previous.SearchRank && row.Position.SearchNumber > previous.SearchNumber {
						t.Fatalf("tenant %s page %d is not ranked: %+v after %+v", org, requests, row.Position, previous)
					}
				}
			}
			if len(page.Records) > 50 {
				last := records[len(records)-1].Position
				after = &last
				continue
			}
			if page.SearchWindowEnd == nil {
				break
			}
			windows++
			after = &viewstore.BoardPosition{SearchWindow: *page.SearchWindowEnd}
		}
		if want := matching(org); len(seen) != want {
			t.Fatalf("tenant %s paged %d of %d matching issues", org, len(seen), want)
		}
		return windows, requests
	}
	const project = 2200
	for _, org := range []string{"org_first", "org_second"} {
		seed(org, 1, project, 2)
	}
	for _, run := range []struct {
		org      tracker.OrganizationID
		reporter bool
	}{{"org_first", false}, {"org_second", false}, {"org_first", true}} {
		if matching(run.org) < 1000 {
			t.Fatal("fixture must match more than a thousand issues with common words")
		}
		windows, requests := search(run.org, run.reporter, "")
		if windows != 1 {
			t.Fatalf("tenant %s reporter=%t used %d windows for a %d-issue project; one request must cover the project", run.org, run.reporter, windows, project)
		}
		t.Logf("tenant=%s reporter=%t requests=%d", run.org, run.reporter, requests)
	}
	seed("org_second", project+1, searchstore.WindowRows, 50)
	windows, requests := search("org_second", false, "")
	if windows < 2 {
		t.Fatalf("a project above the ceiling used %d windows, want continuation past the ceiling", windows)
	}
	t.Logf("tenant=org_second above ceiling windows=%d requests=%d", windows, requests)
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind)
 SELECT organization_id,project_id,work_item_id,'item',work_item_id,$1,true,'worker' FROM native_work_item_pages WHERE organization_id='org_second' AND project_id='prj_first'`, searchstore.FingerprintDigest("example-shared")); err != nil {
		t.Fatal(err)
	}
	if windows, _ := search("org_second", false, "example-shared"); windows < 2 {
		t.Fatalf("text and fingerprint search above both caps used %d windows", windows)
	}
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind)
 SELECT org,'prj_first','wi_window_'||(1+n%$1::integer),'comment','cmt_unrelated_'||n,encode(sha256(convert_to('unrelated-'||n,'UTF8')),'hex'),true,'worker'
 FROM unnest(ARRAY['org_first','org_second']) org CROSS JOIN generate_series(1,3000) n`, project); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		org, item, kind, record string
	}{{"org_first", "wi_window_1", "item", "wi_window_1"}, {"org_second", "wi_window_2", "comment", "cmt_old"}, {"org_second", "wi_window_2", "comment", "cmt_repeat"}} {
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind) VALUES($1,'prj_first',$2,$3,$4,$5,true,'worker')`, row.org, row.item, row.kind, row.record, searchstore.FingerprintDigest("example-old")); err != nil {
			t.Fatal(err)
		}
	}
	for org, want := range map[tracker.OrganizationID]tracker.NativeWorkItemID{"org_first": "wi_window_1", "org_second": "wi_window_2"} {
		var rows []viewstore.BoardRecord
		if err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			rows, err = r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", Fingerprint: "example-old", SearchRanked: true, Limit: 10})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].WorkItemID != want {
			t.Fatalf("tenant %s fingerprint outside the window = %+v, want only %s", org, rows, want)
		}
	}
}
