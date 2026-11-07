package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardSearchColdFallbackTenantWork(t *testing.T) {
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	probe := &boardQueryTracer{}
	counted, err := postgres.NewRepository(ctx, tracedFeaturePool(t, f.pool, probe), smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	f.repository = counted
	if _, err := f.admin.ExecContext(ctx, "DELETE FROM search_documents"); err != nil {
		t.Fatal(err)
	}
	indexes, err := f.admin.QueryContext(ctx, "SELECT indexname,indexdef FROM pg_indexes WHERE schemaname='public' AND (indexname LIKE 'native_board_search_title%' OR indexname LIKE 'native_work_item_search_body%')")
	if err != nil {
		t.Fatal(err)
	}
	var definitions []string
	for indexes.Next() {
		var name, definition string
		if err := indexes.Scan(&name, &definition); err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	if err := indexes.Err(); err != nil {
		t.Fatal(err)
	}
	if err := indexes.Close(); err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		name := strings.Fields(definition)[2]
		if _, err := f.admin.ExecContext(ctx, "DROP INDEX "+name); err != nil {
			t.Fatal(err)
		}
	}
	clone := func(table, org, item, title, body string, count int) {
		t.Helper()
		columns, err := f.admin.QueryContext(ctx, "SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 ORDER BY ordinal_position", table)
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
				values = append(values, org)
			case "work_item_id":
				values = append(values, item)
			case "page_title", "title":
				values = append(values, title)
			case "body":
				values = append(values, body)
			default:
				values = append(values, "b."+name)
			}
		}
		if err := columns.Err(); err != nil {
			t.Fatal(err)
		}
		if err := columns.Close(); err != nil {
			t.Fatal(err)
		}
		query := "INSERT INTO " + table + "(" + strings.Join(names, ",") + ") SELECT " + strings.Join(values, ",") + " FROM " + table + " b CROSS JOIN generate_series(1,$1::integer) n WHERE b.organization_id='org_first' AND b.work_item_id='wi_first_1'"
		if _, err := f.admin.ExecContext(ctx, query, count); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name)
 SELECT $1,'prj_first','wi_cold_'||n,n+10,'Example','human','example','Todo' FROM generate_series(1,1200) n`, org); err != nil {
			t.Fatal(err)
		}
		clone("native_board_items", "'"+org+"'", "'wi_cold_'||n", "'Example'", "''", 1200)
		clone("native_work_item_pages", "'"+org+"'", "'wi_cold_'||n", "'Example'", "(SELECT string_agg(md5(g::text),' ') FROM generate_series(1,256) g) || CASE WHEN n=1200 THEN ' release audit resume credential' ELSE '' END", 1200)
	}
	for _, query := range []string{
		"INSERT INTO organizations(organization_id,name,local,created_at) SELECT 'org_noise_'||n,'Example '||n,false,now() FROM generate_series(1,9998) n",
		"INSERT INTO projects(organization_id,id,name,profile) SELECT 'org_noise_'||n,'prj_first','Example','native' FROM generate_series(1,9998) n",
		"INSERT INTO issues(organization_id,project_id,native_id,number,title,actor_kind,actor_principal_id) SELECT 'org_noise_'||n,'prj_first','wi_noise',1,'Example','human','example' FROM generate_series(1,9998) n",
	} {
		if _, err := f.admin.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	clone("native_board_items", "'org_noise_'||n", "'wi_noise'", "'release audit resume credential'", "''", 9998)
	clone("native_work_item_pages", "'org_noise_'||n", "'wi_noise'", "'Example'", "'release audit resume credential'", 9998)
	for _, definition := range definitions {
		if _, err := f.admin.ExecContext(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.admin.ExecContext(ctx, "CREATE EXTENSION pg_buffercache"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"issues", "native_board_items", "native_work_item_pages"} {
		if _, err := f.admin.ExecContext(ctx, "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
	var statement string
	if err := f.admin.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE oid='public.board_search_candidates(text,text[],boolean,text,boolean)'::regprocedure`).Scan(&statement); err != nil {
		t.Fatal(err)
	}
	statement = strings.NewReplacer("$1", "$1::text", "$2", "$2::text[]", "$3", "$3::boolean", "$4", "$4::text", "$5", "$5::boolean").Replace(statement)
	evict := func() {
		t.Helper()
		var evicted int
		err := f.admin.QueryRowContext(ctx, "SELECT count(*) FILTER (WHERE pg_buffercache_evict(bufferid)) FROM pg_buffercache WHERE reldatabase=(SELECT oid FROM pg_database WHERE datname=current_database()) AND relfilenode IN (SELECT pg_relation_filenode(oid) FROM pg_class WHERE relnamespace IN ('public'::regnamespace,'pg_toast'::regnamespace))").Scan(&evicted)
		if err != nil {
			t.Fatal(err)
		}
		if evicted == 0 {
			t.Fatal("cold search evicted no fixture buffers")
		}
		t.Logf("evicted %d fixture buffers", evicted)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		evict()
		tx, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err = tx.ExecContext(ctx, "SET LOCAL ROLE detent_search_reader_v2"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, "SET LOCAL plan_cache_mode=force_generic_plan"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, "PREPARE cold_fallback(text,text[],boolean,text,boolean) AS "+statement); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		err = tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE cold_fallback('prj_first',ARRAY['%release audit resume credential%','%release%','%audit%','%resume%','%credential%'],false,'example',false)").Scan(&raw)
		if _, deallocateErr := tx.ExecContext(ctx, "DEALLOCATE cold_fallback"); deallocateErr != nil {
			t.Fatal(deallocateErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		t.Logf("tenant=%s cold fallback plan=%s", org, raw)
		var plans []struct {
			Plan      boardLanePlan
			Execution float64 `json:"Execution Time"`
		}
		if err = json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		var buffers []struct {
			Plan struct {
				Reads int `json:"Shared Read Blocks"`
			}
		}
		if err := json.Unmarshal(raw, &buffers); err != nil {
			t.Fatal(err)
		}
		if buffers[0].Plan.Reads == 0 {
			t.Fatal("cold fallback plan had no shared-buffer reads")
		}
		var visited float64
		var inspect func(boardLanePlan)
		inspect = func(p boardLanePlan) {
			if p.Relation == "native_work_item_pages" || p.Relation == "native_board_items" || strings.Contains(p.Index, "search_body") || strings.Contains(p.Index, "search_title") {
				visited += (p.Rows + p.Removed) * p.Loops
			}
			for _, child := range p.Plans {
				inspect(child)
			}
		}
		inspect(plans[0].Plan)
		if visited > 64 {
			t.Errorf("fallback visited %.0f text candidates outside the tenant budget", visited)
		}
		t.Logf("tenant=%s execution=%.3fms text_candidates=%.0f", org, plans[0].Execution, visited)
		evict()
		requestCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond-5*12*time.Millisecond)
		started := time.Now()
		if err := f.repository.ReadTenant(requestCtx, org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(requestCtx, viewstore.BoardSelection{ProjectID: "prj_first", SearchText: "release audit resume credential", SearchRanked: true, Limit: 20})
			if err == nil && (len(rows) != 1 || rows[0].OrganizationID != org || rows[0].WorkItemID != "wi_cold_1200") {
				return fmt.Errorf("cold fallback returned wrong tenant matches: %d", len(rows))
			}
			return err
		}); err != nil {
			t.Errorf("cold fallback failed: %v", err)
		}
		cancel()
		t.Logf("tenant=%s cold request=%s plus 60ms reserved for five production round trips", org, time.Since(started))
		if probe.page == "" {
			t.Fatal("search page query was not captured")
		}
		{
			tx, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, "SET LOCAL ROLE detent_runtime_v2"); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			err = tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+probe.page, probe.args...).Scan(&raw)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				t.Fatal(rollbackErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("tenant=%s outer plan=%s", org, raw)
			var plans []struct{ JIT json.RawMessage }
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			if len(plans[0].JIT) > 0 {
				t.Error("bounded tenant search triggered JIT compilation")
			}

		}
	}
}
