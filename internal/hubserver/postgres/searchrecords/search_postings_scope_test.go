package searchrecords_test

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const postingScopeBody = "Repository association deadlocks appear when two workers refresh the same project index while a reporter archives an issue. The refresh owner retries with capped backoff, records the attempt, and publishes the document once the source revision is current. Operators compare the candidate window against the visible scope before trusting a duplicate answer."

func TestSearchPostingsOnePerIndexedLexeme(t *testing.T) {
	f := newSearchFixture(t)
	foreign := 0
	var exemplar searchstore.Document
	for _, size := range []int{0, 3000} {
		if size > foreign {
			seedForeignIssueIndex(t, f, exemplar, foreign+1, size)
			foreign = size
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			t.Run(fmt.Sprintf("%s/foreign=%d", org, size), func(t *testing.T) {
				item := fmt.Sprintf("wi_first_%d", map[int]int{0: 1, 3000: 2}[size])
				d := f.put(t, searchIssue(org, item), searchstore.Text{Title: "Repository association", Body: postingScopeBody}, 0)
				f.drain(t, org)
				exemplar = d
				var rows, terms, scopes, lexemes int
				if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*),count(DISTINCT term),count(DISTINCT scope),
 (SELECT count(*) FROM search_documents d CROSS JOIN LATERAL unnest(tsvector_to_array(d.vector)) v(term) WHERE d.organization_id=$1 AND d.id=$2 AND starts_with(v.term,$3))
 FROM search_postings WHERE organization_id=$1 AND document_id=$2 AND left(term,1)<>'~'`, org, d.ID, searchstore.Prefix(org, "prj_first")).Scan(&rows, &terms, &scopes, &lexemes); err != nil {
					t.Fatal(err)
				}
				if rows < 300 || rows != terms || rows != lexemes || scopes != 1 {
					t.Fatalf("postings=%d distinct terms=%d project lexemes=%d scopes=%d", rows, terms, lexemes, scopes)
				}
				if got := searchIDs(f.find(t, org, searchstore.Query{Text: "repos associ", Limit: 20})); !slices.Contains(got, item) {
					t.Fatalf("published document not searchable: %v", got)
				}
			})
		}
	}
}

func TestSearchScopedCallerNeverReceivesForeignDocument(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup string
		after string
		org   tracker.OrganizationID
		docs  []searchstore.Document
		query searchstore.Query
		want  string
	}{
		{name: "organization", org: "org_first", docs: []searchstore.Document{searchIssue("org_first", "wi_first_1"), searchIssue("org_second", "wi_first_1")}, query: searchstore.Query{}, want: "[wi_first_1]"},
		{name: "project", org: "org_first", docs: []searchstore.Document{searchIssue("org_first", "wi_first_1"), projectIssue("org_first", "prj_second", "wi_second_1")}, query: searchstore.Query{}, want: "[wi_first_1]"},
		{name: "archived default", setup: `UPDATE issues SET archived=true WHERE organization_id='org_first' AND native_id='wi_first_2'`, org: "org_first", docs: []searchstore.Document{searchIssue("org_first", "wi_first_1"), searchIssue("org_first", "wi_first_2")}, query: searchstore.Query{}, want: "[wi_first_1]"},
		{name: "archived only", setup: `UPDATE issues SET archived=true WHERE organization_id='org_first' AND native_id='wi_first_2'`, org: "org_first", docs: []searchstore.Document{searchIssue("org_first", "wi_first_1"), searchIssue("org_first", "wi_first_2")}, query: searchstore.Query{Archived: "true"}, want: "[wi_first_2]"},
		{name: "reporter", setup: `UPDATE issues SET actor_principal_id='other-principal' WHERE organization_id='org_first' AND native_id='wi_first_2'`, org: "org_first", docs: []searchstore.Document{searchIssue("org_first", "wi_first_1"), searchIssue("org_first", "wi_first_2")}, query: searchstore.Query{Reporter: new("example"), Archived: "all"}, want: "[wi_first_1]"},
		{name: "reporter archived", setup: `UPDATE issues SET archived=true WHERE organization_id='org_first' AND native_id='wi_first_1'; UPDATE issues SET actor_principal_id='other-principal' WHERE organization_id='org_first' AND native_id='wi_first_2'`, org: "org_first", docs: []searchstore.Document{searchIssue("org_first", "wi_first_1"), searchIssue("org_first", "wi_first_2")}, query: searchstore.Query{Reporter: new("example")}, want: "[]"},
		{name: "deleted project", after: `UPDATE projects SET deleted_at=CURRENT_TIMESTAMP WHERE organization_id='org_first' AND id='prj_second'`, org: "org_first", docs: []searchstore.Document{projectIssue("org_first", "prj_second", "wi_second_1")}, query: searchstore.Query{Kind: "work", ProjectID: "prj_second"}, want: "[]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSearchFixture(t)
			if test.setup != "" {
				if _, err := f.admin.ExecContext(t.Context(), test.setup); err != nil {
					t.Fatal(err)
				}
			}
			for _, d := range test.docs {
				f.put(t, d, searchstore.Text{Title: "sharedscopeterm"}, 0)
				f.drain(t, d.OrganizationID)
			}
			if test.after != "" {
				if _, err := f.admin.ExecContext(t.Context(), test.after); err != nil {
					t.Fatal(err)
				}
				f.drain(t, test.org)
			}
			test.query.Text = "sharedscopeterm"
			page := f.find(t, test.org, test.query)
			for _, hit := range page.Items {
				if hit.Document.OrganizationID != test.org {
					t.Fatalf("foreign organization document %+v", hit.Document)
				}
			}
			if got := fmt.Sprint(searchIDs(page)); got != test.want {
				t.Fatalf("visible results=%s want %s", got, test.want)
			}
		})
	}
}

func TestSearchReporterWindowIgnoresOtherReporters(t *testing.T) {
	f := newSearchFixture(t)
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
		t.Fatal(err)
	}
	d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	var body string
	if err := f.admin.QueryRowContext(t.Context(), `SELECT prosrc FROM pg_proc WHERE pronamespace='public'::regnamespace AND proname='search_index_candidates'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	_, body, _ = strings.Cut(body, "RETURN QUERY")
	body = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(body), "END"), ";")
	body = strings.TrimSuffix(strings.TrimSpace(body), ";")
	work := map[int]float64{}
	seeded := 0
	for _, size := range []int{searchstore.CandidateLimit + 1, 3 * searchstore.CandidateLimit} {
		seedOtherReporterHistory(t, f, d, seeded+1, size)
		seeded = size
		if got := searchIDs(f.find(t, "org_first", searchstore.Query{Text: "commonterm", Reporter: new("example-principal")})); fmt.Sprint(got) != "[wi_first_1]" {
			t.Fatalf("other reporters=%d displaced reporter result: %v", size, got)
		}
		tx, err := f.admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `SET LOCAL ROLE detent_platform_v2; SELECT set_config('detent.organization_id','org_first',true)`); err != nil {
			t.Fatal(err)
		}
		expression := strings.Join(searchstore.Expressions("org_first", searchstore.Query{ProjectID: "prj_first", Text: "commonterm"}), " | ")
		args := []any{expression, "prj_first", []string{}, false, "", "work", "", "", "false", true, "example-principal", pgstoretest.HistoryAt(), "", int64(0),
			searchstore.CandidateTerms("org_first", searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "commonterm"}), false,
			[]string{}, []string{}, []float64{}, []string{}, []int64{}, []bool{}}
		var raw []byte
		err = tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+body, args...).Scan(&raw)
		if rollback := tx.Rollback(); err == nil {
			err = rollback
		}
		if err != nil {
			t.Fatal(err)
		}
		var plan []struct{ Plan pgstoretest.SearchPlanNode }
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		work[size] = plan[0].Plan.DocumentWork()
	}
	if work[searchstore.CandidateLimit+1] != work[3*searchstore.CandidateLimit] || work[3*searchstore.CandidateLimit] > searchstore.CandidateLimit {
		t.Fatalf("reporter candidate work grew with other reporters: %v", work)
	}
}

func TestSearchRebuildMarkersKeepDocumentsFindable(t *testing.T) {
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "unrelated"}, 0)
			f.put(t, searchIssue(org, "wi_first_2"), searchstore.Text{Title: "rebuildmarkerterm", Body: postingScopeBody}, 0)
			f.drain(t, org)
			if _, err := f.admin.ExecContext(t.Context(), `DELETE FROM search_postings WHERE organization_id=$1`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,principal_id,thread,impact,updated_at,updated_at_submicrosecond)
 SELECT d.organization_id,k.key,m.term,d.id,COALESCE(i.actor_principal_id,''),false,0,d.updated_at,d.updated_at_submicrosecond
 FROM search_documents d JOIN issues i ON i.organization_id=d.organization_id AND i.project_id=d.project_id AND i.native_id=d.work_item_id
 CROSS JOIN LATERAL public.search_scope_keys(d.project_id,d.work_item_id,d.kind,d.scope,d.status,i.archived,COALESCE(i.actor_principal_id,'')) k(key)
 CROSS JOIN (VALUES ('~all'),('~pending')) m(term) WHERE d.organization_id=$1 AND d.kind='issue'`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id) VALUES($1,'prj_first','',false,'')`, org); err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"rebuilding", "rebuilt"} {
				if state == "rebuilt" {
					f.drain(t, org)
				}
				if got := fmt.Sprint(searchIDs(f.find(t, org, searchstore.Query{Text: "rebuildmarkerterm"}))); got != "[wi_first_2]" {
					t.Fatalf("%s search=%s", state, got)
				}
			}
			var pending, terms int
			if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE term='~pending'),count(*) FILTER(WHERE left(term,1)<>'~') FROM search_postings WHERE organization_id=$1`, org).Scan(&pending, &terms); err != nil || pending != 0 || terms == 0 {
				t.Fatalf("rebuilt postings pending=%d terms=%d error=%v", pending, terms, err)
			}
		})
	}
}

func TestSearchWorkItemFilterIgnoresSameReporterCrowd(t *testing.T) {
	f := newSearchFixture(t)
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
		t.Fatal(err)
	}
	d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	seedReporterHistory(t, f, d, "example-principal", 1, searchstore.CandidateLimit+1)
	for _, reporter := range []bool{false, true} {
		t.Run(fmt.Sprintf("reporter=%t", reporter), func(t *testing.T) {
			query := searchstore.Query{Text: "commonterm", WorkItemID: "wi_first_1"}
			if reporter {
				query.Reporter = new("example-principal")
			}
			if got := fmt.Sprint(searchIDs(f.find(t, "org_first", query))); got != "[wi_first_1]" {
				t.Fatalf("work item search=%s", got)
			}
			if work := searchHelperWork(t, f, "commonterm", "wi_first_1", reporter, "example-principal"); work > searchstore.CandidateLimit {
				t.Fatalf("work item candidate work=%.0f", work)
			}
		})
	}
}

func searchHelperWork(t *testing.T, f searchFixture, text, item string, reporter bool, principal string) float64 {
	t.Helper()
	var body string
	if err := f.admin.QueryRowContext(t.Context(), `SELECT prosrc FROM pg_proc WHERE pronamespace='public'::regnamespace AND proname='search_index_candidates'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	_, body, _ = strings.Cut(body, "RETURN QUERY")
	body = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(body), "END"), ";")
	body = strings.TrimSuffix(strings.TrimSpace(body), ";")
	tx, err := f.admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `SET LOCAL ROLE detent_platform_v2; SELECT set_config('detent.organization_id','org_first',true)`); err != nil {
		t.Fatal(err)
	}
	query := searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: text}
	args := []any{strings.Join(searchstore.Expressions("org_first", query), " | "), "prj_first", []string{}, false, item, "work", "", "", "false", reporter, principal, pgstoretest.HistoryAt(), "", int64(0),
		searchstore.CandidateTerms("org_first", query), false, []string{}, []string{}, []float64{}, []string{}, []int64{}, []bool{}}
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+body, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct{ Plan pgstoretest.SearchPlanNode }
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	return plan[0].Plan.DocumentWork()
}

func TestSearchWorkItemPagesIgnoreOutOfFilterPopulation(t *testing.T) {
	pages := map[bool][]string{}
	for _, crowded := range []bool{false, true} {
		f := newSearchFixture(t)
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
			t.Fatal(err)
		}
		d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "commonterm"}, 0)
		f.drain(t, "org_first")
		if crowded {
			seedReporterHistory(t, f, d, "example-principal", 1, searchstore.CandidateLimit+1)
		}
		pages[crowded] = searchPages(t, f, "org_first", searchstore.Query{Text: "commonterm", WorkItemID: "wi_first_1", Reporter: new("example-principal"), Limit: 1})
	}
	if !slices.Equal(pages[false], pages[true]) || len(pages[false]) == 0 {
		t.Fatalf("pages without out-of-filter population=%q with=%q", pages[false], pages[true])
	}
}

func TestSearchVisibilityChangeBetweenPages(t *testing.T) {
	for _, test := range []struct {
		name, change string
		changed      []string
	}{
		{"reporter reassignment", `UPDATE issues SET actor_principal_id='other-principal' WHERE organization_id='org_first' AND native_id IN ('wi_stale_3','wi_stale_4')`, []string{"wi_stale_3", "wi_stale_4"}},
		{"archival", `UPDATE issues SET archived=true WHERE organization_id='org_first' AND native_id IN ('wi_stale_3','wi_stale_4')`, []string{"wi_stale_3", "wi_stale_4"}},
		{"project deletion", `UPDATE projects SET deleted_at=CURRENT_TIMESTAMP WHERE organization_id='org_first' AND id='prj_first'`, []string{"wi_stale_1", "wi_stale_2", "wi_stale_3", "wi_stale_4"}},
	} {
		for _, rebuilding := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rebuilding=%t", test.name, rebuilding), func(t *testing.T) {
				results := map[bool][]string{}
				cursors := map[bool][]string{}
				for _, refreshed := range []bool{false, true} {
					f := newSearchFixture(t)
					seedStaleVisibilityIssues(t, f, rebuilding)
					query := searchstore.Query{Text: "staleterm", Reporter: new("example-principal"), Limit: 2}
					first := f.find(t, "org_first", query)
					if len(first.Items) != 2 || first.NextCursor == "" {
						t.Fatalf("first page=%v cursor=%q", searchIDs(first), first.NextCursor)
					}
					if _, err := f.admin.ExecContext(t.Context(), test.change); err != nil {
						t.Fatal(err)
					}
					if refreshed {
						f.drain(t, "org_first")
					}
					query.Cursor = first.NextCursor
					for page := 0; page < 10 && query.Cursor != ""; page++ {
						next := f.find(t, "org_first", query)
						for _, id := range searchIDs(next) {
							if slices.Contains(test.changed, id) {
								t.Fatalf("refreshed=%t returned changed document %s", refreshed, id)
							}
							results[refreshed] = append(results[refreshed], id)
						}
						for _, entry := range cursorDocuments(t, next.NextCursor) {
							id, _, _ := strings.Cut(entry, ":")
							if id != "" && !strings.HasPrefix(id, "issue/wi_stale_") && !strings.HasPrefix(id, "issue/wi_first_") {
								t.Fatalf("cursor names a document never visible to the caller: %s", id)
							}
						}
						cursors[refreshed] = append(cursors[refreshed], fmt.Sprint(cursorDocuments(t, next.NextCursor)))
						query.Cursor = next.NextCursor
					}
				}
				slices.Sort(results[false])
				slices.Sort(results[true])
				if !slices.Equal(results[false], results[true]) {
					t.Fatalf("stale results=%v refreshed results=%v", results[false], results[true])
				}
				t.Logf("stale cursor documents=%v refreshed cursor documents=%v", cursors[false], cursors[true])
			})
		}
	}
}

func TestSearchCursorRejectedAfterAccessRevocation(t *testing.T) {
	f := newSearchFixture(t)
	seedStaleVisibilityIssues(t, f, false)
	query := searchstore.Query{Text: "staleterm", ProjectIDs: []tracker.ProjectID{"prj_first"}, Limit: 2}
	first := f.find(t, "org_first", query)
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page=%v", searchIDs(first))
	}
	query.ProjectIDs, query.Cursor = []tracker.ProjectID{"prj_second"}, first.NextCursor
	query.Kind, query.ProjectID, query.Limit = "work", "prj_first", 20
	if _, err := f.content.Find(t.Context(), f.repository, "org_first", query); !errors.Is(err, corestore.ErrInvalidCursor) {
		t.Fatalf("revoked cursor error=%v", err)
	}
	query.Cursor = ""
	page, err := f.content.Find(t.Context(), f.repository, "org_first", query)
	if err != nil || len(page.Items) != 0 || len(cursorDocuments(t, page.NextCursor)) != 0 {
		t.Fatalf("revoked project page=%v cursor=%q err=%v", searchIDs(page), page.NextCursor, err)
	}
}

func seedStaleVisibilityIssues(t *testing.T, f searchFixture, rebuilding bool) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title,actor_principal_id) SELECT 'org_first','wi_stale_'||n,'prj_first',n+10,'staleterm','example-principal' FROM generate_series(1,4) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id='org_first' AND project_id='prj_first'`); err != nil {
		t.Fatal(err)
	}
	for index, item := range []string{"wi_first_1", "wi_first_2", "wi_stale_1", "wi_stale_2", "wi_stale_3", "wi_stale_4"} {
		f.put(t, searchIssue("org_first", item), searchstore.Text{Title: "staleterm " + strings.Repeat("staleterm ", index), Body: postingScopeBody}, 0)
	}
	f.drain(t, "org_first")
	if !rebuilding {
		return
	}
	if _, err := f.admin.ExecContext(t.Context(), `DELETE FROM search_postings WHERE organization_id='org_first' AND left(term,1)<>'~'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,principal_id,impact,updated_at,updated_at_submicrosecond)
 SELECT organization_id,scope,'~pending',document_id,principal_id,0,updated_at,updated_at_submicrosecond FROM search_postings WHERE organization_id='org_first' AND term='~all' ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id) VALUES('org_first','prj_first','',false,'') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
}

func searchPages(t *testing.T, f searchFixture, org tracker.OrganizationID, query searchstore.Query) []string {
	t.Helper()
	var pages []string
	for page := 0; page < 10; page++ {
		next := f.find(t, org, query)
		pages = append(pages, fmt.Sprintf("%v|%v", searchIDs(next), cursorDocuments(t, next.NextCursor)))
		if next.NextCursor == "" {
			break
		}
		query.Cursor = next.NextCursor
	}
	return pages
}

func cursorDocuments(t *testing.T, cursor string) []string {
	t.Helper()
	if cursor == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var position struct {
		ID      string
		BatchID string
		Streams []struct {
			ID   string
			Done bool
		}
	}
	if err := json.Unmarshal(raw, &position); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, id := range []string{position.ID, position.BatchID} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	for _, stream := range position.Streams {
		ids = append(ids, fmt.Sprintf("%s:%t", stream.ID, stream.Done))
	}
	return ids
}

func TestDuplicateSourceReadinessIsProjectWideDegradedSignal(t *testing.T) {
	if fields := reflect.TypeFor[searchstore.DuplicateSourceState]().NumField(); fields != 2 {
		t.Fatalf("duplicate source state has %d fields, want only project-wide Sequence and Ready", fields)
	}
	for _, pending := range []struct{ name, setup string }{
		{"own reporter", ""},
		{"other reporter", `UPDATE issues SET actor_principal_id='other-principal' WHERE organization_id='org_first' AND native_id='wi_first_2'`},
		{"archived item", `UPDATE issues SET archived=true WHERE organization_id='org_first' AND native_id='wi_first_2'`},
	} {
		t.Run(pending.name, func(t *testing.T) {
			f := newSearchFixture(t)
			if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id='org_first' AND project_id='prj_first'`); err != nil {
				t.Fatal(err)
			}
			if pending.setup != "" {
				if _, err := f.admin.ExecContext(t.Context(), pending.setup); err != nil {
					t.Fatal(err)
				}
			}
			f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "readinessterm"}, 0)
			d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "readinessterm"}, 0)
			f.drain(t, "org_first")
			if state := duplicateState(t, f, "org_first"); !state.Ready {
				t.Fatalf("drained state=%+v", state)
			}
			d.Revision++
			f.put(t, d, searchstore.Text{Title: "readinessterm changed"}, d.Revision-1)
			if state := duplicateState(t, f, "org_first"); state.Ready {
				t.Fatalf("pending %s item left project readiness=%+v", pending.name, state)
			}
		})
	}
}

func TestSearchThreadScopeKeysByProjectAccess(t *testing.T) {
	f := newSearchFixture(t)
	for _, d := range []searchstore.Document{
		{OrganizationID: "org_first", ID: "thread/general", SourceID: "thread/general", Kind: "thread", Scope: "general", Status: "open", Revision: 1},
		{OrganizationID: "org_first", ID: "thread/first-project", SourceID: "thread/first-project", Kind: "thread", Scope: "project", Status: "open", ProjectID: "prj_first", Revision: 1},
		{OrganizationID: "org_first", ID: "thread/first-issue", SourceID: "thread/first-issue", Kind: "thread", Scope: "issue", Status: "open", ProjectID: "prj_first", WorkItemID: "wi_first_1", Revision: 1},
		{OrganizationID: "org_first", ID: "thread/second-project", SourceID: "thread/second-project", Kind: "thread", Scope: "project", Status: "open", ProjectID: "prj_second", Revision: 1},
		{OrganizationID: "org_first", ID: "thread/second-issue", SourceID: "thread/second-issue", Kind: "thread", Scope: "issue", Status: "open", ProjectID: "prj_second", WorkItemID: "wi_second_1", Revision: 1},
	} {
		f.put(t, d, searchstore.Text{Title: "threadscopeterm"}, 0)
	}
	f.drain(t, "org_first")
	for _, test := range []struct {
		name     string
		projects []tracker.ProjectID
		want     []string
	}{
		{"member of every project", nil, []string{"thread/first-issue", "thread/first-project", "thread/general", "thread/second-issue", "thread/second-project"}},
		{"member of one project", []tracker.ProjectID{"prj_first"}, []string{"thread/first-issue", "thread/first-project", "thread/general"}},
		{"member without project access", []tracker.ProjectID{}, []string{"thread/general"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := searchstore.Query{Kind: "thread", Text: "threadscopeterm", ProjectIDs: test.projects, Limit: 1}
			var got []string
			for page := 0; page < 10; page++ {
				next := f.find(t, "org_first", query)
				got = append(got, searchIDs(next)...)
				for _, entry := range cursorDocuments(t, next.NextCursor) {
					id, _, _ := strings.Cut(entry, ":")
					if id != "" && !slices.Contains(test.want, id) {
						t.Fatalf("cursor names thread %s outside access", id)
					}
				}
				if next.NextCursor == "" {
					break
				}
				query.Cursor = next.NextCursor
			}
			slices.Sort(got)
			if !slices.Equal(got, test.want) {
				t.Fatalf("threads=%v want %v", got, test.want)
			}
		})
	}
}

func projectIssue(org tracker.OrganizationID, project tracker.ProjectID, item string) searchstore.Document {
	d := searchIssue(org, item)
	d.ProjectID = project
	return d
}

func seedOtherReporterHistory(t *testing.T, f searchFixture, d searchstore.Document, start, end int) {
	t.Helper()
	seedReporterHistory(t, f, d, "other-principal", start, end)
}

func seedReporterHistory(t *testing.T, f searchFixture, d searchstore.Document, principal string, start, end int) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title,actor_principal_id)
 SELECT 'org_first','wi_reporter_'||n,'prj_first',n+10,'commonterm',$3 FROM generate_series($1::int,$2::int) n`, start, end, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents DISABLE TRIGGER search_document_postings`); err != nil {
		t.Fatal(err)
	}
	_, insertErr := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,indexed_revision,title,scope,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,vector)
 SELECT 'org_first','reporter_'||n,'prj_first','wi_reporter_'||n,'issue','wi_reporter_'||n,1,1,'commonterm','issue',CURRENT_TIMESTAMP,$1,$2,$3,$4,$5::tsvector FROM generate_series($6::int,$7::int) n`, d.Payload.Key, d.Payload.SHA256, d.Payload.CompressedSize, d.Payload.UncompressedSize, searchstore.Vector(d, searchstore.Text{Title: "commonterm"}), start, end)
	if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents ENABLE TRIGGER search_document_postings`); err != nil {
		t.Fatal(err)
	}
	if insertErr != nil {
		t.Fatal(insertErr)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,principal_id,impact,updated_at,updated_at_submicrosecond)
 SELECT d.organization_id,public.search_scope_key(d.project_id,'','work','','','false',''),p.term,d.id,$4,100,d.updated_at,d.updated_at_submicrosecond
 FROM search_documents d CROSS JOIN (VALUES ($1::text),('~all')) p(term) WHERE d.organization_id='org_first' AND d.id IN (SELECT 'reporter_'||n FROM generate_series($2::int,$3::int) n)`, searchstore.Prefix("org_first", "prj_first")+"wcommonterm", start, end, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), "VACUUM (ANALYZE) search_documents, search_postings, issues"); err != nil {
		t.Fatal(err)
	}
}

func seedForeignIssueIndex(t *testing.T, f searchFixture, exemplar searchstore.Document, start, end int) {
	t.Helper()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title,actor_principal_id)
 SELECT $1,'wi_foreign_'||n,'prj_second',n+10,'Repository association','other-principal' FROM generate_series($2::int,$3::int) n`, org, start, end); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents DISABLE TRIGGER search_document_postings`); err != nil {
			t.Fatal(err)
		}
		_, insertErr := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,indexed_revision,title,scope,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,vector)
 SELECT $1,'foreign_'||n,'prj_second','wi_foreign_'||n,'issue','wi_foreign_'||n,1,1,'Repository association','issue',CURRENT_TIMESTAMP,$4,$5,$6,$7,''::tsvector FROM generate_series($2::int,$3::int) n`, org, start, end, exemplar.Payload.Key, exemplar.Payload.SHA256, exemplar.Payload.CompressedSize, exemplar.Payload.UncompressedSize)
		if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents ENABLE TRIGGER search_document_postings`); err != nil {
			t.Fatal(err)
		}
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		words := strings.Fields(strings.ToLower(strings.NewReplacer(".", "", ",", "").Replace(postingScopeBody)))
		if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,principal_id,impact,updated_at,updated_at_submicrosecond)
 SELECT d.organization_id,public.search_scope_key('prj_second','','work','','','false',''),$4||w.term,d.id,'other-principal',0.01,d.updated_at,d.updated_at_submicrosecond
 FROM search_documents d CROSS JOIN (SELECT DISTINCT unnest($5::text[]) AS term) w
 WHERE d.organization_id=$1 AND d.id IN (SELECT 'foreign_'||n FROM generate_series($2::int,$3::int) n)`, org, start, end, searchstore.Prefix(org, "prj_second")+"w", words); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.admin.ExecContext(t.Context(), "VACUUM (ANALYZE) search_documents, search_postings, issues"); err != nil {
		t.Fatal(err)
	}
}
