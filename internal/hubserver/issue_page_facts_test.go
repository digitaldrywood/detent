package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssuePageFactsUseTypedPostgresOwners(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "example-library")
	for _, fixture := range []nativeFixture{f, other} {
		response := performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/work-items", fixture.token, tracker.CreateIssue{
			Mutation: tracker.Mutation{IdempotencyKey: "typed-page-facts"}, Title: "Filterable issue", Body: issueContractTestSections, Type: "bug", State: "Todo", Priority: new(2),
			Labels: []string{"Example-Label", "EXAMPLE-LABEL"}, Assignees: []string{"Example-Assignee", "EXAMPLE-ASSIGNEE"},
		})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		response = performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", fixture.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "typed-page-facts-done"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
		drainTestNativeBoard(t, fixture.service)
		refreshNativeDuplicateSearchFixture(t, fixture.service, fixture.project.OrganizationID)
		if err := fixture.service.config.TenantRepository.ReadTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			stored, err := r.WorkItem(t.Context(), fixture.project.ID, issue.WorkItemID)
			if err != nil {
				return err
			}
			if stored.ActorKind == nil || *stored.ActorKind != issue.Actor.Kind || stored.ActorPrincipalID == nil || *stored.ActorPrincipalID != issue.Actor.PrincipalID || stored.IssueType != "bug" || stored.Priority == nil || *stored.Priority != 2 {
				t.Fatalf("typed source facts=%+v actor=%+v", stored, issue.Actor)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, filter := range []struct {
			name, value string
			want        int
		}{
			{"created_by", issue.Actor.PrincipalID, 1}, {"type", "bug", 1}, {"verification", "pending", 1}, {"q", "example-label", 1},
			{"label", "Example-Label", 1}, {"label", "EXAMPLE-LABEL", 1}, {"assignee", "Example-Assignee", 1}, {"assignee", "EXAMPLE-ASSIGNEE", 1}, {"priority", "2", 1},
			{"label", "example-label", 0}, {"assignee", "example-assignee", 0}, {"label", " Example-Label ", 0}, {"assignee", " Example-Assignee ", 0},
		} {
			t.Run(string(fixture.project.ID)+"/"+filter.name+"="+filter.value, func(t *testing.T) {
				response := performHubAPIRequest(t, fixture.service, http.MethodGet, fixture.base+"/work-items?"+url.Values{filter.name: {filter.value}}.Encode(), fixture.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				decodeHubResponse(t, response, &page)
				if len(page.Items) != filter.want || filter.want == 1 && page.Items[0].WorkItemID != issue.WorkItemID {
					t.Fatalf("page=%+v want count=%d item=%s", page, filter.want, issue.WorkItemID)
				}
				if filter.want == 1 && page.Items[0].ClosedAt == nil {
					t.Fatalf("Done issue list projection lost closure: %+v", page.Items[0])
				}
				scope := hubscope.NativeScope{Organization: fixture.project.OrganizationID, Project: fixture.project.ID, SourceActor: &issue.Actor}
				selection, err := boardRecordSelection(scope, url.Values{filter.name: {filter.value}})
				if err != nil {
					t.Fatal(err)
				}
				selection.Limit = 10
				if err := fixture.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.BoardPage(t.Context(), selection)
					if err != nil {
						return err
					}
					if len(rows) != filter.want || filter.want == 1 && rows[0].WorkItemID != issue.WorkItemID {
						t.Fatalf("board %s=%q rows=%+v want count=%d", filter.name, filter.value, rows, filter.want)
					}
					if filter.want == 1 && (rows[0].Position.Lane == 2147483647 || rows[0].ClosedAt == nil) {
						t.Fatalf("typed Done issue lost lane or closure: %+v", rows[0])
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
		for _, filter := range []struct{ name, value string }{{"created_by", issue.Actor.PrincipalID}, {"type", "bug"}, {"label", "Example-Label"}, {"assignee", "Example-Assignee"}, {"reporter", ""}, {"reporter foreign", ""}} {
			scope := hubscope.NativeScope{Organization: fixture.project.OrganizationID, Project: fixture.project.ID, SourceActor: &issue.Actor}
			params := url.Values{}
			want := 1
			if strings.HasPrefix(filter.name, "reporter") {
				scope.Credential.HostedRole = "reporter"
				if filter.name == "reporter foreign" {
					scope.SourceActor = &tracker.Actor{Kind: "human", PrincipalID: "another-actor"}
					want = 0
				}
			} else {
				params.Set(filter.name, filter.value)
			}
			selection, err := boardRecordSelection(scope, params)
			if err != nil {
				t.Fatal(err)
			}
			at := fixture.service.config.Clock().Add(time.Second)
			if err := fixture.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				lanes, err := r.BoardLanes(t.Context(), selection, at)
				if err != nil {
					return err
				}
				if len(lanes) != want || want == 1 && lanes[0].Total != 1 {
					t.Fatalf("%s lanes=%+v want=%d", filter.name, lanes, want)
				}
				windows, err := r.BoardClosedWindows(t.Context(), selection, []time.Time{at.Add(-time.Hour), at.Add(-24 * time.Hour), at.Add(-7 * 24 * time.Hour)}, at)
				if err != nil {
					return err
				}
				counts := [3]int64{}
				for _, window := range windows {
					if window.State != "Done" || window.Window < 0 || window.Window >= len(counts) {
						t.Fatalf("%s unexpected shipped/cancelled window=%+v", filter.name, window)
					}
					counts[window.Window] += window.Total
				}
				if counts != [3]int64{int64(want), int64(want), int64(want)} {
					t.Fatalf("%s shipped windows=%v want=%d each; cancelled must remain zero", filter.name, counts, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		reporter := hubscope.NativeScope{Organization: fixture.project.OrganizationID, Project: fixture.project.ID, SourceActor: &issue.Actor, Credential: hubapi.APICredential{HostedRole: "reporter"}}
		if _, _, err := fixture.service.readWorkItemRecord(t.Context(), reporter, string(issue.WorkItemID)); err != nil {
			t.Fatalf("filer denied own item: %v", err)
		}
		reporter.SourceActor = &tracker.Actor{Kind: "human", PrincipalID: "another-actor"}
		_, _, err := fixture.service.readWorkItemRecord(t.Context(), reporter, string(issue.WorkItemID))
		var denied *hubapi.NativeError
		if !errors.As(err, &denied) || denied.Status != http.StatusNotFound {
			t.Fatalf("foreign reporter item authorization=%v want not found", err)
		}
		assertIssuePageNotificationParticipants(t, fixture, issue, tracker.Actor{Kind: "human", PrincipalID: "commenter_" + string(issue.WorkItemID)}, true)
	}
}

func TestIssuePageFiltersPreservePopulationAndProjectScope(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "example-library")
	want := map[tracker.ProjectID]map[tracker.NativeWorkItemID]string{}
	for _, fixture := range []nativeFixture{f, other} {
		if err := fixture.service.config.TenantRepository.WriteTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), fixture.project.ID)
			if err != nil {
				return err
			}
			record.Settings.DuplicateChallenge = false
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		want[fixture.project.ID] = map[tracker.NativeWorkItemID]string{}
		for index := range 103 {
			group := "unknown"
			if index == 100 {
				group = "known"
			}
			response := performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/work-items", fixture.token, tracker.CreateIssue{
				Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("filter-population-%d", index)}, Title: fmt.Sprintf("Filter population %d", index), Body: issueContractTestSections, State: "Todo",
				Labels:    []string{"Common-Label", "common-label", "label'quote", "label-four", "label-five", group},
				Assignees: []string{"Common-Assignee", "common-assignee", "assignee'quote", "assignee-four", "assignee-five", group},
			})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			want[fixture.project.ID][issue.WorkItemID] = group
		}
	}
	drainTestNativeBoard(t, f.service)
	for _, fixture := range []nativeFixture{f, other} {
		for _, filter := range []struct{ name, value string }{
			{"label", "Common-Label"}, {"label", "label'quote"}, {"label", "known"}, {"label", "unknown"},
			{"assignee", "Common-Assignee"}, {"assignee", "assignee'quote"}, {"assignee", "known"}, {"assignee", "unknown"},
		} {
			t.Run(string(fixture.project.ID)+"/"+filter.name+"="+filter.value, func(t *testing.T) {
				params := url.Values{filter.name: {filter.value}, "limit": {"37"}}
				seen := map[tracker.NativeWorkItemID]bool{}
				for {
					response := performHubAPIRequest(t, fixture.service, http.MethodGet, fixture.base+"/work-items?"+params.Encode(), fixture.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var page tracker.NativeIssuePage
					decodeHubResponse(t, response, &page)
					for _, issue := range page.Items {
						group, ok := want[fixture.project.ID][issue.WorkItemID]
						if !ok || seen[issue.WorkItemID] || issue.ProjectID != fixture.project.ID || filter.value == "known" && group != "known" || filter.value == "unknown" && group != "unknown" {
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
				if filter.value == "known" {
					count = 1
				} else if filter.value == "unknown" {
					count = 102
				}
				if len(seen) != count {
					t.Fatalf("filtered count=%d want=%d", len(seen), count)
				}
				selection := viewstore.BoardSelection{ProjectID: fixture.project.ID, Limit: 37}
				if filter.name == "label" {
					selection.Labels = []string{filter.value}
				} else {
					selection.Assignees = []string{filter.value}
				}
				seen = map[tracker.NativeWorkItemID]bool{}
				for {
					var rows []viewstore.BoardRecord
					if err := fixture.service.config.TenantRepository.ReadTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						rows, err = r.BoardPage(t.Context(), selection)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						group, ok := want[fixture.project.ID][row.WorkItemID]
						if !ok || seen[row.WorkItemID] || row.ProjectID != fixture.project.ID || filter.value == "known" && group != "known" || filter.value == "unknown" && group != "unknown" {
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

func TestBlockerHistoryUsesTypedPostgresFacts(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	for index := range 2 {
		issue := f.create(t, fmt.Sprintf("Blocker history %d", index))
		appendTransition := func(t *testing.T, typed func(*eventstore.Event)) eventstore.Event {
			t.Helper()
			value := tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: f.service.config.Clock(), Data: tracker.CollaborationData{Revision: 2, FromState: "Todo", ToState: "Done", Reason: "Persisted reason", Run: &tracker.NativeRunData{AttemptID: "persisted-attempt", FencingToken: 3}}}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			record, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
			if err != nil {
				t.Fatal(err)
			}
			typed(&record)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				record, err = w.AppendEvent(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return record
		}
		for _, stage := range []string{"persisted", "typed facts differ"} {
			t.Run(fmt.Sprintf("item-%d/%s", index, stage), func(t *testing.T) {
				revision, from, to := tracker.Revision(2), "Todo", "Done"
				if stage == "typed facts differ" {
					revision, from, to = 7, "Backlog", "Cancelled"
				}
				record := appendTransition(t, func(record *eventstore.Event) {
					record.EventRevision, record.FromState, record.ToState = int64(revision), from, to
				})
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/history?view=blockers", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.Page[tracker.CollaborationEvent]
				decodeHubResponse(t, response, &page)
				found := slices.IndexFunc(page.Items, func(event tracker.CollaborationEvent) bool { return event.ID == record.ID })
				if found < 0 {
					t.Fatalf("persisted transition absent from blocker history=%+v", page)
				}
				data := page.Items[found].Data
				if data.Revision != revision || data.FromState != from || data.ToState != to || data.Reason != "Persisted reason" || data.Run == nil || data.Run.AttemptID != "persisted-attempt" || data.Run.FencingToken != 3 {
					t.Fatalf("blocker data=%+v want revision=%d from=%s to=%s", data, revision, from, to)
				}
			})
		}
		record := appendTransition(t, func(record *eventstore.Event) { record.AttemptID = "corrupt-attempt" })
		var corrupted eventstore.Event
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.Events(t.Context(), f.project.ID, issue.WorkItemID, record.Sequence-1, 1)
			if err != nil {
				return err
			}
			if len(rows) != 1 || rows[0].ID != record.ID || rows[0].AttemptID != "corrupt-attempt" {
				t.Fatalf("typed event mismatch fixture did not persist: %+v", rows)
			}
			corrupted = rows[0]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.service.readTenantCollaborationEvent(t.Context(), corrupted); !errors.Is(err, artifact.ErrIntegrity) {
			t.Fatalf("corrupt typed attempt was served: %v", err)
		}
	}
}

func TestIssuePageHistoryReadsPastStorageConversionBudget(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	addedStates := 50 - len(f.project.States)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		for index := range addedStates {
			record.States = append(record.States, tracker.NativeState{Name: fmt.Sprintf("example-state-%d", index), Dispatchable: true, Transitions: []string{"Todo"}})
		}
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := r.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		for index := range addedStates {
			name := fmt.Sprintf("example-state-%d", index)
			if !slices.ContainsFunc(record.States, func(state tracker.NativeState) bool { return state.Name == name }) {
				t.Fatalf("workflow rank population truncated before %s: states=%d", name, len(record.States))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	actor := tracker.Actor{Kind: "human", PrincipalID: strings.Repeat("example-", 1500)}
	for itemIndex := range 2 {
		name := fmt.Sprintf("Large event history %d", itemIndex)
		state := fmt.Sprintf("example-state-%d", addedStates-1)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: state})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		drainTestNativeBoard(t, f.service)
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: f.project.ID, WorkItemIDs: []tracker.NativeWorkItemID{issue.WorkItemID}, Limit: 1})
			if err != nil {
				return err
			}
			if len(rows) != 1 || rows[0].WorkItemID != issue.WorkItemID || rows[0].Position.State != state || rows[0].Position.Lane != len(f.project.States)+addedStates-1 || rows[0].Position.Lane == 2147483647 {
				t.Fatalf("last workflow state has no usable board rank: %+v", rows)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateLinkedSource(t.Context(), eventstore.LinkedSource{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: issue.WorkItemID, URL: fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", itemIndex+1)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{}
		transitions := map[string]bool{}
		through := int64(0)
		for index := range storageConversionRows + 3 {
			for _, kind := range []string{"issue.edited", "comment.created", "workflow.transitioned"} {
				value := tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: kind, SchemaVersion: 1, Actor: actor, RecordedAt: f.service.config.Clock().Add(time.Duration(index) * time.Microsecond), Data: tracker.CollaborationData{Revision: tracker.Revision(index + 2), FromState: "Todo", ToState: "Done", Reason: strings.Repeat("x", 11000)}}
				if index == storageConversionRows+2 {
					value.Data.Fields = []string{"title", "body"}
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				record, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					record, err = w.AppendEvent(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				through = record.Sequence
				want[value.ID] = len(value.Data.Fields) != 0
				if kind == "workflow.transitioned" {
					transitions[value.ID] = true
				}
			}
		}
		seenTransitions := map[string]bool{}
		cursor := reportstore.HistoryEventCursor{Through: through}
		for {
			var page reportstore.HistoryEventPage
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(reportstore.HistoryIssuePageReader).ReadHistoryIssueEvents(t.Context(), f.project.ID, issue.WorkItemID, cursor, 37)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, event := range page.Events {
				if !transitions[event.ID] || seenTransitions[event.ID] || event.FromState != "Todo" || event.ToState != "Done" {
					t.Fatalf("typed transition facts lost source or duplicated history: %+v", event)
				}
				seenTransitions[event.ID] = true
			}
			if page.Exhausted {
				break
			}
			if page.After <= cursor.After {
				t.Fatalf("typed transition page did not advance: before=%+v page=%+v", cursor, page)
			}
			cursor.After = page.After
		}
		if len(seenTransitions) != storageConversionRows+3 {
			t.Fatalf("typed transition facts=%d want=%d", len(seenTransitions), storageConversionRows+3)
		}
		params := url.Values{"limit": {"17"}}
		seen := map[string]bool{}
		lateEdit := false
		for {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/history?"+params.Encode(), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, response, &page)
			for _, event := range page.Items {
				if event.Type == "issue.created" {
					continue
				}
				edited, ok := want[event.ID]
				if !ok || seen[event.ID] || event.AggregateID != issue.WorkItemID || event.ProjectID != f.project.ID {
					t.Fatalf("unexpected or duplicate history event=%+v", event)
				}
				if event.Actor.PrincipalID != actor.PrincipalID || event.Data.Reason != strings.Repeat("x", 11000) || edited && (!slices.Contains(event.Data.Fields, "title") || !slices.Contains(event.Data.Fields, "body")) {
					t.Fatalf("history data lost after page boundary=%+v", event)
				}
				lateEdit = lateEdit || edited
				seen[event.ID] = true
			}
			if page.NextCursor == "" {
				break
			}
			params.Set("cursor", page.NextCursor)
		}
		if len(seen) != len(want) || !lateEdit {
			t.Fatalf("history count=%d want=%d late edit=%t", len(seen), len(want), lateEdit)
		}
		assertIssuePageNotificationParticipants(t, f, issue, actor, true)
		for editIndex, fields := range []struct{ title, body string }{{"Native title", "Native body" + issueContractTestSections}, {issue.Title, issue.Body}} {
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("ownership-%d", editIndex)}, ExpectedRevision: issue.Revision, Title: &fields.title, Body: &fields.body})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				source, err := r.LinkedSource(t.Context(), f.project.ID, issue.WorkItemID)
				if err == nil && (!source.TitleOwned || !source.BodyOwned) {
					t.Fatalf("native edit command lost persistent ownership: %+v", source)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func assertIssuePageNotificationParticipants(t *testing.T, f nativeFixture, issue tracker.NativeIssue, commenter tracker.Actor, createComment bool) {
	t.Helper()
	roles := map[string]bool{}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for index, actor := range []tracker.Actor{issue.Actor, commenter} {
			digest := sha256.Sum256([]byte(actor.PrincipalID))
			user := fmt.Sprintf("page_participant_%x", digest[:8])
			if _, err := w.AccessCredential(t.Context(), actor.PrincipalID); errors.Is(err, corestore.ErrNotFound) {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: actor.PrincipalID, Name: "Example participant " + actor.PrincipalID, Hash: fmt.Sprintf("%x", digest), Scope: "operator", Kind: "personal", ProjectAccess: "selected", CreatedAt: f.service.config.Clock(), UpdatedAt: f.service.config.Clock()}); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			member, err := w.AccessMember(t.Context(), user)
			if errors.Is(err, corestore.ErrNotFound) {
				member = accessstore.Member{OrganizationID: f.project.OrganizationID, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: "member", Active: true, PrincipalID: actor.PrincipalID, NotifyIssues: true, CreatedAt: f.service.config.Clock(), UpdatedAt: f.service.config.Clock()}
				if err := w.CreateAccessMember(t.Context(), member); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: f.project.OrganizationID, UserID: user, ProjectID: f.project.ID}); err != nil {
				return err
			}
			roles[member.Email] = index == 0
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	appendEvent := func(kind string, actor tracker.Actor) eventstore.Event {
		t.Helper()
		value := tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: kind, SchemaVersion: 1, Actor: actor, RecordedAt: f.service.config.Clock(), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Done"}}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		record, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			record, err = w.AppendEvent(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return record
	}
	if createComment {
		appendEvent("comment.created", commenter)
	}
	event := appendEvent("workflow.transitioned", tracker.Actor{Kind: "machine", PrincipalID: "notification-example"})
	owner, ok := f.service.config.TenantRepository.(accountstore.NotificationRepository)
	if !ok {
		t.Fatal("PostgreSQL issue notification owner is unavailable")
	}
	for after := int64(0); after < event.Sequence-1; {
		var earlier []eventstore.Event
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			earlier, err = r.Events(t.Context(), f.project.ID, issue.WorkItemID, after, 100)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(earlier) == 0 {
			t.Fatalf("events before sequence %d ended at %d", event.Sequence, after)
		}
		for _, record := range earlier {
			if record.Sequence >= event.Sequence {
				break
			}
			if done, _, err := owner.QueueNotification(t.Context(), f.project.OrganizationID, record, accountstore.IssueNotice{}, "", false, 256); err != nil || !done {
				t.Fatalf("advance participants past sequence %d complete=%t error=%v", record.Sequence, done, err)
			}
			after = record.Sequence
		}
	}
	complete, _, err := owner.QueueNotification(t.Context(), f.project.OrganizationID, event, accountstore.IssueNotice{StateTo: "Done", Verify: true}, "", true, 256)
	if err != nil || !complete {
		t.Fatalf("queue participants complete=%t error=%v", complete, err)
	}
	seen := map[string]bool{}
	cursor := accountstore.NotificationCursor{}
	for {
		page, err := owner.DueNotification(t.Context(), f.project.OrganizationID, event.RecordedAt.Add(time.Hour), cursor, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !page.Found {
			if !page.More {
				break
			}
			cursor = page.Next
			continue
		}
		if page.Candidate.ProjectID != "" && page.Candidate.ProjectID != f.project.ID || page.Candidate.WorkItemID != issue.WorkItemID {
			t.Fatalf("participant notification crossed project or item: %+v", page.Candidate)
		}
		notice, unresolved, claimed, err := owner.ClaimNotification(t.Context(), f.project.OrganizationID, event.RecordedAt.Add(time.Hour), page.Candidate, true)
		if err == nil && !claimed && unresolved.Verify {
			if unresolved.ProjectID != f.project.ID || unresolved.WorkItemID != issue.WorkItemID {
				t.Fatalf("participant verification crossed project or item: %+v", unresolved)
			}
			notice, _, claimed, err = owner.ClaimNotification(t.Context(), f.project.OrganizationID, event.RecordedAt.Add(time.Hour), unresolved, true)
		}
		if err != nil || !claimed {
			t.Fatalf("claim participant notice claimed=%t error=%v", claimed, err)
		}
		filer, exists := roles[notice.Email]
		if !exists || seen[notice.Email] || len(notice.Notices) != 1 || notice.Notices[0].Verify != filer {
			t.Fatalf("missing participant, duplicate, or incorrect filer role: notice=%+v expected=%v", notice, roles)
		}
		seen[notice.Email] = true
	}
	if len(seen) != 2 {
		t.Fatalf("project %s filer/commenter participants=%v want=%v", f.project.ID, seen, roles)
	}
}

func TestBlockerHistoryUsesTypedPageFacts(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"unknown", "typed"} {
		t.Run(stage, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			source := coreWorkItemNeutralFixture(t, f.project.OrganizationID, f.project.ID, false)
			for _, suffix := range []string{"", "_second"} {
				item := "wi_core_import" + suffix
				actor, err := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: "example-owner"})
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(tracker.CollaborationData{Revision: 2, FromState: "Todo", ToState: "Done", Reason: "opaque reason", Run: &tracker.NativeRunData{AttemptID: "opaque-attempt", FencingToken: 3}})
				if err != nil {
					t.Fatal(err)
				}
				fields := map[string]any{"id": "event_corrected" + suffix, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item, "sequence": 3, "type": "workflow.transitioned", "schema_version": 1, "actor_json": string(actor), "data_json": string(data), "recorded_at": "2026-10-02T01:00:00Z", "event_revision": nil}
				if stage == "typed" {
					for name, value := range map[string]any{"page_projection_ready": 1, "event_revision": 7, "from_state": "Backlog", "to_state": "Cancelled", "reason": "typed reason", "run_present": 1, "run_attempt_id": "typed-attempt", "run_fencing_token": 9} {
						fields[name] = value
					}
				}
				source.add("collaboration_events", fields)
				source.set("issues", map[string]string{"native_id": item}, map[string]any{"event_sequence": 3})
			}
			count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()})
			if err != nil || count != 2 {
				t.Fatalf("historical blocker source import count=%d error=%v", count, err)
			}
			for _, suffix := range []string{"", "_second"} {
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/wi_core_import"+suffix+"/history?view=blockers", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.Page[tracker.CollaborationEvent]
				decodeHubResponse(t, response, &page)
				index := slices.IndexFunc(page.Items, func(event tracker.CollaborationEvent) bool { return event.ID == "event_corrected"+suffix })
				if index < 0 {
					t.Fatalf("historical blocker transition missing: %+v", page)
				}
				revision, from, to, reason, attempt, fence := tracker.Revision(2), "Todo", "Done", "opaque reason", "opaque-attempt", tracker.FencingToken(3)
				if stage == "typed" {
					revision, from, to, reason, attempt, fence = 7, "Backlog", "Cancelled", "typed reason", "typed-attempt", 9
				}
				data := page.Items[index].Data
				if data.Revision != revision || data.FromState != from || data.ToState != to || data.Reason != reason || data.Run == nil || data.Run.AttemptID != attempt || data.Run.FencingToken != fence {
					t.Fatalf("historical blocker corrections lost: %+v", data)
				}
			}
		})
	}
}

func TestIssuePagePullRequestBackfill(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"insert", "update", "delete", "concurrent update"} {
		for _, projection := range []string{"typed", "unknown", "typed URL differs"} {
			t.Run(operation+"/"+projection, func(t *testing.T) {
				now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
				var duringFetch func()
				calls := 0
				sourceAt := now
				number := 1
				canonicalNumber := number
				if projection == "typed URL differs" {
					canonicalNumber = 42
				}
				backend := pullRequestRefreshBackend(func(_ context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
					calls++
					if operation == "delete" {
						return ReconcileSnapshot{}, Permanent(corestore.ErrNotFound)
					}
					if !request.TargetedPullRequests || len(request.Hydrations) != 1 || request.Hydrations[0].GitHubNumber != number {
						t.Fatalf("refresh did not use requested PR identity: %+v", request)
					}
					if duringFetch != nil {
						hook := duringFetch
						duringFetch = nil
						hook()
					}
					head := request.Hydrations[0].HeadSHA
					return ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_example", Owner: "example-org", Name: "example-project"}, PullRequests: []PullRequestSource{{Number: number, HeadSHA: head, State: "open", UpdatedAt: sourceAt}}, PullRequestDetails: []PullRequestDetailSource{{Number: number, HeadSHA: head, Checks: &tracker.CheckSummary{}, Reviews: &tracker.ReviewSummary{}}}}, nil
				})
				f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }, ReconcileBackend: backend})
				adminValue, ok := projectRecordDatabases.Load(f.service.database.postgres)
				if !ok {
					t.Fatal("fixture PostgreSQL administration connection unavailable")
				}
				admin := adminValue.(*sql.DB)
				first, linked, unrelated := f.create(t, "First linked example"), f.create(t, "Second linked example"), f.create(t, "Unrelated example")
				write := func(fn func(tenantstore.Writer) error) {
					t.Helper()
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, fn); err != nil {
						t.Fatal(err)
					}
				}
				write(func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = "example-org/example-project", true
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "linked-change"}, Title: "Linked change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				input := changeTestInput()
				input.Repository = "https://github.com/example-org/example-project"
				input.External = &tracker.ChangeExternalReference{Provider: "github", ID: fmt.Sprint(canonicalNumber), URL: fmt.Sprintf("https://github.com/example-org/example-project/pull/%d", canonicalNumber)}
				content := eventstore.NewContent(f.service.runDetailStore())
				source, err := content.PrepareSourceBundle(t.Context(), f.project.OrganizationID, changeTestSource(t, &input))
				if err != nil {
					t.Fatal(err)
				}
				version := tracker.ChangeVersion{ID: "version_linked", ChangeID: change.ID, Number: 1, ChangeVersionInput: input, Policy: hubTestPolicy(), Actor: first.Actor, CreatedAt: now}
				version.PolicyID = version.Policy.ID
				raw, err := json.Marshal(version)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := content.PrepareVersion(t.Context(), f.project.OrganizationID, raw)
				if err != nil {
					t.Fatal(err)
				}
				stored.SourceObjectKey, stored.SourceSHA256, stored.SourceBytes, stored.SourceContentBytes = source.Key, source.SHA256, source.CompressedSize, source.UncompressedSize
				write(func(w tenantstore.Writer) error {
					if _, err := w.CreateVersion(t.Context(), stored); err != nil {
						return err
					}
					row, err := w.Change(t.Context(), change.ID)
					if err != nil {
						return err
					}
					row.CurrentVersionID = new(stored.ID)
					_, err = w.UpdateChange(t.Context(), row)
					return err
				})
				if projection == "typed URL differs" {
					tx, err := admin.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err := tx.ExecContext(t.Context(), "ALTER TABLE change_versions DISABLE TRIGGER change_versions_immutable"); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "UPDATE change_versions SET external_url='https://github.com/example-org/example-project/pull/1' WHERE organization_id=$1 AND change_id=$2 AND id=$3", f.project.OrganizationID, change.ID, stored.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "ALTER TABLE change_versions ENABLE TRIGGER change_versions_immutable"); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						typed, err := r.WorkItemPullRequestVersion(t.Context(), f.project.ID, first.WorkItemID)
						if err != nil {
							return err
						}
						if typed.External.URL != "https://github.com/example-org/example-project/pull/1" {
							t.Fatalf("typed URL disagreement fixture lost: %+v", typed)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					canonical, err := content.Version(t.Context(), stored)
					if err != nil || canonical.External == nil || canonical.External.URL != "https://github.com/example-org/example-project/pull/42" {
						t.Fatalf("immutable PR identity changed: %+v %v", canonical.External, err)
					}
				}
				request := projectstore.GitHubPullRequestRefresh{GitHubPullRequestRefreshKey: projectstore.GitHubPullRequestRefreshKey{ProjectID: f.project.ID, ChangeID: change.ID, Repository: "example-org/example-project", Number: number}, WorkItemID: linked.WorkItemID, RequestedVersion: stored.ID, RequestedHeadSHA: input.HeadSHA, RequestedAt: now}
				queue := func() {
					write(func(w tenantstore.Writer) error {
						return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), []projectstore.GitHubPullRequestRefresh{request})
					})
				}
				baseline := map[tracker.NativeWorkItemID]tracker.NativeIssue{}
				for _, item := range []tracker.NativeIssue{first, linked, unrelated} {
					baseline[item.WorkItemID] = readWorkItem(t, f, item.WorkItemID, "")
				}
				drainTestNativeBoard(t, f.service)
				if operation == "update" {
					queue()
					for range 2 {
						if err := f.service.maintainGitHubPullRequestRefresh(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					drainTestNativeBoard(t, f.service)
				}
				readRefresh := func() projectstore.GitHubPullRequestRefresh {
					t.Helper()
					var row projectstore.GitHubPullRequestRefresh
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						row, err = r.(projectstore.GitHubPullRequestRefreshReader).GitHubPullRequestRefresh(t.Context(), request.GitHubPullRequestRefreshKey)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return row
				}
				if projection == "unknown" {
					if operation != "update" {
						queue()
						observation := projectstore.GitHubPullRequestObservation{Repository: request.Repository, Number: number, HeadSHA: request.RequestedHeadSHA, SourceUpdatedAt: now, ObservedAt: now, State: "open"}
						for range 2 {
							current := readRefresh()
							write(func(w tenantstore.Writer) error {
								return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), current, observation)
							})
						}
					}
					if _, err := admin.ExecContext(t.Context(), "UPDATE github_pull_request_refresh SET observed_request_count=NULL,pending=true,work_item_id=$4 WHERE organization_id=$1 AND project_id=$2 AND change_id=$3", f.project.OrganizationID, f.project.ID, change.ID, max(first.WorkItemID, linked.WorkItemID)); err != nil {
						t.Fatal(err)
					}
					if legacy := readRefresh(); legacy.ObservedRequestCount != 0 || legacy.Observation == nil {
						t.Fatalf("legacy unknown generation fixture lost: %+v", legacy)
					}
					before := readRefresh()
					beforeCalls := calls
					if err := f.service.maintainGitHubPullRequestRefresh(t.Context()); err != nil {
						t.Fatal(err)
					}
					reset := readRefresh()
					if calls != beforeCalls || reset.WorkItemID != min(first.WorkItemID, linked.WorkItemID) || reset.RequestCount != before.RequestCount+1 || reset.ObservedRequestCount != 0 || reset.Observation == nil {
						t.Fatalf("legacy target was not restarted without reusing uncertain generation: %+v", reset)
					}

				}
				now = now.Add(time.Hour)
				sourceAt = now.Add(time.Minute)
				if projection == "unknown" {
					sourceAt = now.Add(-time.Minute)
				}
				want := now
				if sourceAt.After(want) {
					want = sourceAt
				}
				if projection != "unknown" {
					queue()
				}
				if operation == "concurrent update" {
					duringFetch = func() { sourceAt = sourceAt.Add(time.Minute); want = sourceAt; queue() }
					if err := f.service.maintainGitHubPullRequestRefresh(t.Context()); !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("concurrent request accepted stale completion: %v", err)
					}
					for _, item := range []tracker.NativeIssue{first, linked} {
						if got := readWorkItemWithoutBoardRefresh(t, f, item.WorkItemID, ""); !got.LastActivityAt.Equal(baseline[item.WorkItemID].LastActivityAt) {
							t.Fatalf("stale request changed activity: %+v", got)
						}
					}
				}
				beforeCalls := calls
				for step := range 2 {
					err := f.service.maintainGitHubPullRequestRefresh(t.Context())
					if operation == "delete" && step == 0 {
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("deleted source completion=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					committed := 0
					for _, item := range []tracker.NativeIssue{first, linked} {
						if got := readWorkItemWithoutBoardRefresh(t, f, item.WorkItemID, ""); got.LastActivityAt.Equal(want) {
							committed++
						}
					}
					if operation != "delete" && committed != step+1 {
						t.Fatalf("activity did not commit one issue per turn: committed=%d step=%d", committed, step)
					}
				}
				if operation != "delete" && calls-beforeCalls != 1 {
					t.Fatalf("continuation refetched source: calls=%d", calls-beforeCalls)
				}
				if operation != "delete" {
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var attempted []tracker.NativeWorkItemID
						for range 2 {
							candidate, err := r.NextBoardCandidate(t.Context(), "", attempted...)
							if err != nil {
								return err
							}
							if candidate.QueuedAt == nil || candidate.WorkItemID != first.WorkItemID && candidate.WorkItemID != linked.WorkItemID {
								t.Fatalf("board refresh lost: %+v", candidate)
							}
							attempted = append(attempted, candidate.WorkItemID)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				drainTestNativeBoard(t, f.service)
				for _, item := range []tracker.NativeIssue{first, linked, unrelated} {
					expected := baseline[item.WorkItemID].LastActivityAt
					if operation != "delete" && item.WorkItemID != unrelated.WorkItemID {
						expected = want
					}
					issue := readWorkItem(t, f, item.WorkItemID, "")
					var board viewstore.BoardRecord
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						board, err = r.BoardItem(t.Context(), f.project.ID, item.WorkItemID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					card, err := viewstore.NewContent(f.service.runDetailStore()).BoardRecord(t.Context(), board)
					if err != nil {
						t.Fatal(err)
					}
					if !issue.LastActivityAt.Equal(expected) || !card.Issue.LastActivityAt.Equal(expected) || !issue.UpdatedAt.Equal(baseline[item.WorkItemID].UpdatedAt) || issue.Revision != baseline[item.WorkItemID].Revision {
						t.Fatalf("issue=%s activity=%s board=%s updated=%s want activity=%s", item.WorkItemID, issue.LastActivityAt, card.Issue.LastActivityAt, issue.UpdatedAt, expected)
					}
				}
			})
		}
	}
}
func TestLegacyPageEventsReadPastConversionBudget(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	source := coreWorkItemNeutralFixture(t, f.project.OrganizationID, f.project.ID, false)
	linked := newCoreNeutralFixture(t, f.project.OrganizationID, "linked_issue_sources")
	source.tables["linked_issue_sources"] = linked.tables["linked_issue_sources"]
	source.add("workflow_states", map[string]any{"id": 2, "project_id": f.project.ID, "detent_state": "Done", "terminal": 1})
	actor := tracker.Actor{Kind: "human", PrincipalID: strings.Repeat("example-", 1500)}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}
	const created = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	closed := parseTestTime(t, updated)
	for itemIndex, suffix := range []string{"", "_second"} {
		item := "wi_core_import" + suffix
		url := fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", itemIndex+1)
		var snapshotJSON any
		if itemIndex == 0 {
			raw, err := json.Marshal(tracker.GitHubIssueSnapshot{URL: url, Title: "Historical source title", Body: "Historical source body", Provenance: tracker.Provenance{Provider: "github", ExternalID: "historical-source", AuthorID: "source-author", CreatedAt: closed, UpdatedAt: closed, ObservedAt: closed}})
			if err != nil {
				t.Fatal(err)
			}
			snapshotJSON = string(raw)
		}
		source.add("linked_issue_sources", map[string]any{"work_item_id": item, "source_url": url, "title_supplied": false, "body_supplied": false, "snapshot_json": snapshotJSON})
		current := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: tracker.NativeWorkItemID(item), Number: 1 + itemIndex*2, Revision: 2, Profile: "native"}, Title: "Updated title", Body: coreImportBody(), State: "Done", Terminal: true, ClosedAt: &closed, Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: parseTestTime(t, created), UpdatedAt: closed, LastActivityAt: closed, LinkedSource: &tracker.LinkedIssueSource{URL: url, Status: "pending"}}
		currentRaw, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		keys, values, err := nativeVersionObject(currentRaw)
		if err != nil {
			t.Fatal(err)
		}
		currentRaw, err = nativeVersionObjectBytes(keys, values)
		if err != nil {
			t.Fatal(err)
		}
		source.set("collaboration_versions", map[string]string{"record_id": item, "revision": "2"}, map[string]any{"encoding": "snapshot", "record_json": string(currentRaw)})
		entry, err := json.Marshal(tracker.CollaborationData{Revision: 2, FromState: "Todo", ToState: "Done"})
		if err != nil {
			t.Fatal(err)
		}
		source.add("collaboration_events", map[string]any{"id": "event_entered" + suffix, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item, "sequence": 3, "type": "workflow.transitioned", "schema_version": 1, "actor_json": string(actorJSON), "data_json": string(entry), "event_revision": 2, "recorded_at": updated})
		for index := range storageConversionRows + 3 {
			for kindIndex, kind := range []string{"issue.edited", "comment.created", "workflow.transitioned"} {
				data := tracker.CollaborationData{Revision: 2, Reason: strings.Repeat("x", 11000), FromState: "Todo", ToState: "Done"}
				if index == storageConversionRows+2 {
					data.Fields = []string{"title", "body"}
				}
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				fields := map[string]any{"id": fmt.Sprintf("event_late_%d_%d%s", index, kindIndex, suffix), "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item, "sequence": 4 + index*3 + kindIndex, "type": kind, "schema_version": 1, "actor_json": string(actorJSON), "data_json": string(raw), "event_revision": 2, "recorded_at": "2026-10-02T01:00:00Z"}
				if itemIndex == 1 && index == storageConversionRows+2 && kind == "issue.edited" {
					fields["page_projection_ready"], fields["edited_title"], fields["edited_body"] = true, true, false
				}
				source.add("collaboration_events", fields)
			}
		}
		source.set("issues", map[string]string{"native_id": item}, map[string]any{"event_sequence": 3 + 3*(storageConversionRows+3), "workflow_state_id": 2})
	}
	input := CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
	if count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 {
		t.Fatalf("past-budget historical source import count=%d error=%v", count, err)
	}
	drainTestNativeBoard(t, f.service)
	for itemIndex, suffix := range []string{"", "_second"} {
		id := tracker.NativeWorkItemID("wi_core_import" + suffix)
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			linked, err := r.LinkedSource(t.Context(), f.project.ID, id)
			if err != nil {
				return err
			}
			if linked.URL != fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", itemIndex+1) || linked.TitleSupplied || linked.BodySupplied || !linked.TitleOwned || linked.BodyOwned != (itemIndex == 0) {
				t.Fatalf("late historical edits lost ownership: %+v", linked)
			}
			if itemIndex == 0 {
				snapshot, err := eventstore.NewContent(f.service.runDetailStore()).LinkedIssueSnapshot(t.Context(), f.project.OrganizationID, eventstore.Reference(linked.SnapshotObjectKey, linked.SnapshotSHA256, linked.SnapshotBytes, linked.SnapshotContentBytes))
				if err != nil || snapshot.URL != linked.URL || snapshot.Title != "Historical source title" || snapshot.Body != "Historical source body" {
					t.Fatalf("historical snapshot lost: %+v error=%v", snapshot, err)
				}
			} else if linked.SnapshotObjectKey != "" {
				t.Fatalf("pending source invented a snapshot: %+v", linked)
			}
			rows, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: f.project.ID, WorkItemIDs: []tracker.NativeWorkItemID{id}, Limit: 1})
			if err != nil || len(rows) != 1 || rows[0].Position.State != "Done" || rows[0].Position.Lane == 2147483647 {
				t.Fatalf("historical terminal state has no usable rank: %+v error=%v", rows, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		cursor := reportstore.HistoryEventCursor{Through: int64(3 + 3*(storageConversionRows+3))}
		transitions := map[string]bool{}
		for {
			var page reportstore.HistoryEventPage
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(reportstore.HistoryIssuePageReader).ReadHistoryIssueEvents(t.Context(), f.project.ID, id, cursor, 37)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, event := range page.Events {
				if transitions[event.ID] || event.FromState != "Todo" || event.ToState != "Done" {
					t.Fatalf("historical transition facts lost or duplicated: %+v", event)
				}
				transitions[event.ID] = true
			}
			if page.Exhausted {
				break
			}
			if page.After <= cursor.After {
				t.Fatalf("historical transition page did not advance: %+v", page)
			}
			cursor.After = page.After
		}
		if len(transitions) != storageConversionRows+4 {
			t.Fatalf("historical transition facts=%d want=%d", len(transitions), storageConversionRows+4)
		}
		want := map[string]string{}
		for index := range storageConversionRows + 3 {
			for kindIndex, kind := range []string{"issue.edited", "comment.created", "workflow.transitioned"} {
				want[fmt.Sprintf("event_late_%d_%d%s", index, kindIndex, suffix)] = kind
			}
		}
		params := url.Values{"limit": {"17"}}
		seen := map[string]bool{}
		lateEdit := false
		for {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id)+"/history?"+params.Encode(), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, response, &page)
			for _, event := range page.Items {
				if !strings.HasPrefix(event.ID, "event_late_") {
					continue
				}
				if want[event.ID] != event.Type || seen[event.ID] || event.AggregateID != id || event.Actor.PrincipalID != actor.PrincipalID || event.Data.Reason != strings.Repeat("x", 11000) {
					t.Fatalf("historical event data lost or duplicated: %+v", event)
				}
				if event.ID == fmt.Sprintf("event_late_%d_0%s", storageConversionRows+2, suffix) {
					lateEdit = true
					if !slices.Contains(event.Data.Fields, "title") || slices.Contains(event.Data.Fields, "body") != (itemIndex == 0) {
						t.Fatalf("late edit field provenance lost: %+v", event.Data)
					}
				}
				seen[event.ID] = true
			}
			if page.NextCursor == "" {
				break
			}
			params.Set("cursor", page.NextCursor)
		}
		if len(seen) != 3*(storageConversionRows+3) || !lateEdit {
			t.Fatalf("historical event population=%d late edit=%t", len(seen), lateEdit)
		}
		issue := readWorkItem(t, f, id, "")
		if issue.ClosedAt == nil || !issue.ClosedAt.Equal(closed) {
			t.Fatalf("imported immutable closure changed: item=%s closed=%v want=%s", id, issue.ClosedAt, closed)
		}
		assertIssuePageNotificationParticipants(t, f, issue, actor, false)
	}
	var missingSource eventstore.LinkedSource
	var missingReceipt eventstore.Receipt
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		missingSource, err = r.LinkedSource(t.Context(), f.project.ID, "wi_core_import")
		if err != nil {
			return err
		}
		missingReceipt, err = r.Receipt(t.Context(), "production-import", "core-work-item", "41")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "DELETE FROM linked_issue_sources WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", f.project.OrganizationID, f.project.ID, "wi_core_import"); err != nil {
		t.Fatal(err)
	}
	if count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 {
		t.Fatalf("missing historical source replay count=%d error=%v", count, err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		linked, err := r.LinkedSource(t.Context(), f.project.ID, "wi_core_import")
		if err != nil {
			return err
		}
		if linked != missingSource {
			t.Fatalf("replay did not restore verified missing source: %+v", linked)
		}
		current, err := r.Receipt(t.Context(), "production-import", "core-work-item", "41")
		if err == nil && current != missingReceipt {
			t.Fatalf("missing source repair overwrote import receipt: %+v", current)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id := tracker.NativeWorkItemID("wi_core_import_second")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		linked, err := w.LinkedSource(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		linked.TitleOwned = false
		_, err = w.UpdateLinkedSource(t.Context(), linked)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	issue := readWorkItem(t, f, id, "")
	body := "Current native body" + issueContractTestSections
	snapshot := tracker.GitHubIssueSnapshot{URL: "https://github.com/example-org/example-project/issues/2", Title: "Newer source title", Body: "Newer source body", Provenance: tracker.Provenance{Provider: "github", ExternalID: "newer-source", AuthorID: "source-author", CreatedAt: closed, UpdatedAt: closed.Add(time.Hour), ObservedAt: closed.Add(time.Hour)}}
	ref, err := eventstore.NewContent(f.service.runDetailStore()).PrepareLinkedIssueSnapshot(t.Context(), f.project.OrganizationID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var receipt eventstore.Receipt
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(t.Context(), "production-import", "core-work-item", "43")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	interleaved := false
	store := input.Objects
	input.Objects = collaborationReadProbe{DetailStore: store, beforeRead: func(ctx context.Context, archived attachment.ArchiveReference) error {
		if interleaved {
			return nil
		}
		raw, err := store.ReadDetail(ctx, string(f.project.OrganizationID), archived)
		if err != nil {
			return err
		}
		var event tracker.CollaborationEvent
		if json.Unmarshal(raw, &event) != nil || event.ID != fmt.Sprintf("event_late_%d_2_second", storageConversionRows+2) {
			return nil
		}
		interleaved = true
		response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(id), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "native-body-during-import"}, ExpectedRevision: issue.Revision, Body: &body})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
		return f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			linked, err := w.LinkedSource(ctx, f.project.ID, id)
			if err != nil {
				return err
			}
			if linked.TitleOwned || !linked.BodyOwned {
				t.Fatalf("native body ownership fixture did not persist: %+v", linked)
			}
			linked.SnapshotObjectKey, linked.SnapshotSHA256, linked.SnapshotBytes, linked.SnapshotContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
			_, err = w.UpdateLinkedSource(ctx, linked)
			return err
		})
	}}
	if count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 || !interleaved {
		t.Fatalf("historical ownership replay count=%d interleaved=%t error=%v", count, interleaved, err)
	}
	input.Objects = store
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		linked, err := r.LinkedSource(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		if !linked.TitleOwned || !linked.BodyOwned || linked.SnapshotSHA256 != ref.SHA256 || linked.SnapshotObjectKey != ref.Key || linked.TitleSupplied || linked.BodySupplied {
			t.Fatalf("replay replaced native ownership or newer snapshot: %+v", linked)
		}
		current, err := r.Receipt(t.Context(), "production-import", "core-work-item", "43")
		if err == nil && current != receipt {
			t.Fatalf("replay overwrote import receipt: %+v", current)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	current := readWorkItem(t, f, id, "")
	if current.Revision != issue.Revision || current.Body != body {
		t.Fatalf("historical replay replaced a subsequent native edit: %+v", current)
	}
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE linked_issue_sources SET source_url=$1 WHERE organization_id=$2 AND project_id=$3 AND work_item_id=$4", "https://github.com/example-org/example-project/issues/999", f.project.OrganizationID, f.project.ID, id); err != nil {
		t.Fatal(err)
	}
	if count, err := ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); !errors.Is(err, mutation.ErrConflict) || count != 1 {
		t.Fatalf("unsupported replay association accepted: count=%d error=%v", count, err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		linked, err := r.LinkedSource(t.Context(), f.project.ID, id)
		if err == nil && (linked.URL != "https://github.com/example-org/example-project/issues/999" || !linked.TitleOwned || !linked.BodyOwned || linked.SnapshotSHA256 != ref.SHA256) {
			t.Fatalf("failed replay changed unrelated current source state: %+v", linked)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
