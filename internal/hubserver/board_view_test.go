package hubserver

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardViewReadsOpenCardsPerTenant(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	repository := service.config.TenantRepository
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	type seeded struct {
		id    tracker.NativeWorkItemID
		state string
		actor string
	}
	cards := []seeded{{"wi_view_0001", "Todo", "example"}, {"wi_view_0002", "In Progress", "other"}, {"wi_view_0003", "Done", "example"}}
	projects := map[tracker.OrganizationID]tracker.ProjectID{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, org, "Example")
		projects[org] = fixture.project.ID
		for index, card := range cards {
			terminal := card.state == "Done"
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: card.id, Number: index + 1, Revision: 1}, Title: "Example " + string(org), State: card.state, Terminal: terminal, Actor: tracker.Actor{Kind: "human", PrincipalID: card.actor}, CreatedAt: now, UpdatedAt: now, LastActivityAt: now}
			if terminal {
				issue.ClosedAt = &now
			}
			payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: fixture.project.ID, NativeID: card.id, Number: int64(index + 1), Title: issue.Title, WorkflowStateName: new(card.state), ActorKind: new("human"), ActorPrincipalID: new(card.actor)})
				if err != nil {
					return err
				}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: card.id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: card.state, Identifier: fmt.Sprintf("Example#%d", index+1), Activity: &now}, Terminal: terminal, ClosedAt: issue.ClosedAt, Dispatchable: !terminal, CreatedAt: now, UpdatedAt: now, Payload: payload, ActorPrincipalID: card.actor, PageIssue: &issue, ListIssue: &issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{DependencyReady: true}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: index + 1, Actor: issue.Actor}}
				if terminal {
					row.WindowMask = 7
				}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: fixture.project.ID, WorkItemID: card.id, ExpectedRevision: item.Revision, Current: &row})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for org, project := range projects {
		admin := hubscope.NativeScope{Organization: org, Project: project, Credential: hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}}
		state := savedWorkState{View: "board", CompletedWindow: "24h"}
		t.Run(string(org)+"/board", func(t *testing.T) {
			params := boardViewParams(t, state, []tracker.ProjectID{project})
			params.Set("lanes", "all")
			request, err := parseWorkBoardRequest(params)
			if err != nil {
				t.Fatal(err)
			}
			scope := admin
			scope.Project = ""
			view, err := service.readWorkBoard(t.Context(), scope, request)
			if err != nil {
				t.Fatal(err)
			}
			var open, closed []tracker.NativeWorkItemID
			for _, card := range view.Cards {
				if card.Issue.OrganizationID != org || card.Issue.ProjectID != project || card.Issue.Title != "Example "+string(org) {
					t.Fatalf("card from another tenant or project: %+v", card.Issue.NativeReference)
				}
				if card.Issue.Terminal {
					closed = append(closed, card.Issue.WorkItemID)
				} else {
					open = append(open, card.Issue.WorkItemID)
				}
			}
			slices.Sort(open)
			if !slices.Equal(open, []tracker.NativeWorkItemID{"wi_view_0001", "wi_view_0002"}) || !slices.Equal(closed, []tracker.NativeWorkItemID{"wi_view_0003"}) {
				t.Fatalf("open=%v closed=%v", open, closed)
			}
			work := testBoardWork(view)
			if len(view.Projects) != 1 || work == nil || len(view.Projects[0].Project.States) == 0 || view.Projects[0].Sequence == 0 {
				t.Fatalf("project=%+v", view.Projects)
			}
			lanes := map[string]int{}
			for _, lane := range work.Lanes {
				lanes[lane.State] = lane.Total
			}
			if lanes["Todo"] != 1 || lanes["In Progress"] != 1 || lanes["Done"] != 1 || work.Completed != 1 {
				t.Fatalf("counts=%+v", work)
			}
		})
		t.Run(string(org)+"/reporter", func(t *testing.T) {
			reporter := admin
			reporter.Credential.HostedRole, reporter.Credential.HostedPrincipal, reporter.Project = "reporter", "example", ""
			params := boardViewParams(t, state, []tracker.ProjectID{project})
			params.Set("lanes", "all")
			request, err := parseWorkBoardRequest(params)
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.readWorkBoard(t.Context(), reporter, request)
			if err != nil {
				t.Fatal(err)
			}
			for _, card := range view.Cards {
				if card.Issue.Actor.PrincipalID != "example" {
					t.Fatalf("reporter saw another principal's card: %+v", card.Issue.NativeReference)
				}
			}
			if len(view.Cards) != 2 {
				t.Fatalf("reporter cards=%d want 2", len(view.Cards))
			}
		})
		t.Run(string(org)+"/delta", func(t *testing.T) {
			request, err := parseWorkBoardRequest(boardViewParams(t, state, []tracker.ProjectID{project}, "wi_view_0002", "wi_missing"))
			if err != nil {
				t.Fatal(err)
			}
			scope := admin
			scope.Project = ""
			view, err := service.readWorkBoard(t.Context(), scope, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Cards) != 1 || view.Cards[0].Issue.WorkItemID != "wi_view_0002" || view.Cards[0].Issue.OrganizationID != org || len(view.Lanes) != 0 {
				t.Fatalf("delta=%+v", view.Cards)
			}
		})
	}
	org := tracker.OrganizationID("org_first")
	if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(t.Context(), projects[org], "wi_view_0001")
		if err != nil {
			return err
		}
		return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: projects[org], WorkItemID: "wi_view_0001", ExpectedRevision: item.Revision})
	}); err != nil {
		t.Fatal(err)
	}
	admin := hubscope.NativeScope{Organization: org, Project: projects[org], Credential: hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}}
	for _, card := range readTestWorkBoard(t, t.Context(), service, admin, savedWorkState{View: "board", CompletedWindow: "24h"}).Cards {
		if card.Issue.WorkItemID == "wi_view_0001" {
			t.Fatal("removed board item is still on the board")
		}
	}
	second := readTestWorkBoard(t, t.Context(), service, hubscope.NativeScope{Organization: "org_first_second", Project: projects["org_first_second"], Credential: admin.Credential}, savedWorkState{View: "board", CompletedWindow: "24h"})
	if !slices.ContainsFunc(second.Cards, func(card tracker.NativeBoardCard) bool { return card.Issue.WorkItemID == "wi_view_0001" }) {
		t.Fatal("removing one tenant's card removed the other tenant's card with the same id")
	}
}
