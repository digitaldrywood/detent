package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRecoveryEligibilityIncludesHiddenDependencyAuthority(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Blocked"}, {Name: "In Progress"}, {Name: "Done", Terminal: true}}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		var ownedLease leasestore.Lease
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := seedCoreOrganization(t.Context(), w, organization); err != nil {
				return err
			}
			if err := w.LockRelationshipGraph(t.Context()); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"prj_shared", "prj_private", "prj_optional"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native", RequireDependencies: project != "prj_optional"}, States: states}); err != nil {
					return err
				}
			}
			for index, value := range []struct {
				project  tracker.ProjectID
				id       tracker.NativeWorkItemID
				state    string
				archived bool
			}{
				{project: "prj_shared", id: "wi_ready", state: "Todo"},
				{project: "prj_shared", id: "wi_held", state: "Blocked"},
				{project: "prj_shared", id: "wi_terminal", state: "Done"},
				{project: "prj_shared", id: "wi_hidden", state: "Todo"},
				{project: "prj_shared", id: "wi_resolved", state: "Todo"},
				{project: "prj_shared", id: "wi_no_workflow"},
				{project: "prj_shared", id: "wi_archived", state: "Todo", archived: true},
				{project: "prj_private", id: "wi_private_open", state: "Blocked"},
				{project: "prj_private", id: "wi_private_done", state: "Done"},
				{project: "prj_optional", id: "wi_optional", state: "Todo"},
				{project: "prj_shared", id: "wi_owned_progress", state: "In Progress"},
				{project: "prj_shared", id: "wi_orphan_progress", state: "In Progress"},
			} {
				var state *string
				if value.state != "" {
					state = &value.state
				}
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: value.project, NativeID: value.id, Number: int64(index + 1), Title: "Example", WorkflowStateName: state, Archived: value.archived}); err != nil {
					return err
				}
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_owned", Hostname: "example", Capacity: 1}); err != nil {
				return err
			}
			owned, err := w.WorkItem(t.Context(), "prj_shared", "wi_owned_progress")
			if err != nil {
				return err
			}
			ownedLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: owned.ID, ExpectedRevision: owned.Revision, MachineID: "machine_owned", SessionID: "session_owned", LeaseID: "lease_owned", TTL: time.Hour})
			if err != nil {
				return err
			}
			for _, edge := range []struct {
				project       tracker.ProjectID
				item, blocker tracker.NativeWorkItemID
			}{
				{project: "prj_shared", item: "wi_hidden", blocker: "wi_private_open"},
				{project: "prj_shared", item: "wi_resolved", blocker: "wi_private_done"},
				{project: "prj_optional", item: "wi_optional", blocker: "wi_private_open"},
			} {
				if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: organization, ProjectID: edge.project, WorkItemID: edge.item, RelatedProjectID: "prj_private", RelatedWorkItemID: edge.blocker, Type: "dependency"}, "add", time.Now().UTC()); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(interface {
				RunnerRecoveryEligibility(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error)
			})
			if !ok {
				t.Errorf("%s recovery lacks authoritative scheduling facts", organization)
				return nil
			}
			for _, test := range []struct {
				project tracker.ProjectID
				item    tracker.NativeWorkItemID
				want    bool
			}{
				{project: "prj_shared", item: "wi_ready", want: true},
				{project: "prj_shared", item: "wi_held"},
				{project: "prj_shared", item: "wi_terminal"},
				{project: "prj_shared", item: "wi_hidden"},
				{project: "prj_shared", item: "wi_resolved", want: true},
				{project: "prj_shared", item: "wi_no_workflow"},
				{project: "prj_shared", item: "wi_archived"},
				{project: "prj_optional", item: "wi_optional", want: true},
				{project: "prj_shared", item: "wi_owned_progress", want: true},
				{project: "prj_shared", item: "wi_orphan_progress"},
			} {
				got, err := reader.RunnerRecoveryEligibility(t.Context(), test.project, test.item)
				if err != nil || got != test.want {
					t.Errorf("%s/%s eligibility=%t error=%v want %t", organization, test.item, got, err, test.want)
				}
			}
			if _, err := reader.RunnerRecoveryEligibility(t.Context(), "prj_private", "wi_ready"); !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("cross-project recovery eligibility error=%v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: ownedLease.LeaseID, FencingToken: ownedLease.FencingToken}, "completed")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader := r.(interface {
				RunnerRecoveryEligibility(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error)
			})
			eligible, err := reader.RunnerRecoveryEligibility(t.Context(), "prj_shared", "wi_owned_progress")
			if err == nil && eligible {
				t.Error("released non-dispatchable execution remained eligible for recovery")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
