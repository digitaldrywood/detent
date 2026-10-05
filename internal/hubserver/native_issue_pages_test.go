package hubserver

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/displayorder"
	"github.com/digitaldrywood/detent/internal/displayorder/testfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkListOrder(t *testing.T) {
	f := newNativeFixture(t, nil, "", "list-order")
	for index, test := range testfixture.Comparisons() {
		t.Run(test.Name, func(t *testing.T) {
			needle := fmt.Sprintf("order-case-%02d", index)
			left := f.create(t, needle+"-left")
			right := f.create(t, needle+"-right")
			state := "Todo"
			if test.Terminal {
				state = "Done"
			}
			for _, pair := range []struct {
				issue tracker.NativeIssue
				item  displayorder.Item
			}{{left, test.Left}, {right, test.Right}} {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET
 workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?), last_activity_at = ? WHERE native_id = ?`,
					f.project.ID, state, formatHubTime(pair.item.LastActivityAt), pair.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", pair.item.Priority, pair.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			test.Left.Identifier = string(f.project.ID) + "#" + strconv.Itoa(left.Number)
			test.Right.Identifier = string(f.project.ID) + "#" + strconv.Itoa(right.Number)
			want := []tracker.NativeWorkItemID{left.WorkItemID, right.WorkItemID}
			if displayorder.Compare(test.Terminal, test.Left, test.Right) > 0 {
				slices.Reverse(want)
			}
			params := url.Values{"q": {needle}, "limit": {"1"}}
			for index, id := range want {
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				decodeHubResponse(t, response, &page)
				if len(page.Items) != 1 || page.Items[0].WorkItemID != id {
					t.Fatalf("page %d = %#v, want %s", index, page.Items, id)
				}
				if (page.NextCursor == "") != (index == len(want)-1) {
					t.Fatalf("page %d cursor = %q", index, page.NextCursor)
				}
				params.Set("cursor", page.NextCursor)
			}
		})
	}
}

func TestNativeWorkListPagingDuringChanges(t *testing.T) {
	f := newNativeFixture(t, nil, "", "changing-list")
	var original []tracker.NativeIssue
	for index := range 12 {
		issue := f.create(t, fmt.Sprintf("changing-%d", index))
		state := f.project.States[index%len(f.project.States)]
		priority := index % 4
		activity := time.Date(2026, 10, 1, 0, 0, 0, index*100, time.UTC)
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id =
 (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = ?), last_activity_at = ? WHERE native_id = ?`, f.project.ID, state.Name, formatHubTime(activity), issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", priority, issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		issue.State, issue.Terminal, issue.Priority, issue.LastActivityAt = state.Name, state.Terminal, &priority, activity
		original = append(original, issue)
	}
	lane := func(state string) int {
		return slices.IndexFunc(f.project.States, func(value tracker.NativeState) bool { return value.Name == state })
	}
	slices.SortFunc(original, func(a, b tracker.NativeIssue) int {
		if order := lane(a.State) - lane(b.State); order != 0 {
			return order
		}
		return displayorder.Compare(a.Terminal,
			displayorder.Item{Priority: a.Priority, LastActivityAt: a.LastActivityAt, Identifier: string(a.ProjectID) + "#" + strconv.Itoa(a.Number)},
			displayorder.Item{Priority: b.Priority, LastActivityAt: b.LastActivityAt, Identifier: string(b.ProjectID) + "#" + strconv.Itoa(b.Number)})
	})
	params := url.Values{"limit": {"2"}}
	var seen []tracker.NativeWorkItemID
	var firstCursor string
	for {
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
		for _, issue := range page.Items {
			seen = append(seen, issue.WorkItemID)
		}
		if len(seen) > len(original) {
			t.Fatal("paging repeated an item")
		}
		if len(seen) == 2 {
			firstCursor = page.NextCursor
			for _, issue := range original {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token,
					tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "paging-" + string(issue.WorkItemID)}, Body: "activity while paging"})
				requireNativeStatus(t, response, http.StatusOK)
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id =
 (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Done'), archived = 1 WHERE native_id = ?`, f.project.ID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 0 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			f.create(t, "added-while-paging")
		}
		if page.NextCursor == "" {
			break
		}
		params.Set("cursor", page.NextCursor)
	}
	for index, issue := range original {
		if len(seen) != len(original) || seen[index] != issue.WorkItemID {
			t.Fatalf("seen = %v, want original lane order", seen)
		}
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=1&cursor="+url.QueryEscape(firstCursor), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var replay tracker.NativeIssuePage
	decodeHubResponse(t, response, &replay)
	if len(replay.Items) != 1 || replay.Items[0].WorkItemID != original[2].WorkItemID || !replay.Items[0].Archived || !replay.Items[0].LastActivityAt.After(original[2].LastActivityAt) {
		t.Fatalf("replayed cursor lost frozen order or current detail: %#v", replay.Items)
	}
	expires := f.service.config.now().Add(2 * time.Hour)
	f.service.config.now = func() time.Time { return expires }
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var fresh tracker.NativeIssuePage
	decodeHubResponse(t, response, &fresh)
	if len(fresh.Items) != 1 || fresh.Items[0].Title != "added-while-paging" {
		t.Fatalf("fresh list = %#v", fresh.Items)
	}
	for _, table := range []string{"native_issue_pages", "native_issue_page_items"} {
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("retained %d expired or single-page rows in %s", count, table)
		}
	}
}
