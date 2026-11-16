package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func nativeOperation(scope hubscope.NativeScope, method, suffix string) string {
	return method + " /api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + suffix
}

func (s *Service) commandCreateWorkspace(ctx context.Context, scope hubscope.NativeScope, request workspaceRequest) (json.RawMessage, error) {
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
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
	operation := nativeOperation(scope, "POST", "/workspaces")
	content := eventstore.NewContent(s.runDetailStore())
	ctx = allowWorkspaceLabel(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
			return nil, err
		}
		var previous eventstore.Receipt
		var subject corestore.WorkItem
		var readOnly bool
		err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
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
			subject, readOnly, err = service.workspaceCreationSubject(ctx, r, scope, request)
			return err
		})
		if err != nil {
			return nil, err
		}
		if previous.CommandKey != "" {
			if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= previous.ResponseReplayUntilNs {
				return nil, receiptstore.ErrExpired
			}
			if previous.RequestHash != hash {
				return nil, &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
			}
			return content.Receipt(ctx, previous)
		}
		now := s.config.Clock().UTC().Truncate(time.Microsecond)
		record := workspaceRecord{ID: hubapi.NewID("ws"), OrganizationID: scope.Organization, ProjectID: scope.Project, SubjectWorkItemID: string(subject.NativeID), AttemptID: strings.TrimSpace(request.AttemptID), Ref: strings.TrimSpace(request.Ref), State: workspacesession.StateRequested, Requires: request.Requires, ReadOnly: readOnly, IdleTimeoutSeconds: service.config.IdleTimeoutSeconds(), ExpiresAt: now.Add(service.config.MaxLifetime), RequestedExpiresAt: now.Add(service.config.RequestTimeout), CreatedBy: scope.Credential.ID, Revision: 1, CreatedAt: now, UpdatedAt: now}
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		state, found := firstDispatchableState(tracker.NativeProject{States: project.States})
		if !found {
			return nil, hubapi.NativePublicInvalid("The project has no dispatchable workflow state for a workspace session")
		}
		create := tracker.CreateIssue{Title: workspaceItemTitle(record.ID), Body: workspaceItemBody(record), State: state, Labels: []string{workspaceItemLabel}}
		plan, err := s.loadWorkItemPlan(ctx, scope, "", "", create)
		if err != nil {
			return nil, err
		}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, create)
		if err != nil {
			return nil, err
		}
		plan.Issue.IssueContract.Exempt = true
		policy, policyFound, err := s.validateWorkItemRecordFilingTenant(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		attachmentIDs, err := s.prepareWorkItemAttachmentReferences(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		fields, err := s.prepareWorkItemFields(ctx, scope.Organization, plan)
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
		search, err := searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, ID: "issue:" + string(plan.Issue.WorkItemID), SourceID: string(plan.Issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: plan.Issue.UpdatedAt}, searchstore.Text{Title: plan.Issue.Title, Body: plan.Issue.Body, Labels: plan.Issue.Labels})
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(record.resource())
		if err != nil {
			return nil, err
		}
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		authority, err := s.beginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		var replay eventstore.Receipt
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockAccessOrganization(ctx); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
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
				if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= previous.ResponseReplayUntilNs {
					return receiptstore.ErrExpired
				}
				if previous.RequestHash != hash {
					return &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
				}
				replay = previous
				return s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusCreated)
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			currentSubject, currentReadOnly, err := service.workspaceCreationSubject(ctx, w, scope, request)
			if err != nil {
				return err
			}
			if currentSubject.ID != subject.ID || currentSubject.Revision != subject.Revision || currentReadOnly != readOnly {
				return corestore.ErrSerializationFailure
			}
			if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", s.config.Clock())); err != nil {
				return err
			}
			if err := s.chargeProjectCommand(ctx, w, scope, operation, request.IdempotencyKey); err != nil {
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
			total, mine, err := owner.WorkspaceOpenCounts(ctx, scope.Credential.ID, service.config.PlanMaxOpen)
			if err != nil {
				return err
			}
			if total >= service.config.PlanMaxOpen {
				return workspaceLimit("organization", service.config.PlanMaxOpen)
			}
			if mine >= service.config.PersonMaxOpen {
				return workspaceLimit("person", service.config.PersonMaxOpen)
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
			if err := w.PutSearch(ctx, search, 0); err != nil {
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
			if err := s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusCreated); err != nil {
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
		service.committed(ctx, record)
		return raw, nil
	}
}

func (s *Service) commandCreateAction(ctx context.Context, scope hubscope.NativeScope, request projectActionRequest) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "POST", "", request.Mutation, request)
}
func (s *Service) commandPatchAction(ctx context.Context, scope hubscope.NativeScope, id string, request projectActionPatch) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "PATCH", id, request.Mutation, request)
}
func (s *Service) commandRunAction(ctx context.Context, scope hubscope.NativeScope, id string, request projectActionRunRequest) (json.RawMessage, error) {
	return s.queueActionRunRecord(ctx, scope, id, request)
}
func (s *Service) commandDeleteAction(ctx context.Context, scope hubscope.NativeScope, id string) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "DELETE", id, tracker.Mutation{}, nil)
}

func (s *Service) readAttachment(ctx context.Context, scope hubscope.NativeScope, conversationID, id string) (conversationAttachmentRecord, []byte, error) {
	return s.readAttachmentPG(ctx, scope, conversationID, id)
}

func (s *Service) readRecording(ctx context.Context, scope hubscope.NativeScope, workspaceID, id string) (terminalRecordingRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	service, err := s.requireWorkspaces()
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	var row postgres.TerminalRecording
	var recording terminalRecordingRecord
	err = s.accessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		if _, err := service.readRelayWorkspaceForActor(ctx, reader, scope, workspaceID); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		row, err = owner.TerminalRecording(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if row.WorkspaceID != workspaceID {
			return hubapi.NativeNotFound()
		}
		recording = terminalRecordingFromPostgres(row)
		if !terminalRecordingReadable(scope, recording) {
			return hubapi.NativeNotFound()
		}
		return nil
	})
	if err != nil {
		return terminalRecordingRecord{}, hubapi.ProjectRecordError(err)
	}
	ref := attachment.ArchiveReference{Key: row.CastObjectKey, SHA256: row.CastSha256, CompressedSize: row.CastBytes, UncompressedSize: row.CastContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	if ref.Validate(string(scope.Organization)) != nil || ref.UncompressedSize > workspacesession.MaxRecordingBytes {
		return terminalRecordingRecord{}, artifact.ErrIntegrity
	}
	ref, err = s.relayArchiveReference(ctx, scope.Organization, ref)
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	raw, err := hubcontent.ReadChangeContent(s.contentContext(ctx), string(scope.Organization), ref)
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	recording.Cast = string(raw)
	return recording, nil
}
func (s *Service) readRecordings(ctx context.Context, scope hubscope.NativeScope, workspaceID, relaySessionID string, limit int) ([]TerminalRecording, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	var rows []postgres.TerminalRecording
	err = s.accessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		if _, err := service.readRelayWorkspaceForActor(ctx, reader, scope, workspaceID); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		rows, err = owner.RecentTerminalRecordings(ctx, scope.Project, workspaceID, relaySessionID, terminalRecordingPage)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	result := []TerminalRecording{}
	for _, row := range rows {
		record := terminalRecordingFromPostgres(row)
		if terminalRecordingReadable(scope, record) {
			result = append(result, record.Resource())
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

// runner grants are required for headless execution too. A token retains the
// dashboard's existing non-person restrictions on interactive terminals.
func (s *Service) requireOperatorRunners(ctx context.Context, _ hubapi.NativeQueryer, scope hubscope.NativeScope) error {
	if scope.Credential.Scope != hubapi.APIScopeOperator && scope.Credential.Scope != hubapi.APIScopeAdmin {
		return hubapi.NativeNotFound()
	}
	if scope.Credential.Hosted == nil {
		return nil
	}
	return s.accessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
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
