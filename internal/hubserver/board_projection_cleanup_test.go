package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardProjectionDropsObsoleteCardsPostgres(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var logs bytes.Buffer
	service := &Service{database: &database{postgres: repository}, config: Config{TenantRepository: repository, RunDetails: &memoryRunDetails{}, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			service.localOrganization, service.boardRefreshCursor = string(org), ""
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
					return err
				}
				for _, project := range []tracker.ProjectID{"source", "target"} {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			create := func(project tracker.ProjectID, id tracker.NativeWorkItemID, number int64, canonical bool) {
				t.Helper()
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: int(number), Revision: 1}, Title: "Private example title", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if canonical {
						_, err := w.CreateCollaborationVersion(t.Context(), version)
						return err
					}
					item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: number, Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State})
					if err != nil {
						return err
					}
					card := tracker.NativeBoardCard{Issue: issue}
					payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), card)
					if err != nil {
						return err
					}
					projection := boardProjectionRecord(boardProjectionSnapshot{item: item, project: projectstore.Record{States: nativeFixtureStates()}}, card)
					projection.Payload = payload
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &projection})
				}); err != nil {
					t.Fatal(err)
				}
			}
			create("source", "wi_deleted", 1, false)
			create("source", "wi_stale_source", 2, false)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteWorkItem(t.Context(), "source", "wi_deleted")
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.DeleteWorkItem(t.Context(), "source", "wi_stale_source"); err != nil {
					return err
				}
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "target", NativeID: "wi_moved", Number: 1, Title: "Private example title", ActorKind: new("human"), ActorPrincipalID: new("example"), WorkflowStateName: new("Todo")})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			result, err := admin.ExecContext(t.Context(), "UPDATE native_board_items SET work_item_id='wi_moved' WHERE organization_id=$1 AND project_id='source' AND work_item_id='wi_stale_source'", org)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				t.Fatalf("stale projection rekey count=%d error=%v", count, err)
			}
			create("target", "wi_moved", 1, true)
			for range 2 {
				if err := service.maintainBoardRecords(t.Context()); err != nil {
					t.Error(err)
				}
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				for _, id := range []tracker.NativeWorkItemID{"wi_deleted", "wi_moved"} {
					if _, err := r.BoardItem(t.Context(), "source", id); !errors.Is(err, corestore.ErrNotFound) {
						t.Errorf("obsolete card %s retained: %v", id, err)
					}
				}
				if _, err := r.NextBoardCandidate(t.Context(), "", "wi_moved"); !errors.Is(err, corestore.ErrNotFound) {
					t.Errorf("deleted item consumes future refresh budget: %v", err)
				}
				card, err := r.BoardItem(t.Context(), "target", "wi_moved")
				if err == nil && (!card.Display.Ready || card.OrganizationID != org) {
					t.Errorf("transferred card not usable: %+v", card)
				}
				return err
			}); err != nil {
				t.Error(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for index, id := range []tracker.NativeWorkItemID{"wi_broken", "wi_broken_second"} {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "target", NativeID: id, Number: int64(index + 2), Title: "Private example title", WorkflowStateName: new("Todo")}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			logs.Reset()
			service.maintainNativeBoard(t.Context())
			if !strings.Contains(logs.String(), `"work_item_id":"wi_broken"`) || !strings.Contains(logs.String(), `"work_item_id":"wi_broken_second"`) || !strings.Contains(logs.String(), `"step":"collaboration_version"`) || strings.Contains(logs.String(), "Private example title") {
				t.Fatalf("missing safe per-card diagnostic: %s", &logs)
			}
		})
	}
}
