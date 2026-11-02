package hubserver

import (
	"encoding/json"
	"fmt"
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

func TestBoardProjectionMaintenanceIsBoundedAndTenantScoped(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 22, 0, 0, 0, time.UTC)
	service := &Service{config: Config{TenantRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}, localOrganization: "org_first"}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index := range 33 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%02d", index))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: fmt.Sprintf("Example %d", index), State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
					return err
				}
				_, err := w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertPage := func(org tracker.OrganizationID, count int) int64 {
		t.Helper()
		var sequence int64
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "example", Limit: 200})
			if err != nil {
				return err
			}
			if len(rows) != count {
				t.Errorf("tenant %s has %d cards, want %d", org, len(rows), count)
			}
			for _, row := range rows {
				if row.OrganizationID != org {
					t.Errorf("cross-tenant card: %+v", row)
				}
			}
			head, err := r.BoardSequence(t.Context(), "example")
			sequence = head.Sequence
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return sequence
	}
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_first", 32)
	assertPage("org_second", 0)
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	sequence := assertPage("org_first", 33)
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := assertPage("org_first", 33); got != sequence {
		t.Fatalf("unchanged refresh advanced sequence %d to %d", sequence, got)
	}
	service.localOrganization, service.boardRefreshCursor = "org_second", ""
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_second", 32)
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RetireProject(t.Context(), "example", 1, now) }); err != nil {
		t.Fatal(err)
	}
	service.localOrganization, service.boardRefreshCursor = "org_first", ""
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_first", 1)
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_first", 0)
	assertPage("org_second", 32)
}
