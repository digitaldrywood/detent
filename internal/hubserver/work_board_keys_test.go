package hubserver

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardSeedCard struct {
	id       tracker.NativeWorkItemID
	number   int
	state    string
	blockers []tracker.NativeDependency
}

func seedBoardCards(t *testing.T, service *Service, org tracker.OrganizationID, project tracker.ProjectID, cards ...boardSeedCard) {
	t.Helper()
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
		record, err := w.Project(t.Context(), project)
		if err != nil {
			return err
		}
		for _, card := range cards {
			terminal := card.state == "Done"
			stored, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: card.id, Number: int64(card.number), Title: "Example item", WorkflowStateName: new(card.state), ActorKind: new("human"), ActorPrincipalID: new("owner")})
			if err != nil {
				return err
			}
			ids := make([]tracker.NativeWorkItemID, len(card.blockers))
			for i, blocker := range card.blockers {
				ids[i] = blocker.ID
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: card.id, Number: card.number, Revision: 1, Profile: "native"}, Title: "Example item", State: card.state, Terminal: terminal,
				Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: ids, Blockers: slices.Clone(card.blockers), DependencyReady: len(card.blockers) == 0}
			if terminal {
				issue.ClosedAt = &now
			}
			payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				return err
			}
			row := viewstore.BoardRecord{OrganizationID: org, ProjectID: project, WorkItemID: card.id, SourceRevision: stored.Revision, SourceEventSequence: stored.EventSequence, SourceProjectRevision: record.ContentSettingsRevision,
				Position: viewstore.BoardPosition{State: card.state, Identifier: fmt.Sprintf("%s#%d", project, card.number), Activity: &now}, Terminal: terminal, ClosedAt: issue.ClosedAt, Dispatchable: !terminal, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now,
				Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 4096, ProjectedAt: now,
				ReporterRelations: &viewstore.BoardReporterRelations{ReporterStatus: "reported", Dependencies: slices.Clone(ids), Blockers: slices.Clone(card.blockers), DependencyReady: issue.DependencyReady},
				Display:           viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: card.number, Actor: issue.Actor}}
			if terminal {
				row.WindowMask = 7
			}
			if err := w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: project, WorkItemID: card.id, ExpectedRevision: stored.Revision, ExpectedEventSequence: stored.EventSequence, ExpectedProjectRevision: record.ContentSettingsRevision, Current: &row}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func readKeyBoard(t *testing.T, service *Service, credential hubapi.APICredential, params url.Values) (workBoardView, error) {
	t.Helper()
	request, err := parseWorkBoardRequest(params)
	if err != nil {
		return workBoardView{}, err
	}
	return service.readWorkBoardFor(t.Context(), credential, service.appOrganizationID(), request)
}

func TestWorkBoardFollowsKeyReach(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	org := tracker.OrganizationID(service.appOrganizationID())
	source := newNativeFixture(t, service, org, "Example source")
	target := newNativeFixture(t, service, org, "Example target")
	if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), source.project.ID)
		if err != nil {
			return err
		}
		record.States = append([]tracker.NativeState{{Name: "Backlog", Transitions: []string{"Todo"}}}, record.States...)
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hidden := tracker.NativeDependency{ID: "wi_target", ProjectID: target.project.ID, Number: 1, Title: "Example target item", State: "Todo"}
	seedBoardCards(t, service, org, target.project.ID, boardSeedCard{id: hidden.ID, number: 1, state: "Todo"})
	seedBoardCards(t, service, org, source.project.ID, boardSeedCard{id: "wi_blocked", number: 1, state: "Todo", blockers: []tracker.NativeDependency{hidden}},
		boardSeedCard{id: "wi_second", number: 2, state: "Todo"}, boardSeedCard{id: "wi_third", number: 3, state: "Todo"}, boardSeedCard{id: "wi_working", number: 4, state: "In Progress"}, boardSeedCard{id: "wi_done", number: 5, state: "Done"}, boardSeedCard{id: "wi_later", number: 6, state: "Backlog"})
	partial, status, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(source.token), "", "")
	if err != nil {
		t.Fatalf("partial credential status=%d: %v", status, err)
	}
	full, status, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(target.token), "", "")
	if err != nil {
		t.Fatalf("full credential status=%d: %v", status, err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: full.ID, ProjectID: source.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	both := []string{string(source.project.ID), string(target.project.ID)}
	cardIDs := func(view workBoardView) []tracker.NativeWorkItemID {
		var ids []tracker.NativeWorkItemID
		for _, card := range view.Cards {
			ids = append(ids, card.Issue.WorkItemID)
		}
		slices.Sort(ids)
		return ids
	}
	projectIDs := func(view workBoardView) []string {
		var ids []string
		for _, project := range view.Projects {
			ids = append(ids, string(project.Project.ID))
		}
		slices.Sort(ids)
		return ids
	}
	t.Run("every readable project when none is named", func(t *testing.T) {
		view, err := readKeyBoard(t, service, full, url.Values{})
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Clone(both)
		slices.Sort(want)
		if got := projectIDs(view); !slices.Equal(got, want) {
			t.Fatalf("projects=%v want %v", got, want)
		}
	})
	t.Run("restricted key asking for two projects gets its own", func(t *testing.T) {
		view, err := readKeyBoard(t, service, partial, url.Values{"project": both, "format": {"compact"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := projectIDs(view); !slices.Equal(got, []string{string(source.project.ID)}) {
			t.Fatalf("projects=%v", got)
		}
		if !slices.Equal(view.compact.Unavailable, []tracker.ProjectID{target.project.ID}) {
			t.Fatalf("unavailable=%v", view.compact.Unavailable)
		}
		if _, err := readKeyBoard(t, service, partial, url.Values{"project": {string(target.project.ID)}}); !hubapi.IsNativeNotFound(err) {
			t.Fatalf("unreadable project only=%v, want not found", err)
		}
	})
	for _, test := range []struct {
		name       string
		credential hubapi.APICredential
		blockers   int
	}{
		{"link to a project the key reads is visible", full, 1},
		{"link to a project the key cannot read is hidden", partial, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			view, err := readKeyBoard(t, service, test.credential, url.Values{"project": {string(source.project.ID)}})
			if err != nil {
				t.Fatal(err)
			}
			for _, card := range view.Cards {
				if card.Issue.WorkItemID == "wi_blocked" && len(card.Issue.Blockers) != test.blockers {
					t.Fatalf("blockers=%+v want %d", card.Issue.Blockers, test.blockers)
				}
			}
		})
	}
	t.Run("worker credentials are refused", func(t *testing.T) {
		worker := partial
		worker.Scope = hubapi.APIScopeWorker
		if _, err := readKeyBoard(t, service, worker, url.Values{}); !hubapi.IsNativeNotFound(err) {
			t.Fatalf("worker=%v, want not found", err)
		}
	})
	for _, test := range []struct {
		name   string
		params url.Values
		want   []tracker.NativeWorkItemID
	}{
		{"open by default", url.Values{}, []tracker.NativeWorkItemID{"wi_blocked", "wi_later", "wi_second", "wi_third", "wi_working"}},
		{"active", url.Values{"lanes": {"active"}}, []tracker.NativeWorkItemID{"wi_blocked", "wi_second", "wi_third", "wi_working"}},
		{"all adds terminal", url.Values{"lanes": {"all"}}, []tracker.NativeWorkItemID{"wi_blocked", "wi_done", "wi_later", "wi_second", "wi_third", "wi_working"}},
		{"named lane", url.Values{"lane": {"In Progress"}}, []tracker.NativeWorkItemID{"wi_working"}},
		{"named terminal lane", url.Values{"lane": {"Done"}}, []tracker.NativeWorkItemID{"wi_done"}},
		{"excluded lanes", url.Values{"exclude_lane": {"Todo", "Backlog"}}, []tracker.NativeWorkItemID{"wi_working"}},
	} {
		t.Run("lanes "+test.name, func(t *testing.T) {
			params := url.Values{"project": {string(source.project.ID)}}
			for key, values := range test.params {
				params[key] = values
			}
			view, err := readKeyBoard(t, service, partial, params)
			if err != nil {
				t.Fatal(err)
			}
			if got := cardIDs(view); !slices.Equal(got, test.want) {
				t.Fatalf("cards=%v want %v", got, test.want)
			}
			counts := map[string]int{}
			for _, lane := range view.Projects[0].Work.Lanes {
				counts[lane.State] = lane.Total
			}
			if counts["Todo"] != 3 || counts["In Progress"] != 1 || counts["Done"] != 1 || counts["Backlog"] != 1 {
				t.Fatalf("lane counts=%v, want every lane counted whatever is selected", counts)
			}
		})
	}
	t.Run("unknown lane", func(t *testing.T) {
		_, err := readKeyBoard(t, service, partial, url.Values{"exclude_lane": {"Nonexistent"}})
		var native *hubapi.NativeError
		if !errors.As(err, &native) || native.Status != 422 {
			t.Fatalf("unknown lane=%v, want 422", err)
		}
	})
	t.Run("compact continuation returns each card once", func(t *testing.T) {
		params := url.Values{"project": {string(source.project.ID)}, "format": {"compact"}}
		request, err := parseWorkBoardRequest(params)
		if err != nil {
			t.Fatal(err)
		}
		request.budget = 1
		view, err := service.readWorkBoardFor(t.Context(), partial, string(org), request)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string][]tracker.NativeWorkItemID{}
		next := map[string]string{}
		for _, card := range view.compact.Cards {
			seen[card.State] = append(seen[card.State], card.ID)
		}
		for _, continuation := range view.compact.Continuations {
			next[continuation.State] = continuation.NextCursor
		}
		if len(seen["Todo"]) != 1 || len(seen["In Progress"]) != 1 || len(seen["Backlog"]) != 1 || next["Todo"] == "" || next["In Progress"] != "" || !view.compact.Truncated {
			t.Fatalf("first page=%v continuations=%+v", seen, view.compact.Continuations)
		}
		for cursor := next["Todo"]; cursor != ""; {
			request, err := parseWorkBoardRequest(url.Values{"project": {string(source.project.ID)}, "format": {"compact"}, "lane": {"Todo"}, "cursor": {cursor}})
			if err != nil {
				t.Fatal(err)
			}
			request.budget = 1
			page, err := service.readWorkBoardFor(t.Context(), partial, string(org), request)
			if err != nil {
				t.Fatal(err)
			}
			cursor = ""
			for _, card := range page.compact.Cards {
				if card.State != "Todo" {
					t.Fatalf("continuation of Todo returned %s", card.State)
				}
				seen["Todo"] = append(seen["Todo"], card.ID)
			}
			for _, continuation := range page.compact.Continuations {
				cursor = continuation.NextCursor
			}
		}
		slices.Sort(seen["Todo"])
		if !slices.Equal(seen["Todo"], []tracker.NativeWorkItemID{"wi_blocked", "wi_second", "wi_third"}) {
			t.Fatalf("Todo across pages=%v", seen["Todo"])
		}
	})
}
