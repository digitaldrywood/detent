package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/testenv/searchtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchExistingTeamThreadFilters(t *testing.T) {
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for index, d := range []searchstore.Document{
			{ID: "first", SourceID: "first", Scope: "general", Status: "open"},
			{ID: "middle", SourceID: "middle", Scope: "project", ProjectID: "prj_first", Status: "open"},
			{ID: "last", SourceID: "last", Scope: "issue", ProjectID: "prj_first", WorkItemID: "wi_first_1", Status: "resolved"},
		} {
			d.OrganizationID, d.Kind, d.Revision, d.UpdatedAt = org, "thread", 1, historyAt().Add(time.Duration(index)*time.Nanosecond)
			f.put(t, d, searchstore.Text{Title: "Shared research"}, 0)
		}
	}
	for _, mode := range []string{"pending", "indexed"} {
		if mode == "indexed" {
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				f.drain(t, org)
			}
		}
		for _, test := range searchtest.ThreadFilters("prj_first", "wi_first_1", "first", "middle", "last") {
			t.Run(mode+"/"+test.Name, func(t *testing.T) {
				params, err := url.ParseQuery(test.Query)
				if err != nil {
					t.Fatal(err)
				}
				q := searchstore.Query{Kind: "thread", Scope: params.Get("scope"), ProjectID: tracker.ProjectID(params.Get("project_id")), WorkItemID: tracker.NativeWorkItemID(params.Get("issue_id")), Status: params.Get("status"), Text: params.Get("search"), Limit: 1}
				for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
					q.Cursor = ""
					var ids []string
					for {
						page := f.find(t, org, q)
						ids = append(ids, searchIDs(page)...)
						if page.NextCursor == "" {
							break
						}
						q.Cursor = page.NextCursor
					}
					if !slices.Equal(ids, test.Want) {
						t.Fatalf("thread filters=%v want=%v", ids, test.Want)
					}
				}
			})
		}
	}
	for _, test := range []struct {
		name     string
		projects []tracker.ProjectID
		want     []string
	}{
		{"all grants", nil, []string{"last", "middle", "first"}},
		{"general only", []tracker.ProjectID{}, []string{"first"}},
		{"foreign grant", []tracker.ProjectID{"prj_second"}, []string{"first"}},
		{"own grant", []tracker.ProjectID{"prj_first"}, []string{"last", "middle", "first"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				page := f.find(t, org, searchstore.Query{Kind: "thread", ProjectIDs: test.projects, Text: "shared"})
				if !slices.Equal(searchIDs(page), test.want) {
					t.Fatalf("tenant=%s grant filters=%v want=%v", org, searchIDs(page), test.want)
				}
			}
		})
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE projects SET deleted_at=$1 WHERE organization_id='org_first' AND id='prj_first'`, historyAt()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		org  tracker.OrganizationID
		want []string
	}{{"org_first", []string{"first"}}, {"org_second", []string{"last", "middle", "first"}}} {
		page := f.find(t, test.org, searchstore.Query{Kind: "thread", Text: "shared"})
		if !slices.Equal(searchIDs(page), test.want) {
			t.Fatalf("tenant=%s deleted-project visibility=%v want=%v", test.org, searchIDs(page), test.want)
		}
	}
}

func TestSearchCommentEventAndChange(t *testing.T) {
	f := newSearchFixture(t)
	org := tracker.OrganizationID("org_first")
	history := seedHistory(t, f.repository, org, "search")
	for _, test := range []struct{ kind, source, text string }{
		{"comment", "comment_first_1_1", "Unicorns decorate mock fixture data"},
		{"event", history.event.ID, "Example event transition"},
		{"change", history.change.ID, "Example change discussion"},
	} {
		d := searchIssue(org, "wi_first_1")
		d.ID, d.Kind, d.SourceID = test.kind+"/"+test.source, test.kind, test.source
		f.put(t, d, searchstore.Text{Body: test.text}, 0)
	}
	for _, mode := range []string{"pending", "indexed"} {
		if mode == "indexed" {
			f.drain(t, org)
		}
		for _, query := range []string{"unicorn", "transition", "discussion", "example unicorn"} {
			page := f.find(t, org, searchstore.Query{Text: query})
			if !slices.Equal(searchIDs(page), []string{"wi_first_1"}) {
				t.Fatalf("%s %s lost indexed work history: %+v", mode, query, page)
			}
		}
	}
	err := f.repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
		c, err := w.Comment(t.Context(), "prj_first", "wi_first_1", "comment_first_1_1")
		if err != nil {
			return err
		}
		c.Deleted = true
		_, err = w.UpdateComment(t.Context(), c)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if page := f.find(t, org, searchstore.Query{Text: "unicorn"}); len(page.Items) != 0 {
		t.Fatal("deleted comment remains searchable")
	}
	f.drain(t, org)
	if page := f.find(t, org, searchstore.Query{Text: "unicorn"}); len(page.Items) != 0 {
		t.Fatal("refreshed index restored deleted comment text")
	}
	err = f.repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
		return w.DeleteComment(t.Context(), "prj_first", "wi_first_1", "comment_first_1_1")
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents WHERE kind='comment'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted source projection count=%d err=%v", count, err)
	}
}

func TestSearchThreadCursorContinuesBeyondCandidateBudget(t *testing.T) {
	started := time.Now()
	f := newSearchFixture(t)
	t.Logf("thread fixture setup=%s", time.Since(started))
	started = time.Now()
	org := tracker.OrganizationID("org_first")
	documents := make([]searchstore.Document, 0, searchstore.CandidateLimit+2)
	for index := range searchstore.CandidateLimit + 2 {
		id := fmt.Sprintf("thread_%06d", index)
		d, err := f.content.Prepare(t.Context(), searchstore.Document{OrganizationID: org, ID: id, SourceID: id, Kind: "thread", Scope: "general", Status: "open", Revision: 1, UpdatedAt: historyAt().Add(time.Duration(index) * time.Nanosecond)}, searchstore.Text{Title: "Shared research", Body: "unsearchablepreview"})
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, d)
	}
	t.Logf("thread payload preparation count=%d elapsed=%s", len(documents), time.Since(started))
	started = time.Now()
	if err := bulkThreadCursorSeed(t.Context(), f, org, documents); err != nil {
		t.Fatal(err)
	}
	t.Logf("thread pending seed count=%d elapsed=%s", len(documents), time.Since(started))
	for _, document := range []searchstore.Document{documents[0], documents[len(documents)-1]} {
		assertThreadCursorPublication(t, f, document, false)
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			started = time.Now()
			if err := bulkThreadCursorPublication(t.Context(), f, org, len(documents)); err != nil {
				t.Fatal(err)
			}
			t.Logf("thread indexed seed count=%d elapsed=%s", len(documents), time.Since(started))
			for _, document := range []searchstore.Document{documents[0], documents[len(documents)-1]} {
				assertThreadCursorPublication(t, f, document, true)
			}
		}

		for _, text := range []string{"", "shared"} {
			t.Run(fmt.Sprintf("indexed=%v/%s", indexed, text), func(t *testing.T) {
				query := searchstore.Query{Kind: "thread", Text: text, Limit: 200}
				assertThreadCandidatePointLookups(t, f, org, query, len(documents))
				count := 0
				for {
					page := f.find(t, org, query)
					for _, hit := range page.Items {
						want := fmt.Sprintf("thread_%06d", len(documents)-1-count)
						if hit.Document.ID != want {
							t.Fatalf("thread %d=%s want=%s", count, hit.Document.ID, want)
						}
						count++
					}
					if page.NextCursor == "" {
						break
					}
					query.Cursor = page.NextCursor
				}
				if count != len(documents) {
					t.Fatalf("thread cursor stopped at %d want=%d", count, len(documents))
				}
			})
		}
	}
	if page := f.find(t, org, searchstore.Query{Kind: "thread", Text: "unsearchablepreview"}); len(page.Items) != 0 {
		t.Fatal("thread preview was indexed")
	}
}

func TestSearchTargetKeysetSurvivesEarlierDeletion(t *testing.T) {
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, id := range []string{"a", "b", "c", "d"} {
			f.put(t, searchstore.Document{OrganizationID: org, ID: id, SourceID: id, Kind: "thread", Scope: "general", Status: "open", Revision: 1, UpdatedAt: historyAt()}, searchstore.Text{Title: "keyset sentinel"}, 0)
		}
		f.drain(t, org)
		q := searchstore.Query{Kind: "thread", Text: "keyset", Limit: 1}
		first := f.find(t, org, q)
		if !slices.Equal(searchIDs(first), []string{"d"}) || first.NextCursor == "" {
			t.Fatalf("first page=%+v", first)
		}
		if err := f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error {
			return w.DeleteSearch(t.Context(), "d", 1)
		}); err != nil {
			t.Fatal(err)
		}
		q.Cursor, q.Limit = first.NextCursor, 2
		second := f.find(t, org, q)
		if !slices.Equal(searchIDs(second), []string{"c", "b"}) || second.NextCursor == "" {
			t.Fatalf("deleting earlier row shifted keyset: %+v", second)
		}
		q.Cursor = second.NextCursor
		last := f.find(t, org, q)
		if !slices.Equal(searchIDs(last), []string{"a"}) || last.NextCursor != "" {
			t.Fatalf("last page=%+v", last)
		}
	}
}

func bulkThreadCursorSeed(ctx context.Context, f searchFixture, organization tracker.OrganizationID, documents []searchstore.Document) error {
	first := documents[0]
	if err := f.repository.WriteSearch(ctx, organization, corestore.Background, func(w searchstore.Writer) error {
		return w.PutSearch(ctx, first, 0)
	}); err != nil {
		return err
	}
	ids, keys, hashes := make([]string, 0, len(documents)), make([]string, 0, len(documents)), make([]string, 0, len(documents))
	updated := make([]time.Time, 0, len(documents))
	sub, compressed, raw := make([]int64, 0, len(documents)), make([]int64, 0, len(documents)), make([]int64, 0, len(documents))
	for _, d := range documents[1:] {
		if d.Validate() != nil || d.Payload.Validate(string(organization)) != nil || d.OrganizationID != organization || d.Kind != "thread" || d.Scope != "general" || d.Status != "open" || d.ProjectID != "" || d.WorkItemID != "" || d.SourceID != d.ID || d.Revision != 1 || d.Title != first.Title || !reflect.DeepEqual(d.Source, first.Source) {
			return errors.New("thread fixture metadata is not uniform")
		}
		ids = append(ids, d.ID)
		keys = append(keys, d.Payload.Key)
		hashes = append(hashes, d.Payload.SHA256)
		updated = append(updated, d.UpdatedAt.Truncate(time.Microsecond))
		sub = append(sub, int64(d.UpdatedAt.Nanosecond()%1000))
		compressed = append(compressed, d.Payload.CompressedSize)
		raw = append(raw, d.Payload.UncompressedSize)
	}
	return withPlanSeedTransaction(ctx, f.admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "ALTER TABLE search_documents DISABLE TRIGGER search_document_postings"); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `INSERT INTO search_documents(organization_id,id,kind,source_id,revision,title,scope,status,updated_at,updated_at_submicrosecond,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,source_vector,token_count,terms,term_frequencies)
 SELECT $1,d.id,'thread',d.id,1,$2,'general','open',d.at,d.sub,d.key,d.sha,d.compressed,d.raw,$3::tsvector,$4,$5::text[],$6::bigint[] FROM unnest($7::text[],$8::timestamptz[],$9::bigint[],$10::text[],$11::text[],$12::bigint[],$13::bigint[])d(id,at,sub,key,sha,compressed,raw)`, organization, first.Title, first.Source.Vector, first.Source.Statistics.Tokens, first.Source.Statistics.Terms, first.Source.Statistics.Frequencies, ids, updated, sub, keys, hashes, compressed, raw)
		if err != nil {
			return err
		}
		if result.RowsAffected() != int64(len(documents)-1) {
			return errors.New("thread fixture cardinality changed")
		}
		if err := copyThreadCursorPostings(ctx, tx, organization, first.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "ALTER TABLE search_documents ENABLE TRIGGER search_document_postings"); err != nil {
			return err
		}
		var total, pending, all int
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM search_documents WHERE organization_id=$1 AND kind='thread'),(SELECT count(DISTINCT document_id) FROM search_postings WHERE organization_id=$1 AND term='~pending'),(SELECT count(DISTINCT document_id) FROM search_postings WHERE organization_id=$1 AND term='~all'),(SELECT tgenabled='O' FROM pg_trigger WHERE tgrelid='search_documents'::regclass AND tgname='search_document_postings')`, organization).Scan(&total, &pending, &all, &enabled); err != nil {
			return err
		}
		if total != len(documents) || pending != len(documents) || all != len(documents) || !enabled {
			return errors.New("thread fixture pending markers changed")
		}
		return nil
	})
}

func bulkThreadCursorPublication(ctx context.Context, f searchFixture, organization tracker.OrganizationID, want int) error {
	if _, err := f.admin.ExecContext(ctx, "ANALYZE search_postings"); err != nil {
		return err
	}
	more, err := f.content.RefreshOne(ctx, f.repository, organization)
	if err != nil {
		return err
	}
	if !more {
		return errors.New("thread publication exemplar missing")
	}
	return withPlanSeedTransaction(ctx, f.admin, func(tx pgx.Tx) error {
		var exemplar string
		if err := tx.QueryRow(ctx, `SELECT id FROM search_documents WHERE organization_id=$1 AND kind='thread' AND indexed_revision=revision ORDER BY id LIMIT 1`, organization).Scan(&exemplar); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "ALTER TABLE search_documents DISABLE TRIGGER search_document_postings"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM search_postings p USING search_documents d WHERE d.organization_id=$1 AND d.kind='thread' AND d.id<>$2 AND p.organization_id=d.organization_id AND p.document_id=d.id`, organization, exemplar); err != nil {
			return err
		}
		if err := copyThreadCursorPostings(ctx, tx, organization, exemplar); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE search_documents d SET refresh_order=nextval('search_refresh_order'),vector=seed.vector,source_vector=seed.source_vector,indexed_revision=d.revision,token_count=seed.token_count,terms=seed.terms,term_frequencies=seed.term_frequencies,aggregate_object_key=seed.aggregate_object_key,aggregate_sha256=seed.aggregate_sha256,aggregate_bytes=seed.aggregate_bytes,aggregate_content_bytes=seed.aggregate_content_bytes FROM search_documents seed WHERE d.organization_id=$1 AND seed.organization_id=d.organization_id AND seed.id=$2 AND d.kind='thread' AND d.id<>seed.id`, organization, exemplar)
		if err != nil {
			return err
		}
		if result.RowsAffected() != int64(want-1) {
			return errors.New("thread publication cardinality changed")
		}
		if _, err := tx.Exec(ctx, "ALTER TABLE search_documents ENABLE TRIGGER search_document_postings"); err != nil {
			return err
		}
		var indexed, pending, all int
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM search_documents WHERE organization_id=$1 AND kind='thread' AND indexed_revision=revision),(SELECT count(DISTINCT document_id) FROM search_postings WHERE organization_id=$1 AND term='~pending'),(SELECT count(DISTINCT document_id) FROM search_postings WHERE organization_id=$1 AND term='~all'),(SELECT tgenabled='O' FROM pg_trigger WHERE tgrelid='search_documents'::regclass AND tgname='search_document_postings')`, organization).Scan(&indexed, &pending, &all, &enabled); err != nil {
			return err
		}
		if indexed != want || pending != 0 || all != want || !enabled {
			return errors.New("thread publication markers changed")
		}
		return nil
	})
}

func assertThreadCursorPublication(t *testing.T, f searchFixture, d searchstore.Document, indexed bool) {
	t.Helper()
	snapshot := func() []string {
		var result []string
		err := withPlanSeedTransaction(t.Context(), f.admin, func(tx pgx.Tx) error {
			_, values, err := planSeedColumns(t.Context(), tx, "search_documents", map[string]string{"refresh_order": "NULL::bigint"})
			if err != nil {
				return err
			}
			var document string
			if err := tx.QueryRow(t.Context(), "SELECT ROW("+values+")::text FROM search_documents seed WHERE organization_id=$1 AND id=$2", d.OrganizationID, d.ID).Scan(&document); err != nil {
				return err
			}
			result = append(result, document)
			rows, err := tx.Query(t.Context(), `SELECT ROW(p.*)::text FROM search_postings p WHERE organization_id=$1 AND document_id=$2 ORDER BY scope,term`, d.OrganizationID, d.ID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var posting string
				if err := rows.Scan(&posting); err != nil {
					return err
				}
				result = append(result, posting)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snapshot()
	if err := f.repository.WriteSearch(t.Context(), d.OrganizationID, corestore.Background, func(w searchstore.Writer) error {
		return w.AdvanceSearch(t.Context(), d)
	}); err != nil {
		t.Fatal(err)
	}
	if indexed {
		if more, err := f.content.RefreshOne(t.Context(), f.repository, d.OrganizationID); err != nil || !more {
			t.Fatalf("owner thread publication: more=%t err=%v", more, err)
		}
	}
	if after := snapshot(); !slices.Equal(before, after) {
		t.Fatalf("bulk thread publication differs from owner for %s: before=%q after=%q", d.ID, before, after)
	}
}

func copyThreadCursorPostings(ctx context.Context, tx pgx.Tx, organization tracker.OrganizationID, exemplar string) error {
	columns, values, err := planSeedColumns(ctx, tx, "search_postings", map[string]string{"document_id": "d.id", "updated_at": "d.updated_at", "updated_at_submicrosecond": "d.updated_at_submicrosecond"})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO search_postings("+columns+") SELECT "+values+" FROM search_postings seed JOIN search_documents d ON d.organization_id=seed.organization_id AND d.kind='thread' AND d.id<>seed.document_id WHERE seed.organization_id=$1 AND seed.document_id=$2", organization, exemplar)
	return err
}

func assertThreadCandidatePointLookups(t *testing.T, f searchFixture, org tracker.OrganizationID, query searchstore.Query, population int) {
	t.Helper()
	if err := withPlanSeedTransaction(t.Context(), f.admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
			return err
		}
		if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id',$1,true),set_config('jit','off',true)", org); err != nil {
			return err
		}
		probe := &searchPlanProbe{DBTX: tx}
		rows, err := pgquery.New(probe).SearchCandidates(t.Context(), pgquery.SearchCandidatesParams{OrganizationID: org, Kind: query.Kind, ProjectID: string(query.ProjectID), ProjectIds: []string{}, Expression: strings.Join(searchstore.Expressions(org, query), " | "), SearchTerms: searchstore.CandidateTerms(org, query), EmptyQuery: strings.TrimSpace(query.Text) == ""})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			t.Fatal("thread plan returned no candidates")
		}
		var raw []byte
		if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.statement, probe.args...).Scan(&raw); err != nil {
			return err
		}
		var plans []struct{ Plan searchPlanNode }
		if err := json.Unmarshal(raw, &plans); err != nil {
			return err
		}
		if len(plans) != 1 {
			t.Fatalf("thread plans=%d, want 1", len(plans))
		}
		var visits float64
		var inspect func(searchPlanNode)
		inspect = func(node searchPlanNode) {
			if node.Relation == "search_documents" {
				visits += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
			}
			for _, child := range node.Plans {
				inspect(child)
			}
		}
		inspect(plans[0].Plan)
		t.Logf("thread candidates=%d document_visits=%.0f population=%d", len(rows), visits, population)
		if visits == 0 || visits > float64(population)*32 {
			t.Errorf("thread candidate lookups visited %.0f document rows for population %d", visits, population)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
