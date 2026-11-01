package postgres_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/testenv/searchtest"
)

func TestSearchPrefixReachesEveryThread(t *testing.T) {
	f := newSearchFixture(t)
	const count = 65
	for index := range count {
		id := fmt.Sprintf("prefix_%03d", index)
		f.put(t, searchstore.Document{OrganizationID: "org_first", ID: id, SourceID: id, Kind: "thread", Scope: "general", Status: "open", Revision: 1, UpdatedAt: historyAt().Add(time.Duration(index) * time.Nanosecond)}, searchstore.Text{Title: fmt.Sprintf("prefixword%03d", index)}, 0)
	}
	f.drain(t, "org_first")
	for _, limit := range []int{7, 33, 200} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			query := searchstore.Query{Kind: "thread", Text: "prefixword", Limit: limit}
			seen := 0
			for {
				page := f.find(t, "org_first", query)
				for _, hit := range page.Items {
					want := fmt.Sprintf("prefix_%03d", count-1-seen)
					if hit.Document.ID != want {
						t.Fatalf("match %d=%s want=%s", seen, hit.Document.ID, want)
					}
					seen++
				}
				if page.NextCursor == "" {
					break
				}
				if len(page.Items) == 0 || seen >= count {
					t.Fatalf("cursor did not advance: matches=%d", seen)
				}
				query.Cursor = page.NextCursor
			}
			if seen != count {
				t.Fatalf("prefix matches=%d want=%d", seen, count)
			}
		})
	}
}

func TestSearchExistingPrefixes(t *testing.T) {
	f := newSearchFixture(t)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: strings.Repeat("a", 64) + "one café cafeteria tokenalpha tokenbeta"}, 0)
	f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: strings.Repeat("a", 64) + "two cag tokenalphabet"}, 0)
	for _, mode := range []string{"pending", "indexed"} {
		if mode == "indexed" {
			f.drain(t, "org_first")
		}
		for _, test := range searchtest.Prefixes() {
			t.Run(mode+"/"+test.Name, func(t *testing.T) {
				want := "wi_first_2"
				if test.First {
					want = "wi_first_1"
				}
				page := f.find(t, "org_first", searchstore.Query{Text: test.Query, Exact: test.Exact})
				if !slices.Equal(searchIDs(page), []string{want}) {
					t.Fatalf("matches=%v want=%s", searchIDs(page), want)
				}
			})
		}
	}
}

func TestSearchExactSnippetDoesNotHighlightLongerTokens(t *testing.T) {
	f := newSearchFixture(t)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "tokenalpha tokenalphabet"}, 0)
	for _, mode := range []string{"pending", "indexed"} {
		if mode == "indexed" {
			f.drain(t, "org_first")
		}
		page := f.find(t, "org_first", searchstore.Query{Text: "tokenalpha", Exact: true})
		if len(page.Items) != 1 || page.Items[0].Snippet != "[tokenalpha] tokenalphabet" {
			t.Fatalf("%s exact snippet=%+v", mode, page)
		}
	}
}

func TestSearchOversizedTokenRetainsPrefix(t *testing.T) {
	for _, test := range []struct{ name, suffix, boundary string }{
		{"ASCII", strings.Repeat("a", 3000), "prefixmarker" + strings.Repeat("a", 2012)},
		{"multibyte", strings.Repeat("界", 1000), "prefixmarker" + strings.Repeat("界", 670)},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSearchFixture(t)
			d := searchstore.Document{OrganizationID: "org_first", ID: "oversized", SourceID: "oversized", Kind: "thread", Scope: "general", Status: "open", Revision: 1}
			f.put(t, d, searchstore.Text{Title: "prefixmarker" + test.suffix}, 0)
			for _, mode := range []string{"pending", "indexed"} {
				t.Run(mode, func(t *testing.T) {
					if mode == "indexed" {
						f.drain(t, "org_first")
					}
					page := f.find(t, "org_first", searchstore.Query{Kind: "thread", Text: "prefixmarker"})
					if !slices.Equal(searchIDs(page), []string{"oversized"}) {
						t.Fatalf("prefix matches=%v want=[oversized]", searchIDs(page))
					}
					page = f.find(t, "org_first", searchstore.Query{Kind: "thread", Text: test.boundary, Exact: true})
					if len(page.Items) != 0 {
						t.Fatalf("truncated token matched exact boundary: %v", searchIDs(page))
					}
				})
			}
		})
	}
}

func TestSearchOlderHighImpactPrefixRanksFirst(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	seedSearchPlanHistory(t, f, d, 1, searchstore.CandidateLimit+1)
	comment := searchIssue("org_first", "wi_first_1")
	comment.ID, comment.Kind, comment.SourceID, comment.Revision = "history_1", "comment", "history_1", 2
	comment.UpdatedAt = historyAt().Add(100000 * time.Hour)
	f.put(t, comment, searchstore.Text{Title: "commonterm"}, 1)
	f.drain(t, "org_first")
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_postings SET impact=100
 WHERE organization_id='org_first' AND document_id=$1 AND term=$2`, d.ID, searchstore.Prefix("org_first", "prj_first")+"wcommonterm"); err != nil {
		t.Fatal(err)
	}
	page := f.find(t, "org_first", searchstore.Query{Text: "common", Limit: 1})
	if len(page.Items) != 1 || page.Items[0].Document.WorkItemID != "wi_first_2" {
		t.Fatalf("older high-impact prefix match did not rank first: %+v", page.Items)
	}
}

func TestSearchPrefixLexemeWindowsEmitWorkItemsOnce(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "prefixlexeme999"}, 0)
	seedSearchPlanHistory(t, f, d, 1, 129)
	for index := range 129 {
		comment := searchIssue("org_first", "wi_first_1")
		comment.ID = fmt.Sprintf("history_%d", index+1)
		comment.Kind, comment.SourceID, comment.Revision = "comment", comment.ID, 2
		f.put(t, comment, searchstore.Text{Title: fmt.Sprintf("prefixlexeme%03d", index)}, 1)
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			f.drain(t, "org_first")
		}
		t.Run(fmt.Sprintf("indexed=%v", indexed), func(t *testing.T) {
			query := searchstore.Query{Text: "prefixlexeme", Limit: 1}
			var matches []string
			for range 10 {
				page := f.find(t, "org_first", query)
				matches = append(matches, searchIDs(page)...)
				if len(page.NextCursor) > 128*1024 {
					t.Fatalf("bounded lexeme cursor=%d bytes", len(page.NextCursor))
				}
				if page.NextCursor == "" {
					slices.Sort(matches)
					if !slices.Equal(matches, []string{"wi_first_1", "wi_first_2"}) {
						t.Fatalf("lexeme-window matches=%v", matches)
					}
					return
				}
				if page.NextCursor == query.Cursor {
					t.Fatal("lexeme cursor did not advance")
				}
				query.Cursor = page.NextCursor
			}
			t.Fatal("prefix did not reach the next lexeme window")
		})
	}
}

func TestSearchCanonicalIgnoresDeletedBestDocument(t *testing.T) {
	f := newSearchFixture(t)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Body: "sentinel"}, 0)
	comment := searchIssue("org_first", "wi_first_1")
	comment.ID, comment.Kind, comment.SourceID = "comment/example", "comment", "comment_first_1_1"
	f.put(t, comment, searchstore.Text{Title: "sentinel"}, 0)
	f.drain(t, "org_first")
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_comments SET deleted=true WHERE organization_id='org_first' AND id='comment_first_1_1'`); err != nil {
		t.Fatal(err)
	}
	page := f.find(t, "org_first", searchstore.Query{Text: "sentinel"})
	if len(page.Items) != 1 || page.Items[0].Document.Kind != "issue" {
		t.Fatalf("deleted canonical comment hid its visible issue: %+v", page.Items)
	}
}

func TestSearchMixedPendingAndIndexedDocumentsEmitOnce(t *testing.T) {
	t.Run("mixed sources", func(t *testing.T) {
		f := newSearchFixture(t)
		f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "alpha"}, 0)
		f.drain(t, "org_first")
		comment := searchIssue("org_first", "wi_first_1")
		comment.ID, comment.Kind, comment.SourceID = "comment/a", "comment", "comment_first_1_1"
		f.put(t, comment, searchstore.Text{Title: "bravo"}, 0)
		query := searchstore.Query{Text: "alpha bravo", Limit: 1}
		var matches []string
		for range 10 {
			page := f.find(t, "org_first", query)
			matches = append(matches, searchIDs(page)...)
			if page.NextCursor == "" {
				if !slices.Equal(matches, []string{"wi_first_1"}) {
					t.Fatalf("mixed source matches=%v", matches)
				}
				return
			}
			query.Cursor = page.NextCursor
		}
		t.Fatal("mixed source search did not exhaust")

	})
	t.Run("exhausted pending stream", func(t *testing.T) {
		f := newSearchFixture(t)
		d := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "commonterm"}, 0)
		f.drain(t, "org_first")
		seedSearchWorkHistory(t, f, d, searchstore.CandidateLimit+1)
		f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "commonterm"}, 0)
		query := searchstore.Query{Text: "commonterm", Exact: true, Limit: 100}
		seen := make(map[string]bool)
		for range 40 {
			page := f.find(t, "org_first", query)
			for _, id := range searchIDs(page) {
				if seen[id] {
					t.Fatalf("search returned work item twice: %s", id)
				}
				seen[id] = true
			}
			if page.NextCursor == "" {
				if !seen["wi_first_1"] {
					t.Fatal("pending work item was omitted")
				}
				return
			}
			query.Cursor = page.NextCursor
		}
		t.Fatal("pending and indexed search did not exhaust")
	})
}

func TestSearchAggregateCoverageOwnsAReachableStream(t *testing.T) {
	f := newSearchFixture(t)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "alpha bravo", Body: strings.Repeat("filler ", 100)}, 0)
	f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "alpha"}, 0)
	for index, term := range []string{"alpha", "bravo"} {
		comment := searchIssue("org_first", "wi_first_1")
		comment.ID, comment.Kind = "comment/"+term, "comment"
		comment.SourceID = fmt.Sprintf("comment_first_1_%d", index+1)
		f.put(t, comment, searchstore.Text{Title: strings.Repeat(term+" ", 10)}, 0)
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			f.drain(t, "org_first")
		}
		t.Run(fmt.Sprintf("indexed=%v", indexed), func(t *testing.T) {
			q := searchstore.Query{Text: "alpha bravo", Exact: true, Limit: 1}
			page := f.find(t, "org_first", q)
			if !slices.Equal(searchIDs(page), []string{"wi_first_1"}) || page.Items[0].Coverage != 2 || page.NextCursor == "" {
				t.Fatalf("full-coverage issue must rank first with continuation: %+v", page)
			}
			q.Cursor = page.NextCursor
			page = f.find(t, "org_first", q)
			if !slices.Equal(searchIDs(page), []string{"wi_first_2"}) || page.NextCursor != "" {
				t.Fatalf("remaining issue must be reachable exactly once: %+v", page)
			}
		})
	}
}

func TestSearchPrefixBeyondExpansionBudgetRemainsReachable(t *testing.T) {
	f := newSearchFixture(t)
	var low strings.Builder
	for index := range searchstore.LexemeLimit {
		fmt.Fprintf(&low, "prefixlexeme%03d ", index)
	}
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: low.String()}, 0)
	f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: strings.Repeat("prefixlexeme999 ", 100)}, 0)
	f.drain(t, "org_first")
	q := searchstore.Query{Text: "prefixlexeme", Limit: 1}
	var got []string
	for range 10 {
		page := f.find(t, "org_first", q)
		got = append(got, searchIDs(page)...)
		if page.NextCursor == "" {
			slices.Sort(got)
			if !slices.Equal(got, []string{"wi_first_1", "wi_first_2"}) {
				t.Fatalf("prefix reachability beyond budget: %v", got)
			}
			return
		}
		if q.Cursor == page.NextCursor {
			t.Fatal("prefix cursor did not advance")
		}
		q.Cursor = page.NextCursor
	}
	t.Fatal("prefix cursor did not exhaust")
}

func TestSearchAggregateCapsCommentsAndKeepsTitleAndIdentity(t *testing.T) {
	f := newSearchFixture(t)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "Titlemarker"}, 0)
	comment := searchIssue("org_first", "wi_first_1")
	comment.ID, comment.Kind, comment.SourceID = "comment/example", "comment", "comment_first_1_1"
	f.put(t, comment, searchstore.Text{Body: strings.Repeat("界 ", 20000) + " tailmarker"}, 0)
	f.drain(t, "org_first")
	for _, term := range []string{"Titlemarker", "#1", "界"} {
		page := f.find(t, "org_first", searchstore.Query{Text: term})
		if !slices.Equal(searchIDs(page), []string{"wi_first_1"}) {
			t.Fatalf("aggregate term %q: %v", term, searchIDs(page))
		}
		text, err := f.content.Read(t.Context(), page.Items[0].Document)
		if err != nil || len(text.Title)+len(text.Body) > 64*1024 {
			t.Fatalf("aggregate budget: title=%d body=%d error=%v", len(text.Title), len(text.Body), err)
		}
	}
	if page := f.find(t, "org_first", searchstore.Query{Text: "tailmarker"}); len(page.Items) != 0 {
		t.Fatalf("aggregate included truncated tail: %+v", page)
	}
	var count int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(DISTINCT p.document_id) FROM search_postings p JOIN search_documents d ON d.organization_id=p.organization_id AND d.id=p.document_id WHERE d.organization_id='org_first' AND d.work_item_id='wi_first_1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("work-item posting owners=%d error=%v", count, err)
	}
}

func TestSearchCommentOnlyAggregateAcceptsIssueSource(t *testing.T) {
	f := newSearchFixture(t)
	comment := searchIssue("org_first", "wi_first_1")
	comment.ID, comment.Kind, comment.SourceID = "comment/example", "comment", "comment_first_1_1"
	f.put(t, comment, searchstore.Text{Body: "commentmarker"}, 0)
	for _, indexed := range []bool{false, true} {
		if indexed {
			f.drain(t, "org_first")
		}
		for _, text := range []string{"", "commentmarker"} {
			page := f.find(t, "org_first", searchstore.Query{Text: text})
			if !slices.Equal(searchIDs(page), []string{"wi_first_1"}) {
				t.Fatalf("comment-only query %q indexed=%v: %+v", text, indexed, page)
			}
		}
	}
	issue := searchIssue("org_first", "wi_first_1")
	issue.ID = "issue-source/example"
	f.put(t, issue, searchstore.Text{Title: "Titlemarker", Body: "bodymarker"}, 0)
	for _, indexed := range []bool{false, true} {
		if indexed {
			f.drain(t, "org_first")
		}
		page := f.find(t, "org_first", searchstore.Query{Text: "Titlemarker bodymarker commentmarker"})
		if !slices.Equal(searchIDs(page), []string{"wi_first_1"}) || page.Items[0].Coverage != 3 {
			t.Fatalf("issue source did not replace placeholder: %+v", page)
		}
	}
}

func TestSearchAggregateMigrationRebuildsEveryTenant(t *testing.T) {
	f := newSearchFixture(t)
	issues := []searchstore.Document{searchIssue("org_first", "wi_first_1"), searchIssue("org_second", "wi_first_1")}
	for _, issue := range issues {
		f.put(t, issue, searchstore.Text{Title: "alpha"}, 0)
		comment := issue
		comment.ID, comment.Kind, comment.SourceID = "comment/example", "comment", "comment_first_1_1"
		f.put(t, comment, searchstore.Text{Body: "bravo"}, 0)
		f.drain(t, issue.OrganizationID)
	}
	f.admin.SetMaxOpenConns(1)
	provider, err := migrationProvider(f.admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 20261101050746); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET vector=source_vector WHERE kind='comment'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `SET ROLE detent_migration`); err != nil {
		t.Fatal(err)
	}
	_, migrationErr := provider.Up(t.Context())
	_, resetErr := f.admin.ExecContext(t.Context(), `RESET ROLE`)
	if migrationErr != nil || resetErr != nil {
		t.Fatalf("migrate=%v reset=%v", migrationErr, resetErr)
	}
	var count int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_policies WHERE policyname='search_work_migration'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("temporary policies=%d error=%v", count, err)
	}
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_postings p JOIN search_documents d ON d.organization_id=p.organization_id AND d.id=p.document_id WHERE d.kind='comment'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy comment postings=%d error=%v", count, err)
	}
	for _, issue := range issues {
		f.drain(t, issue.OrganizationID)
		page := f.find(t, issue.OrganizationID, searchstore.Query{Text: "alpha bravo", Exact: true})
		if !slices.Equal(searchIDs(page), []string{"wi_first_1"}) || page.Items[0].Coverage != 2 {
			t.Fatalf("migrated aggregate for %s: %+v", issue.OrganizationID, page)
		}
	}
}
