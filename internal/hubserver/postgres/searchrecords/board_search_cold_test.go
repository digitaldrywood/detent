package searchrecords_test

import (
	"context"
	"encoding/json"
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

func TestBoardSearchWindowBoundsCommonWords(t *testing.T) {
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
	clone := func(table, org, body string, count int) {
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
		query := "INSERT INTO " + table + "(" + strings.Join(names, ",") + ") SELECT " + strings.Join(values, ",") + " FROM " + table + " b CROSS JOIN generate_series(1,$1::integer) n WHERE b.organization_id='" + org + "' AND b.work_item_id='wi_first_1'"
		if _, err := f.admin.ExecContext(ctx, query, count); err != nil {
			t.Fatal(err)
		}
	}
	const issues = 1200
	body := "(SELECT string_agg((" + boardSearchVocabulary + ")[1+abs(hashtext(n::text||':'||g::text))%50],' ') FROM generate_series(1,400) g)"
	for _, org := range []string{"org_first", "org_second"} {
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name)
 SELECT $1,'prj_first','wi_window_'||n,n+10,'Example','human','example','Todo' FROM generate_series(1,$2::integer) n`, org, issues); err != nil {
			t.Fatal(err)
		}
		clone("native_board_items", org, "''", issues)
		clone("native_work_item_pages", org, body, issues)
	}
	if _, err := f.admin.ExecContext(ctx, "ANALYZE issues,native_board_items,native_work_item_pages"); err != nil {
		t.Fatal(err)
	}
	const text = "magazine count reason code"
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		var want int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM native_work_item_pages WHERE organization_id=$1 AND project_id='prj_first' AND search_vector @@ to_tsquery('pg_catalog.english',$2)`, org, searchstore.WindowQuery(text)).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if want < searchstore.WindowRows {
			t.Fatalf("fixture matches only %d issues; common words must exceed one window", want)
		}
		seen := map[tracker.NativeWorkItemID]bool{}
		var after *viewstore.BoardPosition
		windows := 0
		for {
			windows++
			if windows > issues {
				t.Fatal("search cursor did not advance")
			}
			selection := viewstore.BoardSelection{ProjectID: "prj_first", SearchText: text, SearchRanked: true, Limit: 51, After: after}
			var page viewstore.BoardModelPage
			started := time.Now()
			bounded, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
			err := counted.ReadTenant(bounded, org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(interface {
					BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
				}).BoardModelPages(bounded, selection, nil, accessstore.Authority{})
				return err
			})
			cancel()
			if err != nil {
				t.Fatalf("tenant %s window %d: %v", org, windows, err)
			}
			if windows == 1 {
				t.Logf("tenant=%s first page=%s", org, time.Since(started))
				inspectSearchPlan(t, f, org, probe)
			}
			records := page.Records
			if len(records) > 50 {
				records = records[:50]
			}
			for _, row := range records {
				if row.OrganizationID != org || seen[row.WorkItemID] {
					t.Fatalf("tenant %s returned foreign or repeated item %s", org, row.WorkItemID)
				}
				seen[row.WorkItemID] = true
			}
			if len(page.Records) > 50 {
				last := records[len(records)-1].Position
				after = &last
				continue
			}
			if page.SearchWindowEnd == nil {
				break
			}
			after = &viewstore.BoardPosition{SearchNumber: *page.SearchWindowEnd}
		}
		if len(seen) != want {
			t.Fatalf("tenant %s paged %d of %d matching issues", org, len(seen), want)
		}
		t.Logf("tenant=%s matches=%d requests=%d", org, want, windows)
	}
}

func inspectSearchPlan(t *testing.T, f searchFixture, org tracker.OrganizationID, probe *boardQueryTracer) {
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
	for _, relation := range []string{"native_work_item_pages", "native_board_items", "issues"} {
		if visited[relation] > 2*searchstore.WindowRows+102 {
			t.Errorf("tenant %s search visited %.0f %s rows for one page: %s", org, visited[relation], relation, raw)
		}
	}
	t.Logf("tenant=%s execution=%.3fms visited=%v", org, plans[0].Execution, visited)
}
