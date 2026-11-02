package postgres_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchRLSAndSourceRelationships(t *testing.T) {
	f := newSearchFixture(t)
	for _, table := range []string{"search_documents", "search_postings", "search_scope_documents", "search_scope_refresh"} {
		var readable, writable bool
		if err := f.admin.QueryRowContext(t.Context(), `SELECT has_table_privilege('detent_platform_v2',$1,'SELECT'), has_table_privilege('detent_platform_v2',$1,'INSERT,UPDATE,DELETE,TRUNCATE')`, table).Scan(&readable, &writable); err != nil {
			t.Fatal(err)
		}
		if !readable || writable {
			t.Errorf("search helper privileges on %s: readable=%t writable=%t", table, readable, writable)
		}
	}
	var sequenceAccess bool
	if err := f.admin.QueryRowContext(t.Context(), `SELECT has_sequence_privilege('detent_platform_v2','search_refresh_order','USAGE,UPDATE')`).Scan(&sequenceAccess); err != nil {
		t.Fatal(err)
	}
	if sequenceAccess {
		t.Error("search helper can mutate refresh sequence")
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "sentinel"}, 0)
		f.put(t, searchIssue(org, "wi_first_2"), searchstore.Text{Title: "second sentinel"}, 0)
	}
	runtime, err := postgres.NewRuntime(t.Context(), f.pool, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		org    tracker.OrganizationID
		filter string
		want   int
	}{
		{"missing scope", "", "", 0},
		{"unknown scope", "unknown", "", 0},
		{"first without filter", "org_first", "", 2},
		{"second without filter", "org_second", "", 2},
		{"wrong filter", "org_first", "org_second", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var count int
			if test.org == "" {
				if err := f.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents`).Scan(&count); err != nil {
					t.Fatal(err)
				}
			} else {
				tx, err := runtime.Begin(t.Context(), test.org, &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents WHERE $1::text='' OR organization_id=$1`, test.filter).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if count != test.want {
				t.Fatalf("RLS visible count=%d want=%d", count, test.want)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*searchstore.Document)
	}{
		{"cross-tenant writer", func(d *searchstore.Document) { d.OrganizationID = "org_second" }},
		{"wrong project work item", func(d *searchstore.Document) { d.ProjectID = "prj_second" }},
		{"wrong comment parent", func(d *searchstore.Document) { d.Kind, d.SourceID = "comment", "comment_first_2_1" }},
		{"wrong comment project", func(d *searchstore.Document) { d.Kind, d.SourceID = "comment", "comment_second_1_1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := searchIssue("org_first", "wi_first_1")
			d.ID = "invalid/" + test.name
			d.UpdatedAt = historyAt()
			test.mutate(&d)
			d, err = f.content.Prepare(t.Context(), d, searchstore.Text{Title: "invalid source"})
			if err != nil {
				t.Fatal(err)
			}
			err = f.repository.WriteSearch(t.Context(), "org_first", corestore.Serving, func(w searchstore.Writer) error { return w.PutSearch(t.Context(), d, 0) })
			if err == nil {
				t.Fatal("invalid source relationship accepted")
			}
		})
	}
	t.Run("foreign refresh advancement", func(t *testing.T) {
		err := f.repository.WriteSearch(t.Context(), "org_first", corestore.Background, func(w searchstore.Writer) error {
			return w.AdvanceSearch(t.Context(), searchIssue("org_second", "wi_first_1"))
		})
		if !errors.Is(err, corestore.ErrTenantMismatch) {
			t.Fatalf("foreign advancement error=%v", err)
		}
	})

	var count int
	if err := f.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pooled tenant leaked after error: count=%d err=%v", count, err)
	}
}

func TestSearchSourceWritesShareRequestTransaction(t *testing.T) {
	f := newSearchFixture(t)
	d := searchIssue("org_first", "wi_first_1")
	d.UpdatedAt = historyAt()
	d, err := f.content.Prepare(t.Context(), d, searchstore.Text{Title: "changed source"})
	if err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("request rejected")
	err = f.repository.WriteSearch(t.Context(), "org_first", corestore.Serving, func(w searchstore.Writer) error {
		item, err := w.WorkItem(t.Context(), d.ProjectID, d.WorkItemID)
		if err != nil {
			return err
		}
		item.Title = "changed source"
		if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
			return err
		}
		if err := w.PutSearch(t.Context(), d, 0); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback error=%v", err)
	}
	err = f.repository.ReadSearch(t.Context(), "org_first", corestore.Serving, func(r searchstore.Reader) error {
		item, err := r.WorkItem(t.Context(), d.ProjectID, d.WorkItemID)
		if err != nil {
			return err
		}
		if item.Title != "Example title" {
			t.Fatalf("source title survived rollback: %s", item.Title)
		}
		if _, err := r.PendingSearch(t.Context()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("projection survived rollback: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSearchRuntimeRejectsUnsafeIndexHelper(t *testing.T) {
	for _, setting := range []string{"SECURITY INVOKER", "VOLATILE", "SET search_path=public"} {
		t.Run(setting, func(t *testing.T) {
			f := newSearchFixture(t)
			if _, err := f.admin.ExecContext(t.Context(), `ALTER FUNCTION public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[]) `+setting); err != nil {
				t.Fatal(err)
			}
			if _, err := postgres.NewRuntime(t.Context(), f.pool, f.pool); !errors.Is(err, postgres.ErrRuntimeRole) {
				t.Fatalf("unsafe search helper error=%v", err)
			}
		})
	}
}

func TestSearchIndexHelperDeniesUnscopedAndForeignTokens(t *testing.T) {
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "sentinel"}, 0)
		f.drain(t, org)
	}
	runtime, err := postgres.NewRuntime(t.Context(), f.pool, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	expression := strings.Join(searchstore.Expressions("org_second", searchstore.Query{ProjectID: "prj_first", Text: "sentinel"}), " | ")
	const query = `SELECT count(*) FROM public.search_index_candidates($1::tsquery,'prj_first',ARRAY[]::text[],false,'','work','','','all',false,'',$2::timestamptz,'',0,$3::text[],false) WHERE NOT scan_boundary`
	terms := pq.Array(searchstore.CandidateTerms("org_second", searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "sentinel"}))
	var count int
	if err := f.pool.QueryRowContext(t.Context(), query, expression, historyAt(), terms).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unscoped helper count=%d err=%v", count, err)
	}
	tx, err := runtime.Begin(t.Context(), "org_first", &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(t.Context(), query, expression, historyAt(), terms).Scan(&count); err != nil || count != 0 {
		t.Fatalf("foreign token helper count=%d err=%v", count, err)
	}
	own := searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "sentinel"}
	if err := tx.QueryRowContext(t.Context(), query, strings.Join(searchstore.Expressions("org_first", own), " | "), historyAt(), pq.Array(searchstore.CandidateTerms("org_first", own))).Scan(&count); err != nil || count != 1 {
		t.Fatalf("own token helper count=%d error=%v", count, err)
	}

}

type searchPlanProbe struct {
	pgquery.DBTX
	statement string
	args      []any
}

func (p *searchPlanProbe) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	p.statement, p.args = statement, args
	return p.DBTX.QueryContext(ctx, statement, args...)
}

func TestSearchIndexedQueryPlan(t *testing.T) {
	f := newSearchFixture(t)
	d := searchstore.Document{OrganizationID: "org_first", ID: "target", SourceID: "target", Kind: "thread", Scope: "general", Status: "open", Revision: 1}
	d = f.put(t, d, searchstore.Text{Title: "rareplanterm"}, 0)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "rareplanterm"}, 0)
	f.drain(t, "org_first")
	_, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents (organization_id,id,kind,source_id,revision,indexed_revision,title,scope,status,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,vector)
        SELECT 'org_first','other_'||n,'thread','other_'||n,1,1,'commonterm','general','open',CURRENT_TIMESTAMP,$1,$2,$3,$4,$5::tsvector FROM generate_series(1,10000) n`, d.Payload.Key, d.Payload.SHA256, d.Payload.CompressedSize, d.Payload.UncompressedSize, searchstore.Vector(searchstore.Document{OrganizationID: d.OrganizationID, ProjectID: d.ProjectID, Kind: "thread"}, searchstore.Text{Title: "commonterm"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `ANALYZE search_documents`); err != nil {
		t.Fatal(err)
	}
	runtime, err := postgres.NewRuntime(t.Context(), f.pool, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err := f.admin.QueryRowContext(t.Context(), `SELECT prosrc FROM pg_proc WHERE oid='public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[])'::regprocedure`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(body, "RETURN QUERY")
	if !found {
		t.Fatal("search index helper has no query")
	}
	body = strings.TrimSuffix(strings.TrimSpace(body), "END")
	body = strings.TrimSuffix(strings.TrimSpace(body), ";")
	for _, kind := range []string{"thread", "work"} {
		for _, test := range []struct {
			name, text string
			exact      bool
		}{
			{"ordinary", "rareplanterm", false},
			{"exact", "rareplanterm", true},
			{"prefix", "rareplan", false},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				q := searchstore.Query{Kind: kind, Text: test.text, Exact: test.exact}
				if kind == "work" {
					q.ProjectID = "prj_first"
				}
				expression := strings.Join(searchstore.Expressions("org_first", q), " | ")
				terms := searchstore.CandidateTerms("org_first", q)
				tx, err := runtime.Begin(t.Context(), "org_first", &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				probe := &searchPlanProbe{DBTX: tx}
				if _, err := pgquery.New(probe).SearchCandidates(t.Context(), pgquery.SearchCandidatesParams{OrganizationID: "org_first", Kind: kind, ProjectID: string(q.ProjectID), Expression: expression, SearchTerms: terms, Exact: q.Exact, ProjectIds: []string{}}); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				planTx, err := f.admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer planTx.Rollback()
				if _, err := planTx.ExecContext(t.Context(), `SET LOCAL ROLE detent_platform_v2; SELECT set_config('detent.organization_id', 'org_first', true)`); err != nil {
					t.Fatal(err)
				}
				var raw []byte
				if err := planTx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+body, expression, string(q.ProjectID), pq.Array([]string{}), false, "", kind, "", "", "", false, "", historyAt(), "", int64(0), pq.Array(terms), q.Exact, pq.Array([]string{}), pq.Array([]string{}), pq.Array([]float64{}), pq.Array([]string{}), pq.Array([]int64{}), pq.Array([]bool{})).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var plan []struct{ Plan searchPlanNode }
				if err := json.Unmarshal(raw, &plan); err != nil {
					t.Fatal(err)
				}
				if len(plan) != 1 || !plan[0].Plan.usesIndex("search_documents_vector_idx") {
					t.Fatalf("search did not execute its GIN vector index: %s", raw)
				}
			})
		}
	}
}

type searchPlanNode struct {
	Index        string           `json:"Index Name"`
	Relation     string           `json:"Relation Name"`
	Rows         float64          `json:"Actual Rows"`
	Loops        float64          `json:"Actual Loops"`
	Removed      float64          `json:"Rows Removed by Filter"`
	IndexRemoved float64          `json:"Rows Removed by Index Recheck"`
	Plans        []searchPlanNode `json:"Plans"`
}

func (p searchPlanNode) usesIndex(name string) bool {
	if p.Index == name && p.Loops > 0 {
		return true
	}
	for _, child := range p.Plans {
		if child.usesIndex(name) {
			return true
		}
	}
	return false
}

func (p searchPlanNode) documentWork() float64 {
	work := float64(0)
	if p.Relation == "search_documents" || p.Relation == "issues" || p.Relation == "search_postings" || p.Relation == "search_scope_refresh" || p.Relation == "search_scope_documents" || p.Relation == "projects" || p.Relation == "native_comments" {
		work = (p.Rows + p.Removed + p.IndexRemoved) * p.Loops
	}
	if p.Relation == "" && (strings.HasPrefix(p.Index, "search_documents_") || strings.HasPrefix(p.Index, "search_postings_")) {
		work += (p.Rows + p.Removed + p.IndexRemoved) * p.Loops
	}
	for _, child := range p.Plans {
		work += child.documentWork()
	}
	return work
}

func TestSearchExcludedHistoryDoesNotGrowCandidateWork(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=true,actor_principal_id='other-principal' WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id='org_first' AND native_id='wi_first_2'`); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := f.admin.QueryRowContext(t.Context(), `SELECT prosrc FROM pg_proc WHERE pronamespace='public'::regnamespace AND proname='search_index_candidates'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(body, "RETURN QUERY")
	if !found {
		t.Fatal("search helper query missing")
	}
	body = strings.TrimSuffix(strings.TrimSpace(body), "END")
	body = strings.TrimSuffix(strings.TrimSpace(body), ";")
	examinedAtLimit := make(map[string]float64)
	for _, size := range []int{2000, 10000} {
		seedSearchPlanHistory(t, f, d, map[int]int{2000: 1, 10000: 2001}[size], size)
		for _, statement := range []string{"VACUUM (ANALYZE) search_documents", "VACUUM (ANALYZE) search_postings", "VACUUM (ANALYZE) search_scope_documents"} {
			if _, err := f.admin.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range []struct {
			name, archived string
			reporter       bool
		}{
			{"archived", "false", false}, {"reporter invisible", "all", true}, {"visible", "all", false},
		} {
			t.Run(fmt.Sprintf("%s/%d", test.name, size), func(t *testing.T) {
				tx, err := f.admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(t.Context(), `SET LOCAL ROLE detent_platform_v2; SELECT set_config('detent.organization_id','org_first',true)`); err != nil {
					t.Fatal(err)
				}
				for _, cursor := range []string{"", "before-history"} {
					for _, text := range []string{"commonterm", "common"} {
						for _, kind := range []string{"thread", "work"} {
							expression := strings.Join(searchstore.Expressions("org_first", searchstore.Query{ProjectID: "prj_first", Text: text}), " | ")
							args := []any{expression, "prj_first", pq.Array([]string{}), false, "", kind, "", "", test.archived, test.reporter, "example-principal", historyAt(), cursor, int64(0)}
							if strings.Contains(body, "$15") {
								args = append(args, pq.Array(searchstore.CandidateTerms("org_first", searchstore.Query{Kind: kind, ProjectID: "prj_first", Text: text})), false)
							}
							args = append(args, pq.Array([]string{}), pq.Array([]string{}), pq.Array([]float64{}), pq.Array([]string{}), pq.Array([]int64{}), pq.Array([]bool{}))
							var raw []byte
							if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+body, args...).Scan(&raw); err != nil {
								t.Fatal(err)
							}
							var plan []struct{ Plan searchPlanNode }
							if err := json.Unmarshal(raw, &plan); err != nil {
								t.Fatal(err)
							}
							work := plan[0].Plan.documentWork()
							key := test.name + "/" + kind + "/" + text + "/" + cursor
							if size == 2000 {
								examinedAtLimit[key] = work
							} else if work != examinedAtLimit[key] {
								t.Errorf("%s history work grew from %.0f to %.0f", key, examinedAtLimit[key], work)
							}
							if work > 12*searchstore.CandidateLimit {
								t.Errorf("%s history=%d examined=%.0f: %s", kind, size, work, raw)
							}
							t.Logf("%s history=%d examined=%.0f", key, size, work)
						}
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	page := f.find(t, "org_first", searchstore.Query{Text: "commonterm", Archived: "all", Reporter: new("example-principal")})
	if got := searchIDs(page); fmt.Sprint(got) != "[wi_first_2]" {
		t.Fatalf("visible result=%v", got)
	}
}

func TestSearchPrefixExhaustionContinuesThroughUnrelatedHistory(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "commonterm"}, 0)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	seedSearchPlanHistory(t, f, d, 1, 2*(searchstore.CandidateLimit+1))
	vector := searchstore.Vector(searchstore.Document{OrganizationID: "org_first", ProjectID: "prj_first"}, searchstore.Text{Title: "unrelated"})
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET vector=$1::tsvector,updated_at=CURRENT_TIMESTAMP+INTERVAL '1 day'
 WHERE organization_id='org_first' AND id LIKE 'history_%' AND substring(id from 9)::int>$2`, vector, searchstore.CandidateLimit+1); err != nil {
		t.Fatal(err)
	}
	query := searchstore.Query{Text: "common", Limit: 1}
	seen := make(map[string]bool)
	var matches []string
	for range 5 {
		page := f.find(t, "org_first", query)
		matches = append(matches, searchIDs(page)...)
		if page.NextCursor == "" {
			if fmt.Sprint(matches) != "[wi_first_2 wi_first_1]" {
				t.Fatalf("matches=%v want=[wi_first_2 wi_first_1]", matches)
			}
			return
		}
		if seen[page.NextCursor] {
			t.Fatal("continuation cursor did not advance")
		}
		seen[page.NextCursor] = true
		query.Cursor = page.NextCursor
	}
	t.Fatal("search did not finish in bounded pages")
}

func TestSearchHiddenScanBoundaryContinues(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	seedSearchWorkHistory(t, f, d, 2*(searchstore.CandidateLimit+1))
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=true WHERE organization_id='org_first' AND native_id LIKE 'wi_history_%'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_postings SET impact=100 WHERE organization_id='org_first' AND document_id LIKE 'history_%'`); err != nil {
		t.Fatal(err)
	}

	query := searchstore.Query{Text: "common", Limit: 1}
	first := f.find(t, "org_first", query)
	if len(first.Items) != 0 || first.NextCursor == "" {
		t.Fatalf("hidden scan window=%+v, want empty page with cursor", first)
	}
	query.Cursor = first.NextCursor
	for range 5 {
		page := f.find(t, "org_first", query)
		if slices.Contains(searchIDs(page), "wi_first_2") {
			return
		}
		if page.NextCursor == "" || page.NextCursor == query.Cursor {
			t.Fatalf("older match unreachable: %+v", page)
		}
		query.Cursor = page.NextCursor
	}
	t.Fatal("older match was not reached")
}

func TestSearchExactTermContinuesBeyondCandidateLimit(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	seedSearchWorkHistory(t, f, d, searchstore.CandidateLimit+1)
	term := searchstore.Prefix("org_first", "prj_first") + "wcommonterm"
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_postings SET impact=100
 WHERE organization_id='org_first' AND document_id LIKE 'history_%' AND term=$1`, term); err != nil {
		t.Fatal(err)
	}
	var postings int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_postings
	 WHERE organization_id='org_first' AND document_id LIKE 'history_%' AND term=$1
 AND scope=public.search_scope_key('prj_first','','work','','','false','')`, term).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != searchstore.CandidateLimit+1 {
		t.Fatalf("higher-impact postings=%d", postings)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
 SELECT organization_id,scope,$2,document_id,50,updated_at,updated_at_submicrosecond FROM search_postings
 WHERE organization_id='org_first' AND term=$1 AND document_id='history_1'`, term, searchstore.Prefix("org_first", "prj_first")+"wotherterm"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"commonterm", "commonterm otherterm"} {
		t.Run(text, func(t *testing.T) {
			query := searchstore.Query{Text: text, Exact: true, Limit: 1}
			for pageNumber := range 10 {
				page := f.find(t, "org_first", query)
				if slices.Contains(searchIDs(page), "wi_first_2") {
					return
				}
				if page.NextCursor == "" || page.NextCursor == query.Cursor {
					t.Fatalf("exact match beyond candidate limit unreachable: %+v", page)
				}
				if pageNumber == 0 {
					raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
					if err != nil {
						t.Fatal(err)
					}
					var position struct{ Streams []searchstore.StreamPosition }
					if err := json.Unmarshal(raw, &position); err != nil {
						t.Fatal(err)
					}
					for _, requested := range strings.Fields(text) {
						index := slices.IndexFunc(position.Streams, func(p searchstore.StreamPosition) bool {
							return strings.HasSuffix(p.Key, ":"+searchstore.Prefix("org_first", "prj_first")+"w"+requested)
						})
						if index < 0 {
							t.Fatalf("missing %s stream in cursor: %+v", requested, position.Streams)
						}
						stream := position.Streams[index]
						if stream.ID == "" || stream.Done != (requested == "otherterm") || stream.Impact != map[string]float64{"commonterm": 100, "otherterm": 50}[requested] {
							t.Fatalf("incorrect %s stream position: %+v", requested, stream)
						}
					}
				}
				query.Cursor = page.NextCursor
			}
			t.Fatal("exact match was not reached")
		})
	}

}

func seedSearchWorkHistory(t *testing.T, f searchFixture, d searchstore.Document, count int) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title)
 SELECT 'org_first','wi_history_'||n,'prj_first',n+10,'commonterm' FROM generate_series(1,$1::int) n`, count); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents DISABLE TRIGGER search_document_postings`); err != nil {
		t.Fatal(err)
	}
	_, insertErr := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,indexed_revision,title,scope,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,vector)
 SELECT 'org_first','history_'||n,'prj_first','wi_history_'||n,'issue','wi_history_'||n,1,1,'commonterm','issue',CURRENT_TIMESTAMP,$1,$2,$3,$4,$5::tsvector FROM generate_series(1,$6::int) n`, d.Payload.Key, d.Payload.SHA256, d.Payload.CompressedSize, d.Payload.UncompressedSize, searchstore.Vector(searchstore.Document{OrganizationID: d.OrganizationID, ProjectID: d.ProjectID, Kind: "thread"}, searchstore.Text{Title: "commonterm"}), count)
	_, enableErr := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents ENABLE TRIGGER search_document_postings`)
	if err := errors.Join(insertErr, enableErr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
 SELECT d.organization_id,public.search_scope_key(d.project_id,'','work','','','false',''),$1,d.id,1,d.updated_at,d.updated_at_submicrosecond
 FROM search_documents d WHERE d.organization_id='org_first' AND d.id LIKE 'history_%'`, searchstore.Prefix("org_first", "prj_first")+"wcommonterm"); err != nil {
		t.Fatal(err)
	}
}

func seedSearchPlanHistory(t *testing.T, f searchFixture, d searchstore.Document, start, end int) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO native_comments(organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id)
 SELECT 'org_first','history_'||n,'prj_first','wi_first_1',n+10,$1,'human','other-principal' FROM generate_series($2::int,$3::int) n`, d.Payload.Key, start, end); err != nil {
		t.Fatal(err)
	}
	insert := func(first, last int) error {
		ids, keys, hashes := []string{}, []string{}, []string{}
		compressed, raw, nanos := []int64{}, []int64{}, []int64{}
		times := []time.Time{}
		for index := first; index <= last; index++ {
			doc := searchIssue("org_first", "wi_first_1")
			doc.ID = fmt.Sprintf("history_%d", index)
			doc.Kind = "comment"
			doc.SourceID = doc.ID
			doc.UpdatedAt = historyAt().Add(time.Hour + time.Duration(index))
			doc.Title = "commonterm"
			doc.UpdatedAt = doc.UpdatedAt.UTC()
			sourcePayload, err := json.Marshal(struct {
				Document searchstore.Document
				Text     searchstore.Text
			}{doc, searchstore.Text{Title: "commonterm", Body: strings.Repeat("filler ", 1024)}})
			if err != nil {
				return err
			}
			reference, err := f.objects.StoreDetail(t.Context(), string(doc.OrganizationID), sourcePayload)
			if err != nil {
				return err
			}
			ids = append(ids, doc.ID)
			keys = append(keys, reference.Key)
			hashes = append(hashes, reference.SHA256)
			compressed = append(compressed, reference.CompressedSize)
			raw = append(raw, reference.UncompressedSize)
			times = append(times, doc.UpdatedAt.Truncate(time.Microsecond))
			nanos = append(nanos, int64(doc.UpdatedAt.Nanosecond()%1000))
		}
		_, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,indexed_revision,title,scope,status,updated_at,updated_at_submicrosecond,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,vector)
  SELECT 'org_first',id,'prj_first','wi_first_1','comment',id,1,1,'commonterm','issue','',at,nanos,key,hash,compressed,raw,$1::tsvector
  FROM unnest($2::text[],$3::text[],$4::text[],$5::bigint[],$6::bigint[],$7::timestamptz[],$8::bigint[]) row(id,key,hash,compressed,raw,at,nanos)`,
			searchstore.Vector(searchstore.Document{OrganizationID: d.OrganizationID, ProjectID: d.ProjectID, Kind: "thread"}, searchstore.Text{Title: "commonterm"}), pq.Array(ids), pq.Array(keys), pq.Array(hashes), pq.Array(compressed), pq.Array(raw), pq.Array(times), pq.Array(nanos))
		return err
	}
	var indexed bool
	if err := f.admin.QueryRowContext(t.Context(), `SELECT to_regclass('search_postings') IS NOT NULL`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		if err := insert(start, end); err != nil {
			t.Fatal(err)
		}
		return
	}
	if start == 1 {
		if err := insert(1, 1); err != nil {
			t.Fatal(err)
		}
		start++
	}
	if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents DISABLE TRIGGER search_document_postings`); err != nil {
		t.Fatal(err)
	}
	insertErr := insert(start, end)
	_, enableErr := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents ENABLE TRIGGER search_document_postings`)
	if err := errors.Join(insertErr, enableErr); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_scope_documents(organization_id,project_id,work_item_id,id)
 SELECT p.organization_id,p.project_id,p.work_item_id,'history_'||n FROM search_scope_documents p CROSS JOIN generate_series($1::int,$2::int) n
 WHERE p.organization_id='org_first' AND p.project_id='prj_first' AND p.work_item_id IN ('','wi_first_1') AND p.id='history_1'`, start, end); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
 SELECT p.organization_id,p.scope,p.term,'history_'||n,p.impact,d.updated_at,d.updated_at_submicrosecond FROM search_postings p CROSS JOIN generate_series($1::int,$2::int) n
 JOIN search_documents d ON d.organization_id=p.organization_id AND d.id='history_'||n
 WHERE p.organization_id='org_first' AND p.document_id='history_1'`, start, end); err != nil {
		t.Fatal(err)
	}
}

func TestSearchWorkItemsAppearOnceAcrossWindows(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Body: "commonterm"}, 0)
	f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	seedSearchPlanHistory(t, f, d, 1, searchstore.CandidateLimit+1)
	comment := searchIssue("org_first", "wi_first_1")
	comment.ID, comment.Kind, comment.SourceID, comment.Revision = "history_1", "comment", "history_1", 2
	if err := f.admin.QueryRowContext(t.Context(), `SELECT updated_at FROM search_documents
 WHERE organization_id='org_first' AND id='history_2'`).Scan(&comment.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	comment.UpdatedAt = comment.UpdatedAt.Add(time.Hour)
	f.put(t, comment, searchstore.Text{Title: "commonterm raremarker"}, 1)
	f.drain(t, "org_first")
	term := searchstore.Prefix("org_first", "prj_first") + "wcommonterm"
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_postings SET impact=100
 WHERE organization_id='org_first' AND document_id LIKE 'history_%' AND term=$1`, term); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		text  string
		exact bool
	}{
		{"prefix", "common", false},
		{"exact", "commonterm", true},
		{"mixed coverage", "common raremarker", false},
	} {
		for _, limit := range []int{1, corestore.MaxPageSize} {
			t.Run(fmt.Sprintf("%s/%d", test.name, limit), func(t *testing.T) {
				query := searchstore.Query{Text: test.text, Exact: test.exact, Limit: limit}
				var matches []string
				for range 10 {
					page := f.find(t, "org_first", query)
					for _, hit := range page.Items {
						if hit.Document.WorkItemID == "wi_first_1" && (hit.Document.Kind != "issue" || !strings.Contains(hit.Snippet, "raremarker") && test.name == "mixed coverage") {
							t.Fatalf("work item did not retain its aggregate document and comment match: %+v", hit)
						}
					}
					matches = append(matches, searchIDs(page)...)
					if page.NextCursor == "" {
						slices.Sort(matches)
						if !slices.Equal(matches, []string{"wi_first_1", "wi_first_2"}) {
							t.Fatalf("cross-window matches=%v want=[wi_first_1 wi_first_2]", matches)
						}
						return
					}
					if page.NextCursor == query.Cursor {
						t.Fatal("cursor did not advance")
					}
					query.Cursor = page.NextCursor
				}
				t.Fatal("search did not exhaust all windows")
			})
		}
	}
}

func TestSearchRareTermRanksBeforeRecentPartialMatch(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "commonterm"}, 0)
	f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "common raremarker"}, 0)
	f.drain(t, "org_first")
	seedSearchPlanHistory(t, f, d, 1, searchstore.CandidateLimit+1)
	comment := searchIssue("org_first", "wi_first_1")
	comment.ID, comment.Kind, comment.SourceID, comment.Revision = "history_1", "comment", "history_1", 2
	if err := f.admin.QueryRowContext(t.Context(), `SELECT updated_at FROM search_documents
 WHERE organization_id='org_first' AND id='history_2'`).Scan(&comment.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	comment.UpdatedAt = comment.UpdatedAt.Add(time.Hour)
	f.put(t, comment, searchstore.Text{Title: "commonterm"}, 1)
	f.drain(t, "org_first")
	for _, limit := range []int{1, corestore.MaxPageSize} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			page := f.find(t, "org_first", searchstore.Query{Text: "common raremarker", Limit: limit})
			if len(page.Items) == 0 || page.Items[0].Document.WorkItemID != "wi_first_2" || page.Items[0].Coverage != 2 {
				t.Fatalf("coverage-first ranking=%+v", page.Items)
			}
		})
	}
}

func TestSearchWorkCursorRejectsLegacyState(t *testing.T) {
	f := newSearchFixture(t)
	for _, id := range []string{"wi_first_1", "wi_first_2"} {
		f.put(t, searchIssue("org_first", id), searchstore.Text{Title: "sentinel"}, 0)
	}
	query := searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "sentinel", Limit: 1}
	first := f.find(t, "org_first", query)
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var position map[string]json.RawMessage
	if err := json.Unmarshal(raw, &position); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			position["Version"], err = json.Marshal(version)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(position)
			if err != nil {
				t.Fatal(err)
			}
			query.Cursor = base64.RawURLEncoding.EncodeToString(raw)
			if _, err := f.content.Find(t.Context(), f.repository, "org_first", query); !errors.Is(err, corestore.ErrInvalidCursor) {
				t.Fatalf("legacy cursor error=%v", err)
			}
		})
	}
}

func TestSearchNumericMatchAppearsOnceAcrossWindows(t *testing.T) {
	for _, test := range []struct {
		name         string
		withoutIssue bool
		want         []string
	}{
		{name: "issue", want: []string{"wi_first_2", "wi_first_1"}},
		{name: "comment only", withoutIssue: true, want: []string{"wi_first_2", "wi_first_1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSearchFixture(t)
			d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "2"}, 0)
			f.drain(t, "org_first")
			seedSearchPlanHistory(t, f, d, 1, searchstore.CandidateLimit+1)
			vector := searchstore.Vector(searchstore.Document{OrganizationID: "org_first", ProjectID: "prj_first"}, searchstore.Text{Title: "2"})
			if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET vector=$1::tsvector WHERE organization_id='org_first' AND id LIKE 'history_%'`, vector); err != nil {
				t.Fatal(err)
			}
			comment := searchIssue("org_first", "wi_first_1")
			comment.ID, comment.Kind, comment.SourceID, comment.Revision = "history_1", "comment", "history_1", 2
			f.put(t, comment, searchstore.Text{Title: "2"}, 1)
			f.drain(t, "org_first")
			if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_postings SET impact=100 WHERE organization_id='org_first' AND document_id LIKE 'history_%' AND term=$1`, searchstore.Prefix("org_first", "prj_first")+"w2"); err != nil {
				t.Fatal(err)
			}
			if test.withoutIssue {
				if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO native_comments(organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id)
   VALUES('org_first','numeric_target','prj_first','wi_first_2',1000,$1,'human','example-principal')`, d.Payload.Key); err != nil {
					t.Fatal(err)
				}
				target := searchIssue("org_first", "wi_first_2")
				target.ID, target.Kind, target.SourceID = "comment/numeric_target", "comment", "numeric_target"
				f.put(t, target, searchstore.Text{Title: "2"}, 0)
				if err := f.repository.WriteSearch(t.Context(), "org_first", corestore.Serving, func(w searchstore.Writer) error { return w.DeleteSearch(t.Context(), d.ID, d.Revision) }); err != nil {
					t.Fatal(err)
				}
				f.drain(t, "org_first")
			}
			query := searchstore.Query{Text: "2", Exact: true, Limit: 1}
			var matches []string
			for range 10 {
				page := f.find(t, "org_first", query)
				matches = append(matches, searchIDs(page)...)
				if page.NextCursor == "" {
					if !slices.Equal(matches, test.want) {
						t.Fatalf("numeric continuation matches=%v", matches)
					}
					return
				}
				query.Cursor = page.NextCursor
			}
			t.Fatal("numeric search did not exhaust")
		})
	}
}
