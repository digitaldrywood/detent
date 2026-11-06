package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkspaceClaimAssociationIsTenantScopedAndPermanent(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	tenantRepository := tenantstore.Repository(repository)
	now := time.Date(2026, time.November, 1, 20, 0, 0, 0, time.UTC)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.Write(t.Context(), organization, corestore.Serving, func(w corestore.Writer) error {
			if err := seedCoreOrganization(t.Context(), w, organization); err != nil {
				return err
			}
			project, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "prj_shared", Name: "Example", Profile: "native"})
			if err != nil {
				return err
			}
			_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project.ID, NativeID: "wi_shared", Number: 1, Title: "Workspace item"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		item := claimstore.WorkspaceItem{OrganizationID: organization, ProjectID: "prj_shared", WorkItemID: "wi_shared", WorkspaceID: "ws_" + string(organization), CreatedAt: now}
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.PutWorkspaceItem(t.Context(), item)
		}); err != nil {
			t.Fatalf("put workspace association for %s: %v", organization, err)
		}
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.PutWorkspaceItem(t.Context(), item)
		}); err != nil {
			t.Fatalf("replay workspace association for %s: %v", organization, err)
		}
		var found bool
		if err := tenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			found, err = r.IsWorkspaceWorkItem(t.Context(), item.ProjectID, item.WorkItemID)
			return err
		}); err != nil || !found {
			t.Fatalf("workspace association for %s found=%t error=%v", organization, found, err)
		}
	}
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		found, err := r.IsWorkspaceWorkItem(t.Context(), "prj_shared", "wi_other")
		if err != nil {
			return err
		}
		if found {
			t.Fatal("unassociated work item is classified as a workspace")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	closedAt := now.Add(time.Minute)
	if err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CloseWorkspaceItem(t.Context(), "prj_shared", "wi_shared", closedAt)
	}); err != nil {
		t.Fatal(err)
	}
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		found, err := r.IsWorkspaceWorkItem(t.Context(), "prj_shared", "wi_shared")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("closed workspace association became ordinary project work")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	conflict := claimstore.WorkspaceItem{OrganizationID: "org_first", ProjectID: "prj_shared", WorkItemID: "wi_shared", WorkspaceID: "ws_reassigned", CreatedAt: now}
	err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.PutWorkspaceItem(t.Context(), conflict) })
	if !errors.Is(err, corestore.ErrUniqueViolation) {
		t.Fatalf("reassigned workspace association error=%v, want unique violation", err)
	}
}

func TestClaimCandidatePageUsesTenantScopedTypedFactsAndBoundedCursor(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	tenantRepository := tenantstore.Repository(repository)
	now := time.Date(2026, time.November, 1, 21, 0, 0, 0, time.UTC)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := seedCoreOrganization(t.Context(), w, organization); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
				Core:   corestore.Project{OrganizationID: organization, ID: "prj_claim", Name: "Claim example", Profile: "native", RequireDependencies: true},
				States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}},
			})
			return err
		}); err != nil {
			t.Fatalf("seed claim project for %s: %v", organization, err)
		}
	}
	invalidPriority := int64(9)
	invalidItem := corestore.WorkItem{OrganizationID: "org_first", ProjectID: "prj_claim", NativeID: "wi_rollback", Number: 4, Title: "Rollback example", WorkflowStateName: new("Todo"), CreatedAt: now, NativeCreatedAt: now}
	invalidFacts := claimstore.WorkItemFacts{OrganizationID: "org_first", ProjectID: "prj_claim", WorkItemID: "wi_rollback", Author: "author", Repository: "example/repository"}
	invalidQueue := claimstore.QueueEntry{OrganizationID: "org_first", ProjectID: "prj_claim", WorkItemID: "wi_rollback", Scope: "prj_claim", State: "Todo", Rank: "wi_rollback", Priority: &invalidPriority, CreatedAt: now, UpdatedAt: now}
	err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateWorkItemWithClaimFacts(t.Context(), invalidItem, nil, invalidFacts, invalidQueue)
		return err
	})
	if !errors.Is(err, claimstore.ErrInvalidCandidateFact) {
		t.Fatalf("invalid imported claim facts error=%v", err)
	}
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.WorkItem(t.Context(), "prj_claim", "wi_rollback")
		if !errors.Is(err, corestore.ErrNotFound) {
			return fmt.Errorf("failed import left a core item: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for i, id := range []tracker.NativeWorkItemID{"wi_first", "wi_second", "wi_workspace"} {
				state := "Todo"
				createdAt := now.Add(time.Duration(i) * time.Minute)
				assignees := []string{"worker_a"}
				if organization == "org_second" {
					assignees = []string{"worker_b"}
				}
				priority := int64(i)
				item := corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_claim", NativeID: id, Number: int64(i + 1), Title: string(id), WorkflowStateName: &state, CreatedAt: createdAt, NativeCreatedAt: createdAt}
				facts := claimstore.WorkItemFacts{OrganizationID: organization, ProjectID: "prj_claim", WorkItemID: id, Author: "AUTHOR", Repository: "EXAMPLE/REPOSITORY", Assignees: assignees, DispatchWaitSince: &createdAt}
				queue := claimstore.QueueEntry{OrganizationID: organization, ProjectID: "prj_claim", WorkItemID: id, Scope: "prj_claim", State: "Todo", Rank: string(id), Priority: &priority, CreatedAt: createdAt, UpdatedAt: createdAt}
				var labels []string
				if id != "wi_workspace" {
					labels = []string{"ready"}
				}
				if _, err := w.CreateWorkItemWithClaimFacts(t.Context(), item, labels, facts, queue); err != nil {
					return err
				}
			}
			return w.PutWorkspaceItem(t.Context(), claimstore.WorkspaceItem{OrganizationID: organization, ProjectID: "prj_claim", WorkItemID: "wi_workspace", WorkspaceID: "ws_claim_" + string(organization), CreatedAt: now})
		}); err != nil {
			t.Fatalf("seed claim facts for %s: %v", organization, err)
		}
	}
	query := claimstore.CandidateQuery{ProjectID: "prj_claim", Scope: "prj_claim", Assignees: []string{"WORKER_A"}, IncludeLabels: []string{"ready"}, AvailableAt: now, Limit: 1}
	var first claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		first, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].WorkItemID != "wi_first" || first.Items[0].Priority != 1 || first.Items[0].Author != "author" || first.Items[0].Repository != "example/repository" || len(first.Items[0].Assignees) != 1 || len(first.Items[0].Labels) != 1 {
		t.Fatalf("first candidate page = %+v", first)
	}
	requested := query
	requested.WorkItemIDs = []tracker.NativeWorkItemID{"wi_second", "wi_missing"}
	var selected claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		selected, err = r.CandidatePage(t.Context(), requested)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(selected.Items) != 1 || selected.Items[0].WorkItemID != "wi_second" {
		t.Fatalf("provider-filtered candidate page = %+v", selected)
	}
	query.After = first.Next
	var second claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		second, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].WorkItemID != "wi_second" {
		t.Fatalf("second candidate page = %+v", second)
	}
	query.After = claimstore.CandidateCursor{}
	query.Assignees = []string{"worker_b"}
	var otherTenant claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		otherTenant, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(otherTenant.Items) != 1 || otherTenant.Items[0].WorkItemID != "wi_first" {
		t.Fatalf("second tenant candidate page = %+v", otherTenant)
	}
	query.Assignees = []string{"worker_a"}
	query.IncludeLabels = nil
	query.Limit = 3
	query.WorkspaceLane = true
	var workspaceLane claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		workspaceLane, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var sawWorkspace bool
	for _, item := range workspaceLane.Items {
		if item.WorkItemID == "wi_workspace" {
			sawWorkspace = true
		}
	}
	if !sawWorkspace {
		t.Fatalf("workspace lane omitted associated item: %+v", workspaceLane)
	}
}

func TestProviderReservationSnapshotIsTypedAndTenantScoped(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	tenantRepository := tenantstore.Repository(repository)
	now := time.Date(2026, time.November, 1, 22, 0, 0, 0, time.UTC)
	percent := 34.5
	report := providercapacity.Report{
		Provider: "example-provider", Backend: "example-backend", AccountAlias: "example-account", SharedAccountAlias: "shared-example",
		Models: []string{"example-model"}, ModelDetails: []providercapacity.ModelDetail{{ID: "example-model", Label: "Example", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "low"}},
		UsageWindows:  []providercapacity.UsageWindow{{Kind: "weekly", PercentUsed: &percent, ObservedAt: now, ResetAt: now.Add(time.Hour)}},
		MaxConcurrent: 3, Availability: "available", ObservedAt: now,
	}
	reservation := providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "code", Backend: "example-backend", Model: "example-model"}, Report: report, Reason: "Current account capacity is available"}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := seedCoreOrganization(t.Context(), w, organization); err != nil {
				return err
			}
			project, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "prj_provider", Name: "Example", Profile: "native"})
			if err != nil {
				return err
			}
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project.ID, NativeID: "wi_provider", Number: 1, Title: "Example", CreatedAt: now, NativeCreatedAt: now})
			if err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_provider", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_provider", SessionID: "session_provider", LeaseID: "lease_shared", TTL: time.Minute})
			if err != nil {
				return err
			}
			return w.CreateProviderReservation(t.Context(), claimstore.ProviderReservation{OrganizationID: organization, LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: reservation})
		}); err != nil {
			t.Fatalf("seed provider reservation for %s: %v", organization, err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		var got claimstore.ProviderReservation
		if err := tenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			got, err = r.ProviderReservation(t.Context(), "lease_shared")
			return err
		}); err != nil {
			t.Fatalf("read provider reservation for %s: %v", organization, err)
		}
		if got.OrganizationID != organization || got.LeaseID != "lease_shared" || got.Reservation.Requirement != reservation.Requirement || got.Reservation.Report.Provider != report.Provider || got.Reservation.Report.SharedAccountAlias != report.SharedAccountAlias || len(got.Reservation.Report.ModelDetails) != 1 || len(got.Reservation.Report.ModelDetails[0].ReasoningEfforts) != 2 || len(got.Reservation.Report.UsageWindows) != 1 || got.Reservation.Report.UsageWindows[0].PercentUsed == nil || *got.Reservation.Report.UsageWindows[0].PercentUsed != percent || got.Reservation.Reason != reservation.Reason {
			t.Fatalf("provider reservation snapshot for %s = %+v", organization, got)
		}
	}
	err := tenantRepository.ReadTenant(t.Context(), "org_missing", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ProviderReservation(t.Context(), "lease_shared")
		return err
	})
	if !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("cross-tenant provider reservation read error=%v", err)
	}
}
