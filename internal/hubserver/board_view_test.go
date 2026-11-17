package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
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
	t.Run("lane change shows before projection", func(t *testing.T) {
		credential := hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}
		find := func(org tracker.OrganizationID, item tracker.NativeWorkItemID, lanes string) (tracker.NativeBoardCard, bool) {
			params := boardViewParams(t, savedWorkState{View: "board", CompletedWindow: "24h"}, []tracker.ProjectID{projects[org]})
			params.Set("lanes", lanes)
			request, err := parseWorkBoardRequest(params)
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.readWorkBoard(t.Context(), hubscope.NativeScope{Organization: org, Credential: credential}, request)
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(view.Cards, func(card tracker.NativeBoardCard) bool { return card.Issue.WorkItemID == item })
			if index < 0 {
				return tracker.NativeBoardCard{}, false
			}
			return view.Cards[index], true
		}
		for _, move := range []struct {
			item          tracker.NativeWorkItemID
			from, to      string
			terminal      bool
			openBefore    bool
			openAfterward bool
		}{
			{item: "wi_view_0002", from: "In Progress", to: "Done", terminal: true, openBefore: true},
			{item: "wi_view_0003", from: "Done", to: "Todo", openAfterward: true},
		} {
			if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				item, err := w.WorkItem(t.Context(), projects["org_first"], move.item)
				if err != nil {
					return err
				}
				item.WorkflowStateName = new(move.to)
				_, err = w.UpdateWorkItem(t.Context(), item)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			request, err := parseWorkBoardRequest(boardViewParams(t, savedWorkState{View: "board", CompletedWindow: "24h"}, []tracker.ProjectID{projects["org_first"]}, move.item))
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.readWorkBoard(t.Context(), hubscope.NativeScope{Organization: "org_first", Credential: credential}, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Cards) != 1 || view.Cards[0].Issue.State != move.to || view.Cards[0].Issue.Terminal != move.terminal {
				t.Fatalf("%s item read=%+v want %s terminal=%t", move.item, view.Cards, move.to, move.terminal)
			}
			if _, open := find("org_first", move.item, "open"); open != move.openAfterward {
				t.Fatalf("%s on the open board=%t after moving to %s", move.item, open, move.to)
			}
			if card, open := find("org_first", move.item, "all"); open && card.Issue.State != move.to {
				t.Fatalf("%s shown in %s after moving to %s", move.item, card.Issue.State, move.to)
			}
			other, open := find("org_first_second", move.item, "all")
			if !open || other.Issue.State != move.from {
				t.Fatalf("other tenant's %s=%+v open=%t want %s", move.item, other.Issue.NativeReference, open, move.from)
			}
			if _, open := find("org_first_second", move.item, "open"); open != move.openBefore {
				t.Fatalf("other tenant's %s on the open board=%t", move.item, open)
			}
		}
	})
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

func TestBoardViewCardsMatchLaneCards(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	repository := service.config.TenantRepository
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	expires := now.Add(10 * time.Minute)
	identity := &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "example-model", ReasoningEffort: "high"}
	cards := []tracker.NativeBoardCard{
		{Issue: tracker.NativeIssue{Title: "Human card", State: "Todo", Priority: new(1), Labels: []string{"bug"}, Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin", ViaKey: "key_example", DisplayName: "Stored name"},
			Provenance:    &tracker.Provenance{Provider: "github", ExternalID: "101", AuthorID: "octocat", AuthorDisplayName: "Octo Cat", CreatedAt: now, UpdatedAt: now, ObservedAt: now},
			IssueContract: &issuecontract.State{HumanAction: "Confirm the release window"}, ReportContext: &tracker.ReportContext{URL: "https://example.test/work", Browser: "Firefox", CapturedAt: now.Format(time.RFC3339)},
			SchedulingEligible: new(true), BlockedReason: tracker.DependencyReason{Basis: "basis-example", Revision: 1}}},
		{Issue: tracker.NativeIssue{Title: "Runner card", State: "In Progress", Actor: tracker.Actor{Kind: "runner", PrincipalID: "runner_example", Agent: "Luna (example-model)"}},
			Attempt:   &tracker.NativeBoardAttempt{Count: 2, ID: "att_example", Status: "running", RunnerID: "runner_example", MachineID: "machine_example", Identity: identity, StartedAt: now, ExpiresAt: expires},
			Execution: &tracker.NativeBoardExecution{ID: "att_example", Running: true, Stage: "implement", RunnerID: "runner_example", Identity: identity, StartedAt: &now, ExpiresAt: expires}},
		{Issue: tracker.NativeIssue{Title: "Integration card", State: "Todo", Actor: tracker.Actor{Kind: "integration", PrincipalID: "github_example", DisplayName: "octocat"},
			Provenance: &tracker.Provenance{Provider: "github", ExternalID: "102", AuthorID: "octocat", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}},
		{Issue: tracker.NativeIssue{Title: "Machine card", State: "Todo", Actor: tracker.Actor{Kind: "machine", PrincipalID: "runner_example", DisplayName: "runner_example", Agent: "progress-check"}}},
		{Issue: tracker.NativeIssue{Title: "Verified card", State: "Done", Terminal: true, ClosedAt: &now, Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"},
			Verification: &tracker.NativeVerification{State: "verified", VerifiedBy: &tracker.Actor{Kind: "human", PrincipalID: "example", DisplayName: "Example"}, VerifiedAt: &now}}},
	}
	directory := os.Getenv("DETENT_TEST_BOARD_CONTRACT_DIR")
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, org, "Example")
		var record projectstore.Record
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			record, err = reader.ProjectRecord(t.Context(), fixture.project.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var ids []tracker.NativeWorkItemID
		for index, card := range cards {
			number := index + 1
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_contract_%04d", number))
			ids = append(ids, id)
			card.Issue.NativeReference = tracker.NativeReference{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: id, Number: number, Profile: "native"}
			card.Issue.Body, card.Issue.CreatedAt, card.Issue.UpdatedAt, card.Issue.LastActivityAt = "Example body", now, now, now
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: fixture.project.ID, NativeID: id, Number: int64(number), Title: card.Issue.Title, WorkflowStateName: new(card.Issue.State), ActorKind: new(card.Issue.Actor.Kind), ActorPrincipalID: new(card.Issue.Actor.PrincipalID)})
				if err != nil {
					return err
				}
				card.Issue.Revision = tracker.Revision(item.Revision)
				card.StatusLine = nativeBoardStatusLine(card)
				snapshot := boardProjectionSnapshot{organizationSlug: strings.ReplaceAll(string(org), "_", "-"), project: record, item: item}
				if card.Execution != nil {
					snapshot.lease = &leasestore.Lease{LeaseID: card.Execution.ID, FencingToken: 1, ExpiresAt: card.Execution.ExpiresAt}
				}
				row := boardProjectionRecord(snapshot, card)
				row.ProjectedAt = now
				if row.Terminal {
					row.WindowMask = 7
				}
				issue := card.Issue
				issue.WebURL = hubrecords.IssueBrowserPath(strings.ReplaceAll(string(org), "_", "-"), record.Core.Slug, number)
				board, list, reporter, listBytes, err := boundBoardProjection(service.nativeIssueResponse(issue), viewstore.BoardReporterRelations{ReporterStatus: "submitted", DependencyReady: true, BlockedReason: issue.BlockedReason})
				if err != nil {
					return err
				}
				payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), card)
				if err != nil {
					return err
				}
				row.ListIssue, row.ListBytes, row.PageIssue, row.ReporterRelations, row.Payload = &list, listBytes, &board, &reporter, payload
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: fixture.project.ID, WorkItemID: id, ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence, ExpectedProjectRevision: record.Core.ContentSettingsRevision, Current: &row})
			}); err != nil {
				t.Fatal(err)
			}
		}
		admin := hubscope.NativeScope{Organization: org, Credential: hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}}
		reporter := admin
		reporter.Credential.HostedRole, reporter.Credential.HostedPrincipal = "reporter", "test_hub_admin"
		for _, role := range []struct {
			name  string
			scope hubscope.NativeScope
			want  []tracker.NativeWorkItemID
		}{{"member", admin, ids}, {"reporter", reporter, []tracker.NativeWorkItemID{ids[0], ids[4]}}} {
			t.Run(string(org)+"/"+role.name, func(t *testing.T) {
				state := savedWorkState{View: "board", CompletedWindow: "24h"}
				read := func(set map[string]string) workBoardView {
					params := boardViewParams(t, state, []tracker.ProjectID{fixture.project.ID})
					for key, value := range set {
						params.Set(key, value)
					}
					request, err := parseWorkBoardRequest(params)
					if err != nil {
						t.Fatal(err)
					}
					scope := role.scope
					scope.Project = ""
					view, err := service.readWorkBoard(t.Context(), scope, request)
					if err != nil {
						t.Fatal(err)
					}
					return view
				}
				view := read(map[string]string{"lanes": "all"})
				board := boardContractCards(t, view)
				open := read(nil)
				for id, card := range boardContractCards(t, open) {
					if !reflect.DeepEqual(card, board[id]) {
						t.Errorf("%s: default board card differs from the lanes=all card", id)
					}
				}
				if got, want := len(open.Cards), len(role.want)-1; got != want {
					t.Errorf("default board cards=%d want %d open cards", got, want)
				}
				compact := read(map[string]string{"lanes": "all", "format": "compact"}).compact
				if compact == nil || len(compact.Cards) != len(role.want) {
					t.Fatalf("compact board=%+v", compact)
				}
				for _, card := range compact.Cards {
					actor := board[card.ID].(map[string]any)["issue"].(map[string]any)["actor"].(map[string]any)
					want := boardview.CompactActor{Kind: actor["kind"].(string), PrincipalID: actor["principal_id"].(string)}
					want.DisplayName, _ = actor["display_name"].(string)
					want.Agent, _ = actor["agent"].(string)
					if card.CreatedBy != want || want.Kind == "" {
						t.Errorf("%s: compact created_by=%+v want %+v", card.ID, card.CreatedBy, want)
					}
				}
				if got := slices.Sorted(func(yield func(tracker.NativeWorkItemID) bool) {
					for id := range board {
						if !yield(id) {
							return
						}
					}
				}); !slices.Equal(got, role.want) {
					t.Fatalf("board cards=%v want %v", got, role.want)
				}
				lanes := map[tracker.NativeWorkItemID]any{}
				for _, lane := range fixture.project.States {
					params := url.Values{"state": {lane.Name}, "include": {"board"}, "open": {"true"}, "limit": {"200"}}
					if lane.Terminal {
						params = url.Values{"state": {lane.Name}, "include": {"board"}, "sort": {"closed"}, "limit": {"200"}}
					}
					scope := role.scope
					scope.Project = fixture.project.ID
					request := httptest.NewRequest(http.MethodGet, "/work-items?"+params.Encode(), nil).WithContext(t.Context())
					recorder := httptest.NewRecorder()
					c := echo.New().NewContext(request, recorder)
					c.Set("native_scope", scope)
					if err := service.listNativeIssues(c); err != nil {
						t.Fatal(err)
					}
					var page struct {
						Cards []json.RawMessage `json:"cards"`
					}
					if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &page) != nil {
						t.Fatalf("lane %s status=%d body=%s", lane.Name, recorder.Code, recorder.Body.String())
					}
					for _, raw := range page.Cards {
						id, value := boardContractCard(t, raw)
						lanes[id] = value
					}
				}
				for id, value := range board {
					if !reflect.DeepEqual(value, lanes[id]) {
						got, _ := json.Marshal(value)
						want, _ := json.Marshal(lanes[id])
						t.Errorf("%s: board card differs from the lane card\nboard: %s\nlane:  %s", id, got, want)
					}
				}
				request, err := parseWorkBoardRequest(boardViewParams(t, state, []tracker.ProjectID{fixture.project.ID}, role.want...))
				if err != nil {
					t.Fatal(err)
				}
				scope := role.scope
				scope.Project = ""
				delta, err := service.readWorkBoard(t.Context(), scope, request)
				if err != nil {
					t.Fatal(err)
				}
				if got := boardContractCards(t, delta); !reflect.DeepEqual(got, board) {
					t.Errorf("item delta cards differ from the board cards: %d vs %d", len(got), len(board))
				}
				if directory != "" {
					for name, value := range map[string]workBoardView{"": open, "-all": view, "-delta": delta} {
						raw, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("work-board-%s-%s%s.json", org, role.name, name)), raw, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func boardContractCards(t *testing.T, view workBoardView) map[tracker.NativeWorkItemID]any {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Cards []json.RawMessage `json:"cards"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	cards := map[tracker.NativeWorkItemID]any{}
	for _, card := range wire.Cards {
		id, value := boardContractCard(t, card)
		cards[id] = value
	}
	return cards
}

func boardContractCard(t *testing.T, raw json.RawMessage) (tracker.NativeWorkItemID, any) {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	issue, _ := value["issue"].(map[string]any)
	id, _ := issue["work_item_id"].(string)
	if id == "" {
		t.Fatalf("card without an issue id: %s", raw)
	}
	return tracker.NativeWorkItemID(id), value
}
