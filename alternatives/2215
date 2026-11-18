package boardprojection_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestBoardSummaryDiscardsEmptyHistoricalLanesAcrossTenants(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{RunDetails: hubtest.NewDetails()})
	repository := h.Repository
	now := h.Now().UTC().Truncate(time.Microsecond)
	projects := map[tracker.OrganizationID]tracker.ProjectID{}
	latest := map[tracker.OrganizationID]string{}
	for tenant, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := hubfixture.NewNative(t, h, org, "Example changing lanes")
		projects[org] = fixture.Project.ID
		var projectRevision int64
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: fixture.Project.ID, NativeID: "wi_counter", Number: 1, Title: "Example", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner")}); err != nil {
				return err
			}
			project, err := w.Project(t.Context(), fixture.Project.ID)
			projectRevision = project.ContentSettingsRevision
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index := range 220 {
			state := fmt.Sprintf("lane_%d_%03d", tenant, index)
			latest[org] = state
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: fixture.Project.ID, WorkItemID: "wi_counter", Number: 1, Revision: 1, Profile: "native"}, Title: "Example", State: state, Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now}
			payload, err := viewstore.NewContent(h.Config.RunDetails).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			row := viewstore.BoardRecord{OrganizationID: org, ProjectID: fixture.Project.ID, WorkItemID: issue.WorkItemID, SourceRevision: 1, SourceProjectRevision: projectRevision,
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
