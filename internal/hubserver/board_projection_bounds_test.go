package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardProjectionBoundsReporterRelationshipsAcrossTenants(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	repository := service.config.TenantRepository
	now := service.config.now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, org, "Example bounded projection")
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: "wi_bounds", Number: 1, Revision: 1, Profile: "native"}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now, DependencyReady: true,
			Provenance: &tracker.Provenance{Provider: "external", ExternalID: "example", AuthorID: "owner", ObservedAt: now}, BlockedReason: tracker.DependencyReason{Text: strings.Repeat("reason", 4096)}}
		for index := range 20 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_relation_%02d", index))
			issue.Dependencies = append(issue.Dependencies, id)
			issue.Blockers = append(issue.Blockers, tracker.NativeDependency{ID: id, ProjectID: fixture.project.ID, Number: index + 2, Title: strings.Repeat("x", 8*1024), State: "Todo"})
		}
		issue.Dependents, issue.Related = slices.Clone(issue.Blockers), slices.Clone(issue.Blockers)
		reporter := viewstore.BoardReporterRelations{ReporterStatus: "reported", Dependencies: slices.Clone(issue.Dependencies), Blockers: slices.Clone(issue.Blockers), Dependents: slices.Clone(issue.Dependents), Related: slices.Clone(issue.Related), DependencyReady: true, BlockedReason: issue.BlockedReason}
		board, list, boundedReporter, listBytes, err := boundBoardProjection(issue, reporter)
		if err != nil {
			t.Fatal(err)
		}
		if listBytes <= 0 || listBytes > operatortool.WorkListPageBytes {
			t.Fatalf("bounded list bytes=%d, limit=%d", listBytes, operatortool.WorkListPageBytes)
		}
		for _, field := range []string{"blockers", "dependents", "related"} {
			if !slices.Contains(board.OmittedFields, field) {
				t.Errorf("large %s was not marked omitted", field)
			}
		}
		if !reflect.DeepEqual(board.Provenance, issue.Provenance) {
			t.Error("bounding removed board provenance")
		}
		payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: board})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: fixture.project.ID, NativeID: issue.WorkItemID, Number: 1, Title: issue.Title, WorkflowStateName: &issue.State, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID})
			if err != nil {
				return err
			}
			project, err := w.Project(t.Context(), fixture.project.ID)
			if err != nil {
				return err
			}
			row := viewstore.BoardRecord{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: issue.WorkItemID, SourceRevision: item.Revision, SourceEventSequence: item.EventSequence, SourceProjectRevision: project.ContentSettingsRevision,
				Position: viewstore.BoardPosition{State: "Todo", Identifier: "example#1"}, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now, Payload: payload, PageIssue: &board, ListIssue: &list, ReporterRelations: &boundedReporter, ListBytes: listBytes, ProjectedAt: now,
				Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: 1, Actor: issue.Actor}}
			return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence, ExpectedProjectRevision: project.ContentSettingsRevision, Current: &row})
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.BoardItem(t.Context(), fixture.project.ID, issue.WorkItemID)
			if err != nil {
				return err
			}
			if row.PageIssue == nil || row.ListIssue == nil || row.PageIssue.OrganizationID != org || row.ListBytes != listBytes {
				t.Fatalf("typed projection lost tenant or byte budget: %+v", row)
			}
			for _, role := range []string{"", "reporter"} {
				scope := nativeScope{organization: org, project: fixture.project.ID, credential: apiCredential{HostedRole: role, HostedPrincipal: "owner"}}
				card, err := boardModelCard(scope, row)
				if err != nil {
					return err
				}
				if role == "reporter" {
					if len(card.Issue.Blockers)+len(card.Issue.Dependents)+len(card.Issue.Related) != 0 {
						t.Error("reporter restored omitted relationship arrays")
					}
					for _, field := range []string{"blockers", "dependents", "related"} {
						if !slices.Contains(card.Issue.OmittedFields, field) {
							t.Errorf("reporter lost omitted marker %s", field)
						}
					}
				}
				raw, err := json.Marshal(operatortool.NativeItemPage(string(fixture.project.ID), tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{card.Issue}}))
				if err != nil {
					return err
				}
				if len(raw) > row.ListBytes || len(raw) > operatortool.WorkListPageBytes {
					t.Errorf("tenant=%s role=%q encoded bytes=%d stored=%d maximum=%d", org, role, len(raw), row.ListBytes, operatortool.WorkListPageBytes)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBoardSummaryDiscardsEmptyHistoricalLanesAcrossTenants(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	repository := service.config.TenantRepository
	now := service.config.now().UTC().Truncate(time.Microsecond)
	projects := map[tracker.OrganizationID]tracker.ProjectID{}
	latest := map[tracker.OrganizationID]string{}
	for tenant, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, org, "Example changing lanes")
		projects[org] = fixture.project.ID
		var projectRevision int64
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: fixture.project.ID, NativeID: "wi_counter", Number: 1, Title: "Example", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner")}); err != nil {
				return err
			}
			project, err := w.Project(t.Context(), fixture.project.ID)
			projectRevision = project.ContentSettingsRevision
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index := range 220 {
			state := fmt.Sprintf("lane_%d_%03d", tenant, index)
			latest[org] = state
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: "wi_counter", Number: 1, Revision: 1, Profile: "native"}, Title: "Example", State: state, Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now}
			payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			row := viewstore.BoardRecord{OrganizationID: org, ProjectID: fixture.project.ID, WorkItemID: issue.WorkItemID, SourceRevision: 1, SourceProjectRevision: projectRevision,
				Position: viewstore.BoardPosition{State: state, Identifier: "example#1"}, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now, Payload: payload, PageIssue: &issue, ListIssue: &issue, ReporterRelations: &viewstore.BoardReporterRelations{}, ListBytes: 2048, ProjectedAt: now,
				Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: 1, Actor: issue.Actor}}
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ExpectedRevision: 1, ExpectedProjectRevision: projectRevision, Current: &row})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			model := r.(interface {
				BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
			})
			rows, err := model.BoardSummaryModel(t.Context(), viewstore.BoardSelection{ProjectID: projects[org], Archived: new(false), ExcludeWorkspace: true})
			if err != nil {
				return err
			}
			laneCount, emptyCount := 0, 0
			var total, current viewstore.BoardSummaryRecord
			for _, row := range rows {
				if row.Total == 0 {
					emptyCount++
				}
				if row.Kind == "total" {
					total = row
				}
				if row.Kind == "lane" {
					laneCount++
					current = row
				}
			}
			if len(rows) != 3 || laneCount != 1 || emptyCount != 0 {
				t.Errorf("tenant=%s summaries=%d lanes=%d empty=%d, want3/1/0", org, len(rows), laneCount, emptyCount)
			}
			if total.Kind != "total" || total.Total != 1 || current.State != latest[org] || current.Total != 1 {
				t.Errorf("tenant=%s lost current counters: total=%+v lane=%+v", org, total, current)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
