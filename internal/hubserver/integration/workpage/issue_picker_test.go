package workpage_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/workpage"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeIssuePickerProjection(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	target := f.Create(t, "Picker original")
	other := f.Create(t, "Another issue")
	last := f.Create(t, "Third issue")
	response := f.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(target.WorkItemID), tracker.UpdateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "picker-title-priority"}, ExpectedRevision: target.Revision,
		Title: new("Picker needle"), Priority: tracker.SetPriority(new(1)),
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &target)
	f.Hub.RefreshBoard(t, f.Organization())
	f.Hub.RefreshPages(t, f.Organization())

	read := func(t *testing.T, query string) workpage.PickerPage {
		t.Helper()
		response := f.Request(t, http.MethodGet, f.Base+"/work-items?"+query, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		for _, field := range []string{`"body"`, `"labels"`, `"actor"`, `"revision"`} {
			if strings.Contains(response.Body.String(), field) {
				t.Fatalf("picker page carries %s: %s", field, response.Body)
			}
		}
		var page workpage.PickerPage
		hubfixture.Decode(t, response, &page)
		return page
	}

	t.Run("keyset continuation", func(t *testing.T) {
		const query = "fields=picker&limit=2"
		first := read(t, query)
		if first.NextCursor == "" {
			t.Fatal("picker did not return a continuation cursor")
		}
		second := read(t, query+"&cursor="+url.QueryEscape(first.NextCursor))
		if second.NextCursor != "" {
			t.Fatal("picker returned a cursor after the last page")
		}
		remaining := map[tracker.NativeWorkItemID]tracker.NativeIssue{target.WorkItemID: target, other.WorkItemID: other, last.WorkItemID: last}
		for _, item := range append(first.Items, second.Items...) {
			issue, ok := remaining[item.WorkItemID]
			if !ok || item.Number != issue.Number || item.Title != issue.Title || item.State != issue.State {
				t.Fatalf("unexpected, incomplete, or repeated picker row: %+v", item)
			}
			delete(remaining, item.WorkItemID)
		}
		if len(remaining) != 0 {
			t.Fatalf("picker omitted issues: %v", remaining)
		}
	})

	t.Run("server search", func(t *testing.T) {
		page := read(t, "fields=picker&q=needle")
		if len(page.Items) != 1 || page.Items[0].WorkItemID != target.WorkItemID || page.Items[0].Title != "Picker needle" || page.Items[0].Priority == nil || *page.Items[0].Priority != 1 {
			t.Fatalf("unexpected picker search result: %+v", page)
		}
	})

	for _, test := range []struct{ name, query string }{
		{"unknown projection", "fields=full"},
		{"combined with include", "fields=picker&include=summary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.Request(t, http.MethodGet, f.Base+"/work-items?"+test.query, nil)
			hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
		})
	}
}
