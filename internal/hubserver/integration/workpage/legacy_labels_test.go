package workpage_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLegacyLongLabelsKeepBoardsEditsAndFiltersWorking(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	tenants := []hubfixture.Native{hubfixture.NewNative(t, h, hubfixture.Primary, "legacy-labels"), hubfixture.NewNative(t, h, hubfixture.Secondary, "legacy-labels")}
	prefix := strings.Repeat("界", 100)
	long := strings.Repeat("界", 200)
	type seeded struct {
		keep, remove tracker.NativeWorkItemID
		medium, long string
	}
	items := make([]seeded, len(tenants))
	for index, f := range tenants {
		suffix := string(rune('a' + index))
		items[index] = seeded{medium: prefix + suffix, long: long + suffix}
		for _, target := range []*tracker.NativeWorkItemID{&items[index].keep, &items[index].remove} {
			issue := hubtest.Issue(t, h.Repository, h.Config.RunDetails, f.Project, hubtest.IssueOptions{State: "Todo", Body: hubfixture.IssueContractSections, Labels: []string{items[index].medium, items[index].long, "short"}})
			*target = issue.Issue.WorkItemID
		}
		h.RefreshBoard(t, f.Project.OrganizationID)
	}
	list := func(t *testing.T, f hubfixture.Native, params url.Values) []tracker.NativeWorkItemID {
		t.Helper()
		var ids []tracker.NativeWorkItemID
		params.Set("limit", "1")
		for pages := 0; ; pages++ {
			response := f.Request(t, http.MethodGet, f.Base+"/work-items?"+params.Encode(), nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var page tracker.NativeIssuePage
			hubfixture.Decode(t, response, &page)
			for _, issue := range page.Items {
				ids = append(ids, issue.WorkItemID)
			}
			if page.NextCursor == "" {
				return ids
			}
			if pages > 4 {
				t.Fatal("cursor did not advance")
			}
			params.Set("cursor", page.NextCursor)
		}
	}
	for index, f := range tenants {
		t.Run(string(f.Project.OrganizationID), func(t *testing.T) {
			own, other := items[index], items[1-index]
			for _, id := range []tracker.NativeWorkItemID{own.keep, own.remove} {
				if issue := hubfixture.ReadWorkItem(t, f, id, ""); !slices.Contains(issue.Labels, own.long) || !slices.Contains(issue.Labels, own.medium) {
					t.Fatalf("detail lost legacy labels: %d labels", len(issue.Labels))
				}
			}
			if got := list(t, f, url.Values{}); len(got) != 2 || !slices.Contains(got, own.keep) || !slices.Contains(got, own.remove) {
				t.Fatalf("unfiltered list=%v", got)
			}
			if got := list(t, f, url.Values{"label": {own.medium}}); len(got) != 2 {
				t.Fatalf("legacy label within the filter bound matched %v", got)
			}
			if got := list(t, f, url.Values{"label": {other.medium}}); len(got) != 0 {
				t.Fatalf("same-prefix label of another tenant matched %v", got)
			}
			refused := f.Request(t, http.MethodGet, f.Base+"/work-items?"+url.Values{"label": {own.long}}.Encode(), nil)
			if body := refused.Body.String(); refused.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "up to 512 bytes") {
				t.Fatalf("long legacy label filter: status=%d body=%s", refused.Code, body)
			}
			call := hubfixture.MCP(t, h.Handler(), hubfixture.MCPPath(f.Project.OrganizationID), f.Token)
			hubfixture.MCPData(t, call("tools/call", operatortool.WorkList, map[string]any{"project_id": string(f.Project.ID), "label": own.long}), true)

			keep := hubfixture.ReadWorkItem(t, f, own.keep, "")
			title := "Edited " + keep.Title
			response := f.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(own.keep), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "keep-long"}, ExpectedRevision: keep.Revision, Title: &title})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			if edited := hubfixture.ReadWorkItem(t, f, own.keep, ""); edited.Title != title || !slices.Equal(edited.Labels, keep.Labels) {
				t.Fatalf("edit changed labels or lost title: %+v", edited.Labels)
			}
			remove := hubfixture.ReadWorkItem(t, f, own.remove, "")
			labels := []string{own.medium, "short"}
			response = f.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(own.remove), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "remove-long"}, ExpectedRevision: remove.Revision, Labels: &labels})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			if edited := hubfixture.ReadWorkItem(t, f, own.remove, ""); slices.Contains(edited.Labels, own.long) || len(edited.Labels) != 2 {
				t.Fatalf("label removal kept the long label: %d labels", len(edited.Labels))
			}
			if got := list(t, f, url.Values{"label": {own.medium}}); len(got) != 2 {
				t.Fatalf("legacy label after edits matched %v", got)
			}
		})
	}
	t.Run("new label byte bound", func(t *testing.T) {
		f := tenants[0]
		accepted := strings.Repeat("界", 66) + "ab"
		rejected := accepted + "c"
		if len(accepted) != 200 || len(rejected) != 201 {
			t.Fatalf("fixture labels are %d and %d bytes", len(accepted), len(rejected))
		}
		before := list(t, f, url.Values{})
		response := f.Request(t, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "label-over-bound"}, Title: "Label over bound", Body: hubfixture.IssueContractSections, State: "Todo", Labels: []string{rejected}})
		hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
		h.RefreshBoard(t, f.Project.OrganizationID)
		if after := list(t, f, url.Values{}); !slices.Equal(after, before) {
			t.Fatalf("refused create changed the list: before=%v after=%v", before, after)
		}
		response = f.Request(t, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "label-at-bound"}, Title: "Label at bound", Body: hubfixture.IssueContractSections, State: "Todo", Labels: []string{accepted}})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var created tracker.NativeIssue
		hubfixture.Decode(t, response, &created)
		h.RefreshBoard(t, f.Project.OrganizationID)
		if got := list(t, f, url.Values{"label": {accepted}}); !slices.Equal(got, []tracker.NativeWorkItemID{created.WorkItemID}) {
			t.Fatalf("label at the bound matched %v", got)
		}
		labels := []string{accepted, rejected}
		response = f.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(created.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "add-over-bound"}, ExpectedRevision: created.Revision, Labels: &labels})
		hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
		if current := hubfixture.ReadWorkItem(t, f, created.WorkItemID, ""); current.Revision != created.Revision || !slices.Equal(current.Labels, []string{accepted}) {
			t.Fatalf("refused edit changed the item: revision=%d labels=%d", current.Revision, len(current.Labels))
		}
	})
}
