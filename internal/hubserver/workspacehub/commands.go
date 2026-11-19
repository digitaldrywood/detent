package workspacehub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func NativeOperation(scope hubscope.NativeScope, method, suffix string) string {
	return method + " /api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + suffix
}

func (o *Operations) CreateRecord(ctx context.Context, scope hubscope.NativeScope, request Request) (json.RawMessage, error) {
	err := o.RequireWorkspaces()
	if err != nil {
		return nil, err
	}
	if o.Repository() == nil || o.AccessRepository() == nil {
		return nil, hubapi.WorkspaceUnavailable()
	}
	request, err = normalizeWorkspaceRequest(request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	operation := NativeOperation(scope, "POST", "/workspaces")
	content := eventstore.NewContent(o.RunDetailStore())
	ctx = AllowLabel(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := o.RecheckProjectRecordMutation(ctx, scope); err != nil {
			return nil, err
		}
		var previous eventstore.Receipt
		var subject corestore.WorkItem
		var readOnly bool
		err = o.Repository().ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := o.AuthorizeWorkItemRecordScope(ctx, r, scope); err != nil {
				return err
			}
			var err error
			previous, err = r.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
			if err == nil {
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			subject, readOnly, err = o.workspaceCreationSubject(ctx, r, scope, request)
			return err
		})
		if err != nil {
			return nil, err
		}
		if previous.CommandKey != "" {
			if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && o.Clock().UnixNano() >= previous.ResponseReplayUntilNs {
				return nil, receiptstore.ErrExpired
			}
			if previous.RequestHash != hash {
				return nil, &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
			}
			return content.Receipt(ctx, previous)
		}
		now := o.Clock().UTC().Truncate(time.Microsecond)
		record := Record{ID: hubapi.NewID("ws"), OrganizationID: scope.Organization, ProjectID: scope.Project, SubjectWorkItemID: string(subject.NativeID), AttemptID: strings.TrimSpace(request.AttemptID), Ref: strings.TrimSpace(request.Ref), State: workspacesession.StateRequested, Requires: request.Requires, ReadOnly: readOnly, IdleTimeoutSeconds: o.Config().IdleTimeoutSeconds(), ExpiresAt: now.Add(o.Config().MaxLifetime), RequestedExpiresAt: now.Add(o.Config().RequestTimeout), CreatedBy: scope.Credential.ID, Revision: 1, CreatedAt: now, UpdatedAt: now}
		project, err := o.ReadProjectRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		state, found := o.FirstDispatchableState(tracker.NativeProject{States: project.States})
		if !found {
			return nil, hubapi.NativePublicInvalid("The project has no dispatchable workflow state for a workspace session")
		}
		create := tracker.CreateIssue{Title: workspaceItemTitle(record.ID), Body: workspaceItemBody(record), State: state, Labels: []string{ItemLabel}}
		plan, err := o.LoadWorkItemPlan(ctx, scope, "", "", create)
		if err != nil {
			return nil, err
		}
		plan, err = o.PlanWorkItemRecord(ctx, scope, plan, create)
		if err != nil {
			return nil, err
		}
		plan.Issue.IssueContract.Exempt = true
		policy, policyFound, err := o.ValidateWorkItemRecordFilingTenant(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		attachmentIDs, err := o.PrepareWorkItemAttachmentReferences(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		fields, err := o.PrepareWorkItemFields(ctx, scope.Organization, plan)
		if err != nil {
			return nil, err
		}
		issueRaw, err := json.Marshal(plan.Issue)
		if err != nil {
			return nil, err
		}
		version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, RecordID: string(plan.Issue.WorkItemID), Revision: 1}, issueRaw)
		if err != nil {
			return nil, err
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: plan.Issue.WorkItemID, Type: "issue.created", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: plan.Issue.UpdatedAt, Data: tracker.CollaborationData{Revision: 1}})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(record.Resource())
		if err != nil {
			return nil, err
		}
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		authority, err := o.BeginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		var replay eventstore.Receipt
		err = o.Repository().WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockAccessOrganization(ctx); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if err := o.AuthorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			currentProject, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			if currentProject.Core.DeletedAt != nil || currentProject.Core.Profile != "native" {
				return hubapi.NativeNotFound()
			}
			if !hubrecords.SameWorkItemProjectPlan(plan.Project, currentProject) {
				return corestore.ErrSerializationFailure
			}
			previous, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
			if err == nil {
				if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && o.Clock().UnixNano() >= previous.ResponseReplayUntilNs {
					return receiptstore.ErrExpired
				}
				if previous.RequestHash != hash {
					return &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
				}
				replay = previous
				return o.RecordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusCreated)
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			currentSubject, currentReadOnly, err := o.workspaceCreationSubject(ctx, w, scope, request)
			if err != nil {
				return err
			}
			if currentSubject.ID != subject.ID || currentSubject.Revision != subject.Revision || currentReadOnly != readOnly {
				return corestore.ErrSerializationFailure
			}
			if err := hubapi.FromFeatureError(o.Plans().RequireFeatureRecord(ctx, w, "collaboration", o.Clock())); err != nil {
				return err
			}
			if err := o.ChargeProjectCommand(ctx, w, scope, operation, request.IdempotencyKey); err != nil {
				return err
			}
			currentPolicy, err := w.ProjectPolicy(ctx, scope.Project)
			if errors.Is(err, corestore.ErrNotFound) {
				if policyFound {
					return corestore.ErrSerializationFailure
				}
			} else if err != nil {
				return err
			} else if !policyFound || !hubrecords.SameNativeClaimProjectPolicy(policy, currentPolicy) {
				return corestore.ErrSerializationFailure
			}
			owner, ok := w.(postgres.WorkspaceWriter)
			if !ok {
				return hubapi.WorkspaceUnavailable()
			}
			if record.AttemptID != "" {
				id, err := owner.WorkspaceOpenAttempt(ctx, scope.Project, record.AttemptID)
				if err != nil {
					return err
				}
				if id != "" {
					return workspaceExists(id)
				}
			}
			total, mine, err := owner.WorkspaceOpenCounts(ctx, scope.Credential.ID, o.Config().PlanMaxOpen)
			if err != nil {
				return err
			}
			if total >= o.Config().PlanMaxOpen {
				return workspaceLimit("organization", o.Config().PlanMaxOpen)
			}
			if mine >= o.Config().PersonMaxOpen {
				return workspaceLimit("person", o.Config().PersonMaxOpen)
			}
			if _, err := owner.CreateWorkspaceSession(ctx, postgres.WorkspaceCreate{OrganizationID: scope.Organization, ProjectID: scope.Project, ID: record.ID, SubjectWorkItemID: record.SubjectWorkItemID, AttemptID: record.AttemptID, Ref: record.Ref, Requires: record.Requires, ReadOnly: record.ReadOnly, IdleTimeoutSeconds: int32(record.IdleTimeoutSeconds), ExpiresAt: record.ExpiresAt, RequestedExpiresAt: record.RequestedExpiresAt, CreatedBy: record.CreatedBy, CreatedAt: record.CreatedAt}); err != nil {
				return err
			}
			if err := hubrecords.WriteWorkItemRecord(ctx, w, plan, fields); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, event); err != nil {
				return err
			}
			for _, id := range attachmentIDs.Current {
				if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.Project, AttachmentID: id, WorkItemID: plan.Issue.WorkItemID}); err != nil {
					return err
				}
			}
			if err := owner.PutWorkspaceItem(ctx, claimstore.WorkspaceItem{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, WorkspaceID: record.ID, CreatedAt: now}); err != nil {
				return err
			}
			if err := o.RecordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusCreated); err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		err = errors.Join(err, hubrecords.ReleaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) {
			continue
		}
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		if replay.CommandKey != "" {
			return content.Receipt(ctx, replay)
		}
		o.Committed(ctx, record)
		return raw, nil
	}
}

func (o *Operations) RequireOperatorRunners(ctx context.Context, _ hubapi.NativeQueryer, scope hubscope.NativeScope) error {
	if scope.Credential.Scope != hubapi.APIScopeOperator && scope.Credential.Scope != hubapi.APIScopeAdmin {
		return hubapi.NativeNotFound()
	}
	if scope.Credential.Hosted == nil {
		return nil
	}
	return o.AccessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		if err := o.AuthorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		grant, err := reader.AccessProjectGrant(ctx, scope.Credential.Hosted.Subject, scope.Project, scope.Credential.HostedRole)
		if err != nil {
			return err
		}
		if !grant.ManageRunner {
			return hubapi.NativeNotFound()
		}
		return nil
	})
}

// Workspace failures, in the native error shape of the conversation contract.

// workspaceExists reports a second request on an attempt that already has an
// open workspace. It names the existing id, because the client's next move is
// to use it rather than to retry.
func workspaceExists(id string) error {
	return &hubapi.NativeError{
		Code: "workspace_exists", Message: "A workspace is already open on this attempt",
		Status: http.StatusConflict, Details: map[string]any{"workspace_id": id},
		PublicMessage: true,
	}
}

// workspaceLimit reports the per-organization or per-person cap. It names
// which one was reached, so the answer is actionable rather than only a
// refusal.
func workspaceLimit(scope string, limit int) error {
	return &hubapi.NativeError{
		Code: "workspace_limit", Message: "The open workspace limit has been reached",
		Status: http.StatusUnprocessableEntity, Details: map[string]any{"scope": scope, "limit": limit},
		PublicMessage: true,
	}
}
