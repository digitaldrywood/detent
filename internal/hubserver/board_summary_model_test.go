package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardSummaryProjectionReplaysAndExpiresWindowsPerTenant(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details, now: func() time.Time { return now }})
	repository := service.config.TenantRepository
	projects := map[tracker.OrganizationID]tracker.ProjectID{}
	candidates := map[tracker.OrganizationID][]viewstore.BoardCandidate{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, org, "Example summary")
		projects[org] = fixture.project.ID
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), fixture.project.ID)
			if err != nil {
				return err
			}
			project.States = append(project.States, tracker.NativeState{Name: "Cancelled", Terminal: true}, tracker.NativeState{Name: "Backlog"})
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index, state := range []string{"Todo", "Done", "Cancelled"} {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_summary_%d", index))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: id, Number: index + 1, Revision: 1, Profile: "native"}, Title: "Example " + state, Body: "Example body", State: state, Terminal: state != "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now}
			issue.Type = "change_request"
			if state == "Todo" {
				issue.Type, issue.Labels, issue.Assignees, issue.Priority = "bug", []string{"selected"}, []string{"selected"}, new(1)
			}
			if state == "Done" {
				issue.ClosedAt = new(now.Add(-30 * time.Minute))
			}
			if state == "Cancelled" {
				issue.ClosedAt = new(now.Add(-3 * time.Hour))
			}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: fixture.project.ID, NativeID: id, Number: int64(index + 1), Title: issue.Title, WorkflowStateName: &issue.State, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID}); err != nil {
					return err
				}
				_, err := w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			candidate := viewstore.BoardCandidate{ProjectID: fixture.project.ID, WorkItemID: id}
			candidates[org] = append(candidates[org], candidate)
			if err := service.refreshBoardRecord(t.Context(), org, candidate, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func(org tracker.OrganizationID) map[string]viewstore.BoardSummaryRecord {
		t.Helper()
		details.beforeRead = func(context.Context) error { return errors.New("counts request accessed an object") }
		defer func() { details.beforeRead = nil }()
		result := map[string]viewstore.BoardSummaryRecord{}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			model, ok := r.(interface {
				BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
			})
			if !ok {
				return errors.New("missing typed summary reader")
			}
			rows, err := model.BoardSummaryModel(t.Context(), viewstore.BoardSelection{ProjectID: projects[org], Archived: new(false), ExcludeWorkspace: true})
			for _, row := range rows {
				key := row.State
				if row.Kind == "total" {
					key = "*"
				} else if row.Kind != "lane" {
					key = "@" + row.Kind
				}
				result[key] = row
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := map[tracker.OrganizationID]map[string]viewstore.BoardSummaryRecord{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		rows := read(org)
		initial[org] = rows
		for state, total := range map[string]int64{"*": 3, "@open": 1, "@closed": 2, "Todo": 1, "Done": 1, "Cancelled": 1} {
			if rows[state].Total != total || rows[state].Running != 0 {
				t.Errorf("%s/%s count=%+v, want total=%d running=0", org, state, rows[state], total)
			}
		}
		all, done, cancelled := rows["*"], rows["Done"], rows["Cancelled"]
		scope := nativeScope{organization: org, project: projects[org], credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		legacy, err := service.readWorkBoardProject(t.Context(), scope, savedWorkState{View: "board", Lanes: []string{"Todo"}, CompletedWindow: "24h"})
		if err != nil {
			t.Fatal(err)
		}
		if legacy.Work == nil || legacy.Work.Completed != 1 || legacy.Work.Cancelled != 1 || len(legacy.Work.Lanes) != 3 || len(legacy.Work.ShippedWindows) != 3 || legacy.Work.ShippedWindows[0].Shipped != 1 || legacy.Work.ShippedWindows[0].Cancelled != 0 {
			t.Errorf("%s legacy windows=%+v", org, legacy.Work)
		}

		for _, test := range []struct {
			name  string
			state savedWorkState
			want  tracker.NativeWorkItemID
		}{
			{"label", savedWorkState{Label: []string{"selected"}}, "wi_summary_0"},
			{"type", savedWorkState{Type: []string{"bug"}}, "wi_summary_0"},
			{"assignee", savedWorkState{Assignee: []string{"selected"}}, "wi_summary_0"},
			{"priority", savedWorkState{Priority: []string{"1"}}, "wi_summary_0"},
			{"created_by", savedWorkState{CreatedBy: "owner"}, "wi_summary_0"},
			{"completed_from", savedWorkState{CompletedFrom: now.Add(-30 * time.Minute).Format(time.RFC3339Nano)}, "wi_summary_1"},
			{"completed_to", savedWorkState{CompletedTo: now.Add(-30 * time.Minute).Format(time.RFC3339Nano)}, "wi_summary_2"},
			{"completed_range", savedWorkState{CompletedFrom: now.Add(-30 * time.Minute).Format(time.RFC3339Nano), CompletedTo: now.Format(time.RFC3339Nano)}, "wi_summary_1"},
			{"label_no_match", savedWorkState{Label: []string{"missing"}}, ""},
			{"type_no_match", savedWorkState{Type: []string{"change_request"}}, ""},
			{"assignee_no_match", savedWorkState{Assignee: []string{"missing"}}, ""},
			{"priority_no_match", savedWorkState{Priority: []string{"3"}}, ""},
			{"created_by_no_match", savedWorkState{CreatedBy: "other"}, ""},
			{"completed_range_no_match", savedWorkState{CompletedFrom: now.Add(-2 * time.Hour).Format(time.RFC3339Nano), CompletedTo: now.Add(-time.Hour).Format(time.RFC3339Nano)}, ""},
		} {
			t.Run(string(org)+"/legacy/"+test.name, func(t *testing.T) {
				state := test.state
				state.View, state.Lanes, state.CompletedWindow = "board", []string{"Backlog", "Todo"}, "24h"
				params := url.Values{"include": {"board"}, "state": {"Todo"}, "type": state.Type, "label": state.Label, "assignee": state.Assignee, "priority": state.Priority}
				if state.CreatedBy != "" {
					params.Set("created_by", state.CreatedBy)
				}
				if state.CompletedFrom != "" || state.CompletedTo != "" {
					state.View, state.Tab, state.Sort = "list", "closed", "closed"
					params.Del("state")
					params.Set("open", "false")
					params.Set("completed_from", state.CompletedFrom)
					params.Set("completed_to", state.CompletedTo)
				}
				result, err := service.readWorkBoardProject(t.Context(), scope, state)
				if err != nil {
					t.Fatal(err)
				}
				wantCount := 0
				if test.want != "" {
					wantCount = 1
				}
				if len(result.Cards) != wantCount || len(result.Issues) != wantCount {
					t.Fatalf("cards=%d issues=%d want=%d", len(result.Cards), len(result.Issues), wantCount)
				}
				for _, card := range result.Cards {
					if card.Issue.WorkItemID != test.want || card.Issue.OrganizationID != org || card.Issue.ProjectID != scope.project {
						t.Errorf("unexpected filtered card: %+v", card.Issue.NativeReference)
					}
				}
				if result.Work != nil || state.View == "board" && result.BacklogTotal != -1 {
					t.Errorf("unsupported filtered counts: work=%+v backlog=%d", result.Work, result.BacklogTotal)
				}
				page, err := service.readBoardRecordsFor(t.Context(), scope, params, true)
				if err != nil {
					t.Fatal(err)
				}
				if page.Total != -1 || page.Work != nil || len(page.Cards) != wantCount {
					t.Errorf("filtered page total=%d work=%+v cards=%d want=%d", page.Total, page.Work, len(page.Cards), wantCount)
				}
			})
		}

		if all.Shipped1h != 1 || all.Shipped24h != 1 || all.Shipped7d != 1 || all.Cancelled1h != 0 || all.Cancelled24h != 1 || all.Cancelled7d != 1 {
			t.Errorf("%s summary windows=%+v", org, all)
		}
		if done.Shipped1h != 1 || done.Shipped24h != 1 || cancelled.Cancelled1h != 0 || cancelled.Cancelled24h != 1 {
			t.Errorf("%s lane windows done=%+v cancelled=%+v", org, done, cancelled)
		}
		if !all.ProjectedAt.Equal(now) || all.Sequence == 0 {
			t.Errorf("%s missing checkpoint: %+v", org, all)
		}
		for _, candidate := range candidates[org] {
			if err := service.refreshBoardRecord(t.Context(), org, candidate, now); err != nil {
				t.Fatal(err)
			}
		}
		if replay := read(org); !reflect.DeepEqual(replay, rows) {
			t.Errorf("%s replay changed counts or sequence: before=%+v after=%+v", org, rows, replay)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			model := r.(interface {
				BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
			})
			_, err := model.BoardSummaryModel(t.Context(), viewstore.BoardSelection{ProjectID: projects[org], Types: []string{"bug"}})
			if !errors.Is(err, corestore.ErrInvalidPage) {
				t.Errorf("unsupported exact count filter error=%v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(31 * time.Minute)
	if err := service.refreshBoardRecord(t.Context(), "org_first", candidates["org_first"][1], now); err != nil {
		t.Fatal(err)
	}
	updated := read("org_first")
	if updated["*"].Total != 3 || updated["Done"].Total != 1 || updated["*"].Shipped1h != 0 || updated["Done"].Shipped1h != 0 || updated["*"].Shipped24h != 1 || updated["*"].Cancelled24h != 1 {
		t.Errorf("expired window changed the wrong counts: %+v", updated)
	}
	if updated["*"].Sequence <= initial["org_first"]["*"].Sequence || !updated["*"].ProjectedAt.Equal(now) {
		t.Errorf("expiry did not advance checkpoint: %+v", updated["*"])
	}
	if other := read("org_first_second"); !reflect.DeepEqual(other, initial["org_first_second"]) {
		t.Errorf("one tenant's refresh changed another tenant: before=%+v after=%+v", initial["org_first_second"], other)
	}
}

func TestBoardCountsFenceOwnershipUntilProjection(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }})
	repository := service.config.TenantRepository
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			fixture := newNativeFixture(t, service, org, "Example counts")
			credential, _, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(fixture.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			credential.HostedRole, credential.HostedPrincipal = "reporter", "owner"
			scope := nativeScope{organization: org, project: fixture.project.ID, credential: credential}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: "wi_counts", Number: 1, Revision: 1, Profile: "native"}, Title: "Example", State: "Done", Terminal: true, Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now, ClosedAt: new(now)}
			publish := func(create bool) {
				t.Helper()
				payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
				if err != nil {
					t.Fatal(err)
				}
				err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					var item corestore.WorkItem
					var err error
					if create {
						item, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: fixture.project.ID, NativeID: issue.WorkItemID, Number: 1, Title: issue.Title, WorkflowStateName: new(issue.State), ActorKind: new("human"), ActorPrincipalID: new(issue.Actor.PrincipalID)})
					} else {
						item, err = w.WorkItem(t.Context(), fixture.project.ID, issue.WorkItemID)
					}
					if err != nil {
						return err
					}
					project, err := w.Project(t.Context(), fixture.project.ID)
					if err != nil {
						return err
					}
					issue.Revision = tracker.Revision(item.Revision)
					row := viewstore.BoardRecord{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: issue.WorkItemID, SourceRevision: item.Revision, SourceEventSequence: item.EventSequence, SourceProjectRevision: project.ContentSettingsRevision, Position: viewstore.BoardPosition{State: issue.State, Identifier: "example#1"}, ActorPrincipalID: issue.Actor.PrincipalID, Terminal: true, Running: true, WindowMask: 7, ReporterRelations: &viewstore.BoardReporterRelations{}, Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 1000, ProjectedAt: now, CreatedAt: now, UpdatedAt: now}
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: fixture.project.ID, WorkItemID: issue.WorkItemID, ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence, ExpectedProjectRevision: project.ContentSettingsRevision, Current: &row})
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			read := func(owner string, pending bool, total int64) {
				t.Helper()
				err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					model := r.(interface {
						BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
					})
					rows, err := model.BoardSummaryModel(t.Context(), viewstore.BoardSelection{ProjectID: fixture.project.ID, Reporter: true, ReporterPrincipalID: owner, Archived: new(false), ExcludeWorkspace: true})
					if pending {
						if !errors.Is(err, searchstore.ErrIndexPending) || len(rows) != 0 {
							t.Errorf("%s stale counts=%+v error=%v", owner, rows, err)
						}
						return nil
					}
					if err != nil {
						return err
					}
					var got int64
					for _, row := range rows {
						if row.Kind == "total" {
							got = row.Total
							if row.Running != total || row.Shipped1h != total {
								t.Errorf("summary facts=%+v", row)
							}
						}
					}
					if got != total {
						t.Errorf("%s total=%d want %d", owner, got, total)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			httpRead := func(want int) {
				t.Helper()
				recorder := httptest.NewRecorder()
				c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/board/counts", nil), recorder)
				c.Set("native_scope", scope)
				if err := service.boardModelCounts(c); err != nil {
					t.Fatal(err)
				}
				if recorder.Code != want {
					t.Errorf("HTTP counts status=%d want=%d body=%s", recorder.Code, want, recorder.Body.String())
				}
			}
			publish(true)
			sub := &dashboardSubscription{initial: true, sequence: 1, params: map[string][]string{}, frames: make(chan dashboardFrame, 1), done: make(chan struct{})}
			delivery := &dashboardDelivery{config: dashboardDeliveryConfig{pageSize: 10}}
			if _, err := service.dashboardBoardRecordPage(t.Context(), delivery, sub, scope); err != nil {
				t.Fatal(err)
			}
			select {
			case frame := <-sub.frames:
				var body map[string]json.RawMessage
				if err := json.Unmarshal([]byte(frame.data), &body); err != nil {
					t.Fatal(err)
				}
				if string(body["counts_independent"]) != "true" {
					t.Errorf("live frame does not preserve independently loaded counts: %s", frame.data)
				}
			default:
				t.Fatal("missing initial board frame")
			}
			read("owner", false, 1)
			httpRead(http.StatusOK)
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				item, err := w.WorkItem(t.Context(), fixture.project.ID, issue.WorkItemID)
				if err != nil {
					return err
				}
				item.ActorPrincipalID = new("other")
				_, err = w.UpdateWorkItem(t.Context(), item)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			read("owner", true, 0)
			read("other", true, 0)
			httpRead(http.StatusServiceUnavailable)
			issue.Actor.PrincipalID = "other"
			publish(false)
			read("owner", false, 0)
			read("other", false, 1)
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				return w.DeleteAccessTokenGrant(t.Context(), credential.ID, fixture.project.ID)
			}); err != nil {
				t.Fatal(err)
			}
			httpRead(http.StatusForbidden)
		})
	}
}
