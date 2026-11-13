package searchrecords_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

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

func projectIssue(org tracker.OrganizationID, project tracker.ProjectID, item string) searchstore.Document {
	d := searchIssue(org, item)
	d.ProjectID = project
	return d
}

func seedOtherReporterHistory(t *testing.T, f searchFixture, d searchstore.Document, start, end int) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title,actor_principal_id)
 SELECT 'org_first','wi_reporter_'||n,'prj_first',n+10,'commonterm','other-principal' FROM generate_series($1::int,$2::int) n`, start, end); err != nil {
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
 SELECT d.organization_id,public.search_scope_key(d.project_id,'','work','','','false',''),p.term,d.id,'other-principal',100,d.updated_at,d.updated_at_submicrosecond
 FROM search_documents d CROSS JOIN (VALUES ($1::text),('~all')) p(term) WHERE d.organization_id='org_first' AND d.id IN (SELECT 'reporter_'||n FROM generate_series($2::int,$3::int) n)`, searchstore.Prefix("org_first", "prj_first")+"wcommonterm", start, end); err != nil {
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
