package operatorcommands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s ScheduledAttributionDeps) commit(ctx context.Context, scope hubscope.NativeScope, operation, repository, item string, request tracker.CreateIssue) (json.RawMessage, error) {
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.Receipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	content := eventstore.NewContent(s.Details())
	var records []ScheduledAttributionRecord
	var original hubrecords.WorkItemRecordPlan
	if item != "" {
		original, err = s.LoadWorkItemPlan(ctx, scope, item, "", tracker.CreateComment{})
		if err != nil {
			return nil, err
		}
		if original.Issue.Archived {
			item = ""
		} else {
			plan := original
			if plan.Issue.State == "Done" {
				if err := hubrecords.ValidateNativeWorkflowTransition(plan.Project.States, "Done", "Rework", scope.Credential.Scope); err != nil {
					return nil, err
				}
				plan.Issue.State = "Rework"
				plan.Issue.Terminal = hubrecords.TenantProjectStateTerminal(plan.Project, "Rework")
				plan.Issue.Revision++
				plan.Issue.UpdatedAt = s.Now()
				plan.Issue.LastActivityAt = plan.Issue.UpdatedAt
				plan.EventType = "workflow.transitioned"
				p, err := s.PrepareRecord(ctx, scope, plan, tracker.CollaborationData{FromState: "Done", ToState: "Rework", Reason: string(gate.ReasonValidatorRework)})
				if err != nil {
					return nil, err
				}
				records = append(records, p)
				plan.Current.EventSequence++
			}
			plan, err = s.PlanWorkItemRecord(ctx, scope, plan, tracker.CreateComment{Body: request.Body})
			if err != nil {
				return nil, err
			}
			p, err := s.PrepareRecord(ctx, scope, plan, tracker.CollaborationData{})
			if err != nil {
				return nil, err
			}
			records = append(records, p)
		}
	}
	raw, err := json.Marshal(struct {
		Attributed bool `json:"attributed"`
	}{item != ""})
	if err != nil {
		return nil, err
	}
	now := s.Now().UTC()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.AuthorizeWorkItem(ctx, w, scope); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			replay = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		currentRepository := project.Settings.CheckoutRepository
		if currentRepository == "" {
			currentRepository = project.Settings.Repository
		}
		if currentRepository != repository {
			return corestore.ErrRevisionConflict
		}
		if err := s.RequireCollaboration(ctx, w, now); err != nil {
			return err
		}
		if item != "" {
			if !hubrecords.SameWorkItemProjectPlan(original.Project, project) {
				return corestore.ErrRevisionConflict
			}
			current, err := w.LockWorkItem(ctx, scope.Project, original.Current.NativeID)
			if err != nil {
				return err
			}
			if current.Archived || current.Revision != original.Current.Revision || current.EventSequence != original.Current.EventSequence {
				return corestore.ErrRevisionConflict
			}
		}
		for _, p := range records {
			if err := hubrecords.WriteWorkItemRecord(ctx, w, p.Plan, p.Fields); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(ctx, p.Version); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, p.Event); err != nil {
				return err
			}
		}
		if scope.Credential.Hosted != nil {
			audit := s.ProjectActor(scope).Audit
			audit.Event, audit.Route, audit.Status = "action", operation, http.StatusOK
			audit.RecordedAt = now
			audit.RecordedAtNS = now.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}
