package hubserver

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/displayorder"
	"github.com/digitaldrywood/detent/internal/displayorder/testfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkListOrder(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "list-order")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
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
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: needle + pair.name}, Title: needle + "-" + pair.name, Body: issueContractTestSections, State: state, Priority: priority, Labels: []string{needle}})
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE issues SET last_activity_at=$1 WHERE organization_id=$2 AND project_id=$3 AND native_id=$4", pair.item.LastActivityAt, f.project.OrganizationID, f.project.ID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if err := f.service.refreshBoardRecord(t.Context(), f.project.OrganizationID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: issue.WorkItemID}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
				issues = append(issues, issue)
			}
			left, right := issues[0], issues[1]
			test.Left.Identifier = string(f.project.ID) + "#" + strconv.Itoa(left.Number)
			test.Right.Identifier = string(f.project.ID) + "#" + strconv.Itoa(right.Number)
			want := []tracker.NativeWorkItemID{left.WorkItemID, right.WorkItemID}
			if displayorder.Compare(test.Terminal, test.Left, test.Right) > 0 {
				slices.Reverse(want)
			}
			params := url.Values{"label": {needle}, "limit": {"1"}}
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
	t.Parallel()
	f, guard := newWriterGuardNativeFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	original := make([]tracker.NativeIssue, 0, 12)
	for index := range 12 {
		state := f.project.States[index%len(f.project.States)]
		priority := index % 4
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{
			Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("create-changing-%d", index)}, Title: fmt.Sprintf("changing-%d", index),
			Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: state.Name, Priority: &priority,
		})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		activity := time.Date(2026, 10, 1, 0, 0, 0, index*int(time.Microsecond), time.UTC)
		result, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE issues SET last_activity_at=$1 WHERE organization_id=$2 AND project_id=$3 AND native_id=$4", activity, f.project.OrganizationID, f.project.ID, issue.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			t.Fatalf("activity fixture affected%d rows: %v", count, err)
		}
		if err := f.service.refreshBoardRecord(t.Context(), f.project.OrganizationID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: issue.WorkItemID}, f.service.config.now()); err != nil {
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
	drainTestNativeBoard(t, f.service)
	installWriterReadGuard(t, f.service, guard)
	writes := func() int64 {
		t.Helper()
		guard.mu.Lock()
		defer guard.mu.Unlock()
		return int64(len(guard.violations))
	}
	for {
		before := writes()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if len(seen) == 0 && writes() != before {
			t.Fatal("reading a first work-list page wrote to the database")
		}
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
				current := readWorkItem(t, f, issue.WorkItemID, "")
				response = performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token,
					tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "priority-" + string(issue.WorkItemID)}, ExpectedRevision: current.Revision, Priority: tracker.SetPriority(new(0))})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &current)
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token,
					tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "done-" + string(issue.WorkItemID)}, ExpectedRevision: current.Revision, State: "Done", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &current)
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/archive", f.token,
					archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "archive-" + string(issue.WorkItemID)}, ExpectedRevision: current.Revision})
				requireNativeStatus(t, response, http.StatusOK)
				if err := f.service.refreshBoardRecord(t.Context(), f.project.OrganizationID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: issue.WorkItemID}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
			}
			f.create(t, "added-while-paging")
			drainTestNativeBoard(t, f.service)
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
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=1&cursor="+url.QueryEscape(firstCursor), f.token, nil)
	requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
	if writes() != 0 || guard.queries.Load() == 0 || guard.transactions.Load() == 0 {
		t.Fatalf("page reads bypassed PostgreSQL read-only guard: writes=%d queries=%d transactions=%d", writes(), guard.queries.Load(), guard.transactions.Load())
	}
}
