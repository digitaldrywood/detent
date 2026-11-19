package workpage_test

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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkListOrder(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Organization(), corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.Project.ID)
		if err != nil {
			return err
		}
		record.Settings.DuplicateChallenge = false
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for index, test := range testfixture.Comparisons() {
		t.Run(test.Name, func(t *testing.T) {
			needle := fmt.Sprintf("ordercase%02d", index)
			state := "Todo"
			if test.Terminal {
				state = "Done"
			}
			var issues []tracker.NativeIssue
			for _, pair := range []struct {
				name string
				item displayorder.Item
			}{{"left", test.Left}, {"right", test.Right}} {
				priority := pair.item.Priority
				if priority != nil && (*priority < displayorder.PriorityUrgent || *priority > displayorder.PriorityLow) {
					priority = nil
				}
				response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items", f.Token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: needle + pair.name}, Title: needle + "-" + pair.name, Body: hubfixture.IssueContractSections, State: state, Priority: priority, Labels: []string{needle}})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				hubfixture.Decode(t, response, &issue)
				if _, err := f.Hub.Admin.ExecContext(t.Context(), "UPDATE issues SET last_activity_at=$1 WHERE organization_id=$2 AND project_id=$3 AND native_id=$4", pair.item.LastActivityAt, f.Organization(), f.Project.ID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				f.Hub.RefreshBoard(t, f.Organization())
				issues = append(issues, issue)
			}
			left, right := issues[0], issues[1]
			test.Left.Identifier = string(f.Project.ID) + "#" + strconv.Itoa(left.Number)
			test.Right.Identifier = string(f.Project.ID) + "#" + strconv.Itoa(right.Number)
			want := []tracker.NativeWorkItemID{left.WorkItemID, right.WorkItemID}
			if displayorder.Compare(test.Terminal, test.Left, test.Right) > 0 {
				slices.Reverse(want)
			}
			params := url.Values{"label": {needle}, "limit": {"1"}}
			for index, id := range want {
				response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?"+params.Encode(), f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				hubfixture.Decode(t, response, &page)
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
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	original := make([]tracker.NativeIssue, 0, 6)
	for index := range 6 {
		state := f.Project.States[index%len(f.Project.States)]
		priority := index % 4
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items", f.Token, tracker.CreateIssue{
			Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("create-changing-%d", index)}, Title: fmt.Sprintf("changing-%d", index),
			Body: testenv.DistinctText(fmt.Sprint(index), 24) + "\n" + hubfixture.IssueBody(), State: state.Name, Priority: &priority,
		})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		hubfixture.Decode(t, response, &issue)
		activity := time.Date(2026, 10, 1, 0, 0, 0, index*int(time.Microsecond), time.UTC)
		_, err := f.Hub.Admin.ExecContext(t.Context(), "UPDATE issues SET last_activity_at=$1 WHERE organization_id=$2 AND project_id=$3 AND native_id=$4", activity, f.Organization(), f.Project.ID, issue.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		f.Hub.RefreshBoard(t, f.Organization())
		issue.State, issue.Terminal, issue.Priority, issue.LastActivityAt = state.Name, state.Terminal, &priority, activity
		original = append(original, issue)
	}
	lane := func(state string) int {
		return slices.IndexFunc(f.Project.States, func(value tracker.NativeState) bool { return value.Name == state })
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
	var added tracker.NativeWorkItemID
	f.Hub.RefreshBoard(t, f.Organization())
	for {
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?"+params.Encode(), f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		hubfixture.Decode(t, response, &page)
		for _, issue := range page.Items {
			seen = append(seen, issue.WorkItemID)
		}
		if len(seen) > len(original) {
			t.Fatal("paging repeated an item")
		}
		if len(seen) == 2 {
			firstCursor = page.NextCursor
			for _, issue := range original {
				response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.Token,
					tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "paging-" + string(issue.WorkItemID)}, Body: "activity while paging"})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				current := hubfixture.ReadWorkItem(t, f, issue.WorkItemID, "")
				response = f.Hub.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(issue.WorkItemID), f.Token,
					tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "priority-" + string(issue.WorkItemID)}, ExpectedRevision: current.Revision, Priority: tracker.SetPriority(new(0))})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				hubfixture.Decode(t, response, &current)
				if current.State != "Done" {
					response = f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.Token,
						tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "done-" + string(issue.WorkItemID)}, ExpectedRevision: current.Revision, State: "Done", Reason: "user_requested"})
					hubfixture.RequireStatus(t, response, http.StatusOK)
					hubfixture.Decode(t, response, &current)
				}
				response = f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/archive", f.Token,
					map[string]string{"idempotency_key": "archive-" + string(issue.WorkItemID), "expected_revision": fmt.Sprint(current.Revision)})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				f.Hub.RefreshBoard(t, f.Organization())
			}
			added = f.Create(t, "added-while-paging").WorkItemID
			f.Hub.RefreshBoard(t, f.Organization())
			break
		}
		if page.NextCursor == "" {
			break
		}
		params.Set("cursor", page.NextCursor)
	}
	if len(seen) != 2 || seen[0] != original[0].WorkItemID || seen[1] != original[1].WorkItemID || firstCursor == "" {
		t.Fatalf("seen = %v, want the first page of the original lane order", seen)
	}
	response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?limit=2&cursor="+url.QueryEscape(firstCursor), f.Token, nil)
	if failure := hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "invalid_request"); failure.Message != "Work list order changed; restart the query" {
		t.Fatalf("board change left the cursor valid: %s", response.Body.String())
	}
	response = f.Hub.Request(t, http.MethodGet, f.Base+"/work-items", f.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var fresh tracker.NativeIssuePage
	hubfixture.Decode(t, response, &fresh)
	if len(fresh.Items) != 1 || fresh.Items[0].WorkItemID != added || fresh.NextCursor != "" {
		t.Fatalf("restarted list did not contain only the newly filed issue %s", added)
	}
}

func TestNativeWorkListRunnerPaging(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	other := hubfixture.NewNative(t, h, hubfixture.Secondary, "Other project")
	foreign := other.Create(t, "Other issue")
	seedRunner := func(t *testing.T, f hubfixture.Native) string {
		t.Helper()
		return hubtest.Runner(t, h.Repository, f.Organization(), hubtest.RunnerOptions{
			Projects: []tracker.ProjectID{f.Project.ID}, Operations: []string{runnerauth.Read}, Now: h.Now(),
		}).Credential.Token
	}
	otherToken := seedRunner(t, other)
	h.RefreshBoard(t, other.Organization())
	for _, query := range []string{"archived=all", "state=Todo"} {
		t.Run(query, func(t *testing.T) {
			f := hubfixture.NewNative(t, h, h.Primary, "Runner paging "+query)
			token := seedRunner(t, f)
			if query == "state=Todo" {
				response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items", f.Token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "earlier-done"}, Title: "Earlier completed item", Body: hubfixture.IssueContractSections, State: "Done"})
				hubfixture.RequireStatus(t, response, http.StatusOK)
			}
			var items []tracker.NativeIssue
			for index := range 4 {
				items = append(items, f.Create(t, fmt.Sprintf("paging item %d", index)))
			}
			f.Hub.RefreshBoard(t, f.Organization())
			read := func(params string) tracker.NativeIssuePage {
				t.Helper()
				response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?"+query+params, token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				hubfixture.Decode(t, response, &page)
				return page
			}
			probe := read("&limit=1")
			if len(probe.Items) != 1 || probe.NextCursor == "" {
				t.Fatal("one-item lookup did not return the first match")
			}
			limit := "&limit=1"
			if query == "archived=all" {
				limit = "&limit=2"
			}
			page := read(limit + "&cursor=")
			if len(page.Items) == 0 || page.NextCursor == "" {
				t.Fatalf("first page has %d items, cursor %q", len(page.Items), page.NextCursor)
			}
			path := other.Base + "/work-items?" + query + "&limit=1"
			response := f.Hub.Request(t, http.MethodGet, path+"&cursor=", otherToken, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var otherPage tracker.NativeIssuePage
			hubfixture.Decode(t, response, &otherPage)
			if len(otherPage.Items) != 1 || otherPage.Items[0].WorkItemID != foreign.WorkItemID {
				t.Fatalf("second organization returned %v", otherPage.Items)
			}
			response = f.Hub.Request(t, http.MethodGet, path+"&cursor="+url.QueryEscape(page.NextCursor), otherToken, nil)
			hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
			var seen []tracker.NativeWorkItemID
			for _, item := range page.Items {
				seen = append(seen, item.WorkItemID)
			}
			var want []tracker.NativeWorkItemID
			for _, item := range items {
				want = append(want, item.WorkItemID)
				response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(item.WorkItemID)+"/comments", f.Token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "activity-" + string(item.WorkItemID)}, Body: "Updated during paging"})
				hubfixture.RequireStatus(t, response, http.StatusOK)
			}
			if query == "state=Todo" {
				moved := items[1]
				response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(moved.WorkItemID)+"/workflow", f.Token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-during-paging"}, ExpectedRevision: hubfixture.ReadWorkItem(t, f, moved.WorkItemID, "").Revision, State: "Done", Reason: "user_requested"})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				want = slices.Delete(want, 1, 2)
				f.Create(t, "created after page one")
			}
			f.Hub.RefreshBoard(t, f.Organization())
			response = f.Hub.Request(t, http.MethodGet, f.Base+"/work-items?"+query+"&limit=1&cursor="+url.QueryEscape(probe.NextCursor), token, nil)
			if failure := hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "invalid_request"); failure.Message != "Work list order changed; restart the query" {
				t.Fatalf("unexpected display cursor error: %v", failure.Message)
			}
			for pages := 1; page.NextCursor != ""; pages++ {
				if pages > len(items) {
					t.Fatal("cursor did not reach the end of the original items")
				}
				page = read(limit + "&cursor=" + url.QueryEscape(page.NextCursor))
				if len(page.Items) == 0 {
					t.Fatal("expected the next matching item")
				}
				for _, item := range page.Items {
					seen = append(seen, item.WorkItemID)
				}
			}
			if !slices.Equal(seen, want) {
				t.Fatalf("read %v, want %v", seen, want)
			}
		})
	}
}
