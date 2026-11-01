package postgres_test

import (
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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
			page := f.find(t, "org_first", searchstore.Query{Kind: "thread", ProjectIDs: test.projects, Text: "shared"})
			if !slices.Equal(searchIDs(page), test.want) {
				t.Fatalf("grant filters=%v want=%v", searchIDs(page), test.want)
			}
		})
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
	f := newSearchFixture(t)
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
	err := f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error {
		for _, d := range documents {
			if err := w.PutSearch(t.Context(), d, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			f.drain(t, org)
		}

		for _, text := range []string{"", "shared"} {
			t.Run(fmt.Sprintf("indexed=%v/%s", indexed, text), func(t *testing.T) {
				query := searchstore.Query{Kind: "thread", Text: text, Limit: 200}
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
