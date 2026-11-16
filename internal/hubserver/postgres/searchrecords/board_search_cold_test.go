package searchrecords_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const boardSearchVocabulary = `ARRAY['the','runner','lease','review','change','magazine','count','reason','code','release','search','index','project','issue','tenant','board','worker','commit','branch','landing','deploy','policy','queue','retry','budget','storage','postgres','hub','token','report','failure','timeout','window','cursor','page','body','title','label','state','done','cancel','todo','progress','agent','codex','claude','gate','test','suite','package']`

func TestBoardSearchCoversProjectsUpToTheCeiling(t *testing.T) {
	t.Parallel()
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	probe := &boardQueryTracer{}
	counted, err := postgres.NewRepository(ctx, pgstoretest.TracedFeaturePool(t, f.pool, probe), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
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
	seed := func(org string, first, count, words int) {
		t.Helper()
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name)
 SELECT $1,'prj_first','wi_window_'||n,n+10,'Example','human','example','Todo' FROM generate_series($2::integer,$3::integer) n`, org, first, first+count-1); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("(SELECT string_agg((%s)[1+abs(hashtext(n::text||':'||g::text))%%50],' ') FROM generate_series(1,%d) g)", boardSearchVocabulary, words)
		clone("native_board_items", org, "''", first, count)
		clone("native_work_item_pages", org, body, first, count)
		if _, err := f.admin.ExecContext(ctx, "ANALYZE issues,native_board_items,native_work_item_pages"); err != nil {
			t.Fatal(err)
		}
	}
	const text = "magazine count reason code"
	matching := func(org tracker.OrganizationID) int {
		t.Helper()
		var want int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM native_work_item_pages WHERE organization_id=$1 AND project_id='prj_first' AND search_vector @@ to_tsquery('pg_catalog.english',$2)`, org, searchstore.WindowQuery(text)).Scan(&want); err != nil {
			t.Fatal(err)
		}
		return want
	}
	search := func(org tracker.OrganizationID, reporter bool, bound float64, fingerprint string) (int, int) {
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
			started := time.Now()
			bounded, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
			probe.page = ""
			err := counted.ReadTenant(bounded, org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(interface {
					BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
				}).BoardModelPages(bounded, selection, nil, accessstore.Authority{})
				return err
			})
			cancel()
			if err != nil {
				t.Fatalf("tenant %s request %d: %v", org, requests, err)
			}
			if requests == 1 {
				t.Logf("tenant=%s reporter=%t first page=%s", org, reporter, time.Since(started))
				inspectSearchPlan(t, f, org, probe, bound)
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
		seed(org, 1, project, 400)
	}
	for _, run := range []struct {
		org      tracker.OrganizationID
		reporter bool
	}{{"org_first", false}, {"org_second", false}, {"org_first", true}} {
		if matching(run.org) < 1000 {
			t.Fatal("fixture must match more than a thousand issues with common words")
		}
		windows, requests := search(run.org, run.reporter, 4*project+102, "")
		if windows != 1 {
			t.Fatalf("tenant %s reporter=%t used %d windows for a %d-issue project; one request must cover the project", run.org, run.reporter, windows, project)
		}
		t.Logf("tenant=%s reporter=%t requests=%d", run.org, run.reporter, requests)
	}
	seed("org_second", project+1, searchstore.WindowRows, 12)
	windows, requests := search("org_second", false, 4*searchstore.WindowRows+102, "")
	if windows < 2 {
		t.Fatalf("a project above the ceiling used %d windows, want continuation past the ceiling", windows)
	}
	t.Logf("tenant=org_second above ceiling windows=%d requests=%d", windows, requests)
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind)
 SELECT organization_id,project_id,work_item_id,'item',work_item_id,$1,true,'worker' FROM native_work_item_pages WHERE organization_id='org_second' AND project_id='prj_first'`, searchstore.FingerprintDigest("example-shared")); err != nil {
		t.Fatal(err)
	}
	if windows, _ := search("org_second", false, 5*searchstore.WindowRows+102, "example-shared"); windows < 2 {
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
	if _, err := f.admin.ExecContext(ctx, "ANALYZE native_work_item_fingerprints"); err != nil {
		t.Fatal(err)
	}
	for org, want := range map[tracker.OrganizationID]tracker.NativeWorkItemID{"org_first": "wi_window_1", "org_second": "wi_window_2"} {
		var rows []viewstore.BoardRecord
		probe.page = ""
		if err := counted.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			rows, err = r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_first", Fingerprint: "example-old", SearchRanked: true, Limit: 10})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].WorkItemID != want {
			t.Fatalf("tenant %s fingerprint outside the window = %+v, want only %s", org, rows, want)
		}
		inspectSearchPlan(t, f, org, probe, 10)
	}
}

func inspectSearchPlan(t *testing.T, f searchFixture, org tracker.OrganizationID, probe *boardQueryTracer, bound float64) {
	t.Helper()
	if probe.page == "" {
		t.Fatal("search page query was not captured")
	}
	tx, err := f.admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+probe.page, probe.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan      boardLanePlan
		Execution float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	visited := map[string]float64{}
	var inspect func(boardLanePlan)
	inspect = func(node boardLanePlan) {
		if node.Relation != "" {
			visited[node.Relation] += (node.Rows + node.Removed) * node.Loops
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plans[0].Plan)
	for _, relation := range []string{"native_work_item_pages", "native_work_item_fingerprints", "native_board_items", "issues"} {
		if visited[relation] > bound {
			t.Errorf("tenant %s search visited %.0f %s rows for one page: %s", org, visited[relation], relation, raw)
		}
	}
	t.Logf("tenant=%s execution=%.3fms visited=%v", org, plans[0].Execution, visited)
}
