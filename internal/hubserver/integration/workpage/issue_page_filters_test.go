package workpage_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssuePageFiltersPreservePopulationAndProjectScope(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "example-library")
	want := map[tracker.ProjectID]map[tracker.NativeWorkItemID]string{}
	for _, fixture := range []hubfixture.Native{f, other} {
		if err := fixture.Hub.Repository.WriteTenant(t.Context(), fixture.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), fixture.Project.ID)
			if err != nil {
				return err
			}
			record.Settings.DuplicateChallenge = false
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		want[fixture.Project.ID] = map[tracker.NativeWorkItemID]string{}
		for index := range 103 {
			group := "unknown"
			if index == 100 {
				group = "known"
			}
			response := fixture.Hub.Request(t, http.MethodPost, fixture.Base+"/work-items", fixture.Token, tracker.CreateIssue{
				Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("filter-population-%d", index)}, Title: fmt.Sprintf("Filter population %d", index), Body: hubfixture.IssueContractSections, State: "Todo",
				Labels:    []string{"Common-Label", "common-label", "label'quote", "label-four", "label-five", group},
				Assignees: []string{"Common-Assignee", "common-assignee", "assignee'quote", "assignee-four", "assignee-five", group},
			})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			hubfixture.Decode(t, response, &issue)
			want[fixture.Project.ID][issue.WorkItemID] = group
		}
	}
	f.Hub.RefreshBoard(t, f.Project.OrganizationID)
	for _, fixture := range []hubfixture.Native{f, other} {
		for _, filter := range []struct{ name, value string }{
			{"label", "Common-Label"}, {"label", "label'quote"}, {"label", "known"}, {"label", "unknown"},
			{"assignee", "Common-Assignee"}, {"assignee", "assignee'quote"}, {"assignee", "known"}, {"assignee", "unknown"},
		} {
			t.Run(string(fixture.Project.ID)+"/"+filter.name+"="+filter.value, func(t *testing.T) {
				params := url.Values{filter.name: {filter.value}, "limit": {"37"}}
				seen := map[tracker.NativeWorkItemID]bool{}
				for {
					response := fixture.Hub.Request(t, http.MethodGet, fixture.Base+"/work-items?"+params.Encode(), fixture.Token, nil)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var page tracker.NativeIssuePage
					hubfixture.Decode(t, response, &page)
					for _, issue := range page.Items {
						group, ok := want[fixture.Project.ID][issue.WorkItemID]
						if !ok || seen[issue.WorkItemID] || issue.ProjectID != fixture.Project.ID || filter.value == "known" && group != "known" || filter.value == "unknown" && group != "unknown" {
							t.Fatalf("unexpected or duplicate filtered issue=%+v", issue)
						}
						seen[issue.WorkItemID] = true
					}
					if page.NextCursor == "" {
						break
					}
					params.Set("cursor", page.NextCursor)
				}
				count := 103
				switch filter.value {
				case "known":
					count = 1
				case "unknown":
					count = 102
				}
				if len(seen) != count {
					t.Fatalf("filtered count=%d want=%d", len(seen), count)
				}
				selection := viewstore.BoardSelection{ProjectID: fixture.Project.ID, Limit: 37}
				if filter.name == "label" {
					selection.Labels = []string{filter.value}
				} else {
					selection.Assignees = []string{filter.value}
				}
				seen = map[tracker.NativeWorkItemID]bool{}
				for {
					var rows []viewstore.BoardRecord
					if err := fixture.Hub.Repository.ReadTenant(t.Context(), fixture.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						rows, err = r.BoardPage(t.Context(), selection)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						group, ok := want[fixture.Project.ID][row.WorkItemID]
						if !ok || seen[row.WorkItemID] || row.ProjectID != fixture.Project.ID || filter.value == "known" && group != "known" || filter.value == "unknown" && group != "unknown" {
							t.Fatalf("unexpected or duplicate filtered board row=%+v", row)
						}
						seen[row.WorkItemID] = true
					}
					if len(rows) < selection.Limit {
						break
					}
					position := rows[len(rows)-1].Position
					selection.After = &position
				}
				if len(seen) != count {
					t.Fatalf("board filtered count=%d want=%d", len(seen), count)
				}
			})
		}
	}
}
