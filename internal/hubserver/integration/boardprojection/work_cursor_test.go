package boardprojection_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkListCursorKeepsProjectionAndFilterBinding(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	remaining := map[tracker.NativeWorkItemID]bool{}
	for _, name := range []string{"Cursor first", "Cursor second", "Cursor third"} {
		issue := f.Create(t, name)
		remaining[issue.WorkItemID] = true
	}
	f.Hub.RefreshBoard(t, f.Organization())
	f.Hub.RefreshPages(t, f.Organization())

	query := url.Values{"state": {"Todo"}, "limit": {"1"}}
	get := func(t *testing.T, include string) tracker.NativeIssuePage {
		t.Helper()
		query.Set("include", include)
		response := f.Request(t, http.MethodGet, f.Base+"/work-items?"+query.Encode(), nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		hubfixture.Decode(t, response, &page)
		return page
	}
	get(t, "workspace")
	for _, include := range []string{"summary", "work", ""} {
		page := get(t, include)
		for _, issue := range page.Items {
			if !remaining[issue.WorkItemID] {
				t.Fatalf("unexpected or repeated issue %s", issue.WorkItemID)
			}
			delete(remaining, issue.WorkItemID)
		}
		if include == "summary" {
			if page.NextCursor == "" {
				t.Fatal("summary page omitted its continuation cursor")
			}
			query.Set("cursor", page.NextCursor)
			query.Set("include", "summary,workspace")
			response := f.Request(t, http.MethodGet, f.Base+"/work-items?"+query.Encode(), nil)
			hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
		}
		query.Set("cursor", page.NextCursor)
	}
	if len(remaining) != 0 || query.Get("cursor") != "" {
		t.Fatalf("continuation lost issues %v or retained cursor %q", remaining, query.Get("cursor"))
	}
}
