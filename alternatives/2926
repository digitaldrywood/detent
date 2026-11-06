package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func configureHumanWorkflowProject(t *testing.T, service *Service, organization tracker.OrganizationID, project tracker.ProjectID) {
	t.Helper()
	scope := nativeScope{organization: organization, project: project}
	original, err := service.readTenantProjectRecord(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	record := original
	record.States = append(slices.Clone(original.States), tracker.NativeState{Name: "Needs input", Transitions: []string{"Todo", "Cancelled"}})
	for i := range record.States {
		if record.States[i].Name == "Backlog" {
			record.States[i].Transitions = append(slices.Clone(record.States[i].Transitions), "Blocked", "Needs input")
		}
	}
	record.Core.WorkflowSource, record.Core.WorkflowSourceRevision = "detent.yaml", strings.Repeat("a", 40)
	descriptor := configuredHumanWorkflowPolicy(hubTestPolicy(), record.States)
	descriptor.Configuration = &policy.Configuration{Behavior: json.RawMessage(`{"BacklogAdmission":{"Sources":{"States":["Backlog"]},"TargetState":"Todo"}}`), Prompt: "Follow the approved workflow"}
	descriptor = descriptor.WithID()
	approved, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	approved.ProjectID, approved.ApprovedBy, approved.ApprovedAt = project, "credential_owner", service.config.now()
	var previousApproval policystore.Revision
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		previous, err := w.ProjectPolicy(t.Context(), project)
		if err != nil {
			return err
		}
		previousApproval = previous
		if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
			return err
		}
		return w.ApproveProjectPolicy(t.Context(), approved, previous.PolicyID)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if err := service.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			current, err := w.ProjectRecord(ctx, project)
			if err != nil {
				return err
			}
			current.Core.WorkflowSource, current.Core.WorkflowSourceRevision = original.Core.WorkflowSource, original.Core.WorkflowSourceRevision
			current.States = original.States
			if _, err := w.UpdateProjectRecord(ctx, current); err != nil {
				return err
			}
			return w.ApproveProjectPolicy(ctx, previousApproval, approved.PolicyID)
		}); err != nil {
			t.Error(err)
		}
	})
}

func seedConfiguredHumanWorkflowIssue(t *testing.T, service *Service, organization tracker.OrganizationID, project tracker.ProjectID, token string, request tracker.CreateIssue) tracker.NativeIssue {
	t.Helper()
	credential, status, err := service.authenticateAccessHash(t.Context(), apikey.HashToken(token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate fixture actor: status=%d error=%v", status, err)
	}
	scope := nativeScope{organization: organization, project: project, credential: credential, action: operatortool.FileIssue}
	plan, err := service.loadWorkItemPlan(t.Context(), scope, "", "", request)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = service.planWorkItemRecord(t.Context(), scope, plan, request)
	var unavailable *nativeError
	if err != nil && (!errors.As(err, &unavailable) || unavailable.status != http.StatusServiceUnavailable) || plan.project.Core.WorkflowSource == "" || plan.issue.IssueContract == nil || !plan.create || plan.issue.WorkItemID == "" {
		t.Fatalf("expected complete configured-source plan: %v", err)
	}
	policyRevision, policyFound, err := service.validateWorkItemRecordFilingTenant(t.Context(), scope, plan)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := service.prepareWorkItemFields(t.Context(), organization, plan)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan.issue)
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: int64(plan.issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: organization, ProjectID: project, AggregateID: plan.issue.WorkItemID, Type: plan.eventType, SchemaVersion: 1, Actor: scope.actor(), RecordedAt: plan.issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.issue.Revision, Fields: plan.fields}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), organization, eventRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := service.authorizeNativeWorkflowScope(t.Context(), w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(t.Context(), project); err != nil {
			return err
		}
		currentProject, err := w.ProjectRecord(t.Context(), project)
		if err != nil {
			return err
		}
		if !sameWorkItemProjectPlan(plan.project, currentProject) {
			return corestore.ErrRevisionConflict
		}
		currentPolicy, err := w.ProjectPolicy(t.Context(), project)
		if errors.Is(err, corestore.ErrNotFound) {
			if policyFound {
				return corestore.ErrRevisionConflict
			}
		} else if err != nil {
			return err
		} else if !policyFound || !sameNativeClaimProjectPolicy(policyRevision, currentPolicy) {
			return corestore.ErrRevisionConflict
		}
		if err := writeWorkItemRecord(t.Context(), w, plan, fields); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		_, err = w.AppendEvent(t.Context(), event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return plan.issue
}
