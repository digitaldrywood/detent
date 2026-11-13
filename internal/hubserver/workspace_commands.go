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
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func nativeOperation(scope nativeScope, method, suffix string) string {
	return method + " /api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + suffix
}

func (s *Service) commandCreateWorkspace(ctx context.Context, scope nativeScope, request workspaceRequest) (json.RawMessage, error) {
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return nil, workspaceUnavailable()
	}
	request, err = normalizeWorkspaceRequest(request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := nativeCommandHash(request)
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
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
				return err
			}
			var err error
			previous, err = r.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
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
			if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= previous.ResponseReplayUntilNs {
				return nil, receiptstore.ErrExpired
			}
			if previous.RequestHash != hash {
				return nil, &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
			}
			return content.Receipt(ctx, previous)
		}
		now := s.config.now().UTC().Truncate(time.Microsecond)
		record := workspaceRecord{ID: newNativeID("ws"), OrganizationID: scope.organization, ProjectID: scope.project, SubjectWorkItemID: string(subject.NativeID), AttemptID: strings.TrimSpace(request.AttemptID), Ref: strings.TrimSpace(request.Ref), State: workspacesession.StateRequested, Requires: request.Requires, ReadOnly: readOnly, IdleTimeoutSeconds: service.config.IdleTimeoutSeconds(), ExpiresAt: now.Add(service.config.MaxLifetime), RequestedExpiresAt: now.Add(service.config.RequestTimeout), CreatedBy: scope.credential.ID, Revision: 1, CreatedAt: now, UpdatedAt: now}
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		state, found := firstDispatchableState(tracker.NativeProject{States: project.States})
		if !found {
			return nil, nativePublicInvalid("The project has no dispatchable workflow state for a workspace session")
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
		plan.issue.IssueContract.Exempt = true
		policy, policyFound, err := s.validateWorkItemRecordFilingTenant(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		attachmentIDs, err := s.prepareWorkItemAttachmentReferences(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		fields, err := s.prepareWorkItemFields(ctx, scope.organization, plan)
		if err != nil {
			return nil, err
		}
		issueRaw, err := json.Marshal(plan.issue)
		if err != nil {
			return nil, err
		}
		version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: 1}, issueRaw)
		if err != nil {
			return nil, err
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: plan.issue.WorkItemID, Type: "issue.created", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: plan.issue.UpdatedAt, Data: tracker.CollaborationData{Revision: 1}})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return nil, err
		}
		search, err := searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, ID: "issue:" + string(plan.issue.WorkItemID), SourceID: string(plan.issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: plan.issue.UpdatedAt}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(record.resource())
		if err != nil {
			return nil, err
		}
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		authority, err := s.beginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		var replay eventstore.Receipt
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockAccessOrganization(ctx); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			currentProject, err := w.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if currentProject.Core.DeletedAt != nil || currentProject.Core.Profile != "native" {
				return nativeNotFound()
			}
			if !sameWorkItemProjectPlan(plan.project, currentProject) {
				return corestore.ErrSerializationFailure
			}
			previous, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			if err == nil {
				if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= previous.ResponseReplayUntilNs {
					return receiptstore.ErrExpired
				}
				if previous.RequestHash != hash {
					return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
				}
				replay = previous
				return s.recordEntryAccessKeyAuditWith(ctx, w, entryKeyAuditFrom(ctx), http.StatusCreated)
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
			if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
				return err
			}
			currentPolicy, err := w.ProjectPolicy(ctx, scope.project)
			if errors.Is(err, corestore.ErrNotFound) {
				if policyFound {
					return corestore.ErrSerializationFailure
				}
			} else if err != nil {
				return err
			} else if !policyFound || !sameNativeClaimProjectPolicy(policy, currentPolicy) {
				return corestore.ErrSerializationFailure
			}
			owner, ok := w.(postgres.WorkspaceWriter)
			if !ok {
				return workspaceUnavailable()
			}
			if record.AttemptID != "" {
				id, err := owner.WorkspaceOpenAttempt(ctx, scope.project, record.AttemptID)
				if err != nil {
					return err
				}
				if id != "" {
					return workspaceExists(id)
				}
			}
			total, mine, err := owner.WorkspaceOpenCounts(ctx, scope.credential.ID, service.config.PlanMaxOpen)
			if err != nil {
				return err
			}
			if total >= service.config.PlanMaxOpen {
				return workspaceLimit("organization", service.config.PlanMaxOpen)
			}
			if mine >= service.config.PersonMaxOpen {
				return workspaceLimit("person", service.config.PersonMaxOpen)
			}
			if _, err := owner.CreateWorkspaceSession(ctx, postgres.WorkspaceCreate{OrganizationID: scope.organization, ProjectID: scope.project, ID: record.ID, SubjectWorkItemID: record.SubjectWorkItemID, AttemptID: record.AttemptID, Ref: record.Ref, Requires: record.Requires, ReadOnly: record.ReadOnly, IdleTimeoutSeconds: int32(record.IdleTimeoutSeconds), ExpiresAt: record.ExpiresAt, RequestedExpiresAt: record.RequestedExpiresAt, CreatedBy: record.CreatedBy, CreatedAt: record.CreatedAt}); err != nil {
				return err
			}
			if err := writeWorkItemRecord(ctx, w, plan, fields); err != nil {
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
			for _, id := range attachmentIDs.current {
				if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.project, AttachmentID: id, WorkItemID: plan.issue.WorkItemID}); err != nil {
					return err
				}
			}
			if err := owner.PutWorkspaceItem(ctx, claimstore.WorkspaceItem{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, WorkspaceID: record.ID, CreatedAt: now}); err != nil {
				return err
			}
			if err := s.recordEntryAccessKeyAuditWith(ctx, w, entryKeyAuditFrom(ctx), http.StatusCreated); err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		err = errors.Join(err, releaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) {
			continue
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		if replay.CommandKey != "" {
			return content.Receipt(ctx, replay)
		}
		service.committed(ctx, record)
		return raw, nil
	}
}

func (s *Service) commandCreateAction(ctx context.Context, scope nativeScope, request projectActionRequest) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "POST", "", request.Mutation, request)
}
func (s *Service) commandPatchAction(ctx context.Context, scope nativeScope, id string, request projectActionPatch) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "PATCH", id, request.Mutation, request)
}
func (s *Service) commandRunAction(ctx context.Context, scope nativeScope, id string, request projectActionRunRequest) (json.RawMessage, error) {
	return s.queueActionRunRecord(ctx, scope, id, request)
}
func (s *Service) commandDeleteAction(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "DELETE", id, tracker.Mutation{}, nil)
}

func (s *Service) readAttachment(ctx context.Context, scope nativeScope, conversationID, id string) (conversationAttachmentRecord, []byte, error) {
	return s.readAttachmentPG(ctx, scope, conversationID, id)
}

func (s *Service) readRecording(ctx context.Context, scope nativeScope, workspaceID, id string) (terminalRecordingRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	service, err := s.requireWorkspaces()
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	var row postgres.TerminalRecording
	var recording terminalRecordingRecord
	err = s.accessRead(ctx, scope.organization, func(reader tenantstore.Reader) error {
		if _, err := service.readRelayWorkspaceForActor(ctx, reader, scope, workspaceID); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		row, err = owner.TerminalRecording(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if row.WorkspaceID != workspaceID {
			return nativeNotFound()
		}
		recording = terminalRecordingFromPostgres(row)
		if !terminalRecordingReadable(scope, recording) {
			return nativeNotFound()
		}
		return nil
	})
	if err != nil {
		return terminalRecordingRecord{}, projectRecordError(err)
	}
	ref := attachment.ArchiveReference{Key: row.CastObjectKey, SHA256: row.CastSha256, CompressedSize: row.CastBytes, UncompressedSize: row.CastContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	if ref.Validate(string(scope.organization)) != nil || ref.UncompressedSize > workspacesession.MaxRecordingBytes {
		return terminalRecordingRecord{}, artifact.ErrIntegrity
	}
	ref, err = s.relayArchiveReference(ctx, scope.organization, ref)
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	raw, err := readChangeContent(s.contentContext(ctx), string(scope.organization), ref)
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	recording.Cast = string(raw)
	return recording, nil
}
func (s *Service) readRecordings(ctx context.Context, scope nativeScope, workspaceID, relaySessionID string, limit int) ([]TerminalRecording, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	var rows []postgres.TerminalRecording
	err = s.accessRead(ctx, scope.organization, func(reader tenantstore.Reader) error {
		if _, err := service.readRelayWorkspaceForActor(ctx, reader, scope, workspaceID); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		rows, err = owner.RecentTerminalRecordings(ctx, scope.project, workspaceID, relaySessionID, terminalRecordingPage)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
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
func (s *Service) requireOperatorRunners(ctx context.Context, _ nativeQueryer, scope nativeScope) error {
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin {
		return nativeNotFound()
	}
	if scope.credential.Hosted == nil {
		return nil
	}
	return s.accessRead(ctx, scope.organization, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		grant, err := reader.AccessProjectGrant(ctx, scope.credential.Hosted.Subject, scope.project)
		if err != nil {
			return err
		}
		if !grant.ManageRunner {
			return nativeNotFound()
		}
		return nil
	})
}
