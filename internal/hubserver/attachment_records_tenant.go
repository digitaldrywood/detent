package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func attachmentUploadIdentity(input attachment.UploadRequest) any {
	metadata := input.Metadata
	metadata.ID, metadata.ProjectID, metadata.Uploader = "", "", ""
	metadata.CreatedAt = time.Time{}
	if input.Evidence == nil {
		return metadata
	}
	return struct {
		Metadata attachment.Metadata
		Evidence *attachment.EvidenceRequest
	}{metadata, input.Evidence}
}
func attachmentAccountingUnavailable() error {
	return &nativeError{Code: "storage_owner_unavailable", Message: "Attachment accounting is unavailable with PostgreSQL storage", status: http.StatusServiceUnavailable, publicMessage: true}
}
func (s *Service) attachmentQuotaLimit(ctx context.Context, r tenantstore.Reader) (int64, error) {
	if s.database.plans() == nil {
		return math.MaxInt64, nil
	}
	if err := s.database.requireHostedFeatureRecord(ctx, r, "collaboration", s.config.now()); err != nil {
		return 0, err
	}
	entitlement, err := s.database.hostedEntitlementRecord(ctx, r, s.config.now())
	if err != nil {
		return 0, err
	}
	if limit, ok := entitlement.Allowances["collaboration_bytes"]; ok {
		return limit, nil
	}
	return math.MaxInt64, nil
}
func (s *Service) attachmentWriteAuthority(ctx context.Context, w tenantstore.Writer, scope nativeScope, evidence *attachment.EvidenceRequest) (corestore.WorkItem, error) {
	if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
		return corestore.WorkItem{}, err
	}
	project, err := w.Project(ctx, scope.project)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if project.DeletedAt != nil {
		return corestore.WorkItem{}, nativeNotFound()
	}
	if scope.credential.Scope != apiScopeWorker {
		return corestore.WorkItem{}, nil
	}
	credential, err := w.AccessCredential(ctx, scope.credential.ID)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	now := s.config.now().UTC()
	if credential.OrganizationID != scope.organization || credential.Kind != "service" || credential.Scope != string(apiScopeWorker) || !credential.Valid(scope.credential.Hash, now) || identity.OrganizationID != scope.organization || identity.RunnerID != scope.credential.Runner.RunnerID || identity.TokenID != credential.ID || identity.RemovedAt != nil || identity.State == "disabled" || !identity.ExpiresAt.After(now) {
		return corestore.WorkItem{}, runnerUnauthorized()
	}
	operation := runnerauth.Collaborate
	if evidence != nil {
		operation = runnerauth.Events
	}
	if !slices.Contains(identity.Operations, operation) {
		return corestore.WorkItem{}, runnerUnauthorized()
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if !allowed {
		return corestore.WorkItem{}, nativeNotFound()
	}
	if evidence == nil {
		return corestore.WorkItem{}, nil
	}
	item, err := w.LockWorkItem(ctx, scope.project, evidence.WorkItemID)
	if err != nil {
		return item, err
	}
	locker, ok := w.(leasestore.FencedLeaseLocker)
	if !ok {
		return item, workItemStorageUnavailable()
	}
	lease, err := locker.LockLease(ctx, string(evidence.LeaseID))
	if err != nil {
		return item, err
	}
	owner, err := w.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return item, err
	}
	now = s.config.now().UTC()
	if !credential.Valid(scope.credential.Hash, now) || item.Archived || lease.OrganizationID != scope.organization || lease.IssueID != item.ID || lease.FencingToken != int64(evidence.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || !runnerIdentityMatchesHeldLease(owner, scope.organization, identity.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) {
		return item, nativeStaleExecution("Evidence producer lease is no longer current")
	}
	attempt, err := w.Attempt(ctx, evidence.AttemptID)
	if err != nil {
		return item, err
	}
	if attempt.IssueID != item.ID || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.Status != "running" {
		return item, nativeStaleExecution("Evidence producer is not the running attempt")
	}
	return item, nil
}
func (s *Service) prepareCloudAttachmentTenant(c echo.Context, input attachment.UploadRequest) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	input.ID = conversation.NewAttachmentID()
	if input.Validate() != nil || strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil || input.Evidence != nil && input.Evidence.IdempotencyKey != input.RequestID {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment upload"))
	}
	scope := nativeRequestScope(c)
	scope.action = operatortool.UploadAttachment
	operation := "attachment.prepare " + string(scope.project)
	hash, err := nativeCommandHash(attachmentUploadIdentity(input))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	for {
		receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, input.RequestID, hash)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if found {
			raw, err := content.Receipt(ctx, receipt)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			var metadata attachment.Metadata
			if json.Unmarshal(raw, &metadata) != nil || metadata.Validate() != nil {
				return s.nativeAPIError(c, eventstore.ErrInvalidRecord)
			}
			err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.Attachment(ctx, scope.project, metadata.ID)
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				if stored.Metadata.DeletedAt != nil || stored.ObjectDeletedAt != nil {
					return nativeConflict(0)
				}
				return nil
			})
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			return c.JSONBlob(http.StatusOK, raw)
		}
		metadata := input.Metadata
		metadata.ProjectID, metadata.Uploader = string(scope.project), scope.credential.ID
		if scope.credential.HostedRole == "reporter" {
			metadata.Uploader = scope.actor().PrincipalID
		}
		metadata.CreatedAt = s.config.now().UTC().Truncate(time.Microsecond)
		raw, err := json.Marshal(metadata)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: input.RequestID, RequestHash: hash}, raw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if _, err := s.attachmentWriteAuthority(ctx, w, scope, input.Evidence); err != nil {
				return err
			}
			if previous, err := w.Receipt(ctx, scope.credential.ID, operation, input.RequestID); err == nil {
				if previous.RequestHash != hash {
					return nativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			{
				limit, err := s.attachmentQuotaLimit(ctx, w)
				if err != nil {
					return err
				}
				used, err := w.CollaborationBytes(ctx)
				if errors.Is(err, corestore.ErrNotFound) {
					return attachmentAccountingUnavailable()
				}
				if err != nil {
					return err
				}
				if metadata.Size > limit-used {
					return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
				}
			}
			_, err := w.PutReceipt(ctx, prepared)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		return c.JSONBlob(http.StatusOK, raw)
	}
}

type attachmentEvidencePublication struct {
	plan    workItemRecordPlan
	fields  workItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
	search  searchstore.Document
	bytes   int64
}

func (s *Service) prepareAttachmentEvidencePublication(ctx context.Context, scope nativeScope, input *attachment.EvidenceRequest, metadata attachment.Metadata) (attachmentEvidencePublication, error) {
	result := attachmentEvidencePublication{}
	if input == nil {
		return result, nil
	}
	row, version, err := s.readWorkItemRecord(ctx, scope, string(input.WorkItemID))
	if err != nil {
		return result, err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return result, err
	}
	at := s.config.now().UTC().Truncate(time.Microsecond)
	comment := tracker.NativeComment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: input.WorkItemID, ID: newNativeID("cmt"), Revision: 1, Sequence: row.EventSequence + 1, Body: escapeEvidenceCaption(input.Caption) + "\n\n" + metadata.Markdown(string(scope.organization)), Actor: scope.actor(), CreatedAt: at, UpdatedAt: at}
	result.plan = workItemRecordPlan{current: row, issue: issue, comment: comment, create: true, eventType: "comment.created"}
	result.fields, err = s.prepareWorkItemFields(ctx, scope.organization, result.plan)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(comment)
	if err != nil {
		return result, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	result.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: input.WorkItemID, RecordID: comment.ID, Revision: 1}, raw)
	if err != nil {
		return result, err
	}
	data := tracker.CollaborationData{Revision: 1, CommentID: comment.ID}
	event := tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: input.WorkItemID, Type: "comment.created", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: at, Data: data}
	eventRaw, err := json.Marshal(event)
	if err != nil {
		return result, err
	}
	result.event, err = content.PrepareEvent(ctx, scope.organization, eventRaw)
	if err != nil {
		return result, err
	}
	dataRaw, err := json.Marshal(data)
	if err != nil {
		return result, err
	}
	actorRaw, err := json.Marshal(event.Actor)
	if err != nil {
		return result, err
	}
	result.bytes = int64(len(comment.Body) + len(raw) + len(dataRaw) + len(actorRaw))
	result.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: input.WorkItemID, ID: "comment:" + comment.ID, SourceID: comment.ID, Kind: "comment", Scope: "issue", Revision: 1, UpdatedAt: at}, searchstore.Text{Body: comment.Body})
	return result, err
}
func (s *Service) recordCloudAttachmentTenant(c echo.Context, input attachment.UploadRequest) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if input.Validate() != nil || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil || input.Evidence != nil && (input.RequestID == "" || input.Evidence.IdempotencyKey != input.RequestID) {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment metadata"))
	}
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	scope.action = operatortool.UploadAttachment
	key := input.RequestID
	if key == "" {
		key = input.ID
	}
	operation := "attachment.record " + string(scope.project)
	identity := any(input.Metadata)
	if input.Evidence != nil {
		identity = input
	}
	hash, err := nativeCommandHash(identity)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	for {
		receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, key, hash)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if found {
			raw, err := content.Receipt(ctx, receipt)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			return c.JSONBlob(http.StatusCreated, raw)
		}
		metadata := input.Metadata
		if input.RequestID != "" {
			prepareHash, err := nativeCommandHash(attachmentUploadIdentity(input))
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			prepared, found, err := s.workItemRecordReceipt(ctx, scope, "attachment.prepare "+string(scope.project), input.RequestID, prepareHash)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			if !found {
				return s.nativeAPIError(c, nativePublicInvalid("Attachment preparation receipt is required"))
			}
			raw, err := content.Receipt(ctx, prepared)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			if json.Unmarshal(raw, &metadata) != nil || !reflect.DeepEqual(metadata, input.Metadata) {
				return s.nativeAPIError(c, nativeConflict(0))
			}
		} else {
			metadata.ProjectID, metadata.Uploader = string(scope.project), scope.credential.ID
			if scope.credential.HostedRole == "reporter" {
				metadata.Uploader = scope.actor().PrincipalID
			}
			metadata.CreatedAt = s.config.now().UTC().Truncate(time.Microsecond)
		}
		publication, err := s.prepareAttachmentEvidencePublication(ctx, scope, input.Evidence, metadata)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		record := attachmentstore.Record{OrganizationID: scope.organization, Metadata: metadata, Kind: "attachment"}
		if input.Evidence != nil {
			record.Metadata.WorkItemID = string(input.Evidence.WorkItemID)
			record.Metadata.CommentID = publication.plan.comment.ID
			record.EvidenceAttemptID = input.Evidence.AttemptID
			record.EvidenceCaption = input.Evidence.Caption
		}
		raw, err := json.Marshal(record.Metadata)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: key, RequestHash: hash}, raw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			row, err := s.attachmentWriteAuthority(ctx, w, scope, input.Evidence)
			if err != nil {
				return err
			}
			if previous, err := w.Receipt(ctx, scope.credential.ID, operation, key); err == nil {
				if previous.RequestHash != hash {
					return nativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if input.Evidence != nil && (row.ID != publication.plan.current.ID || row.Revision != publication.plan.current.Revision || row.EventSequence != publication.plan.current.EventSequence) {
				return corestore.ErrSerializationFailure
			}
			{
				limit, err := s.attachmentQuotaLimit(ctx, w)
				if err != nil {
					return err
				}
				usage, err := w.AdjustCollaborationBytes(ctx, metadata.Size+publication.bytes, limit)
				if errors.Is(err, corestore.ErrNotFound) {
					return attachmentAccountingUnavailable()
				}
				if err != nil {
					return err
				}
				if !usage.Allowed {
					return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
				}
			}
			if input.Evidence != nil {
				if err := writeWorkItemRecord(ctx, w, publication.plan, publication.fields); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(ctx, publication.version); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, publication.event); err != nil {
					return err
				}
				if err := w.PutSearch(ctx, publication.search, 0); err != nil {
					return err
				}
			}
			if err := w.CreateAttachment(ctx, record); err != nil {
				return err
			}
			if input.Evidence != nil {
				if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.project, AttachmentID: metadata.ID, WorkItemID: input.Evidence.WorkItemID, CommentID: publication.plan.comment.ID}); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, preparedReceipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) {
			continue
		}
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		return c.JSONBlob(http.StatusCreated, raw)
	}
}

func (s *Service) readAttachmentRecordTenant(ctx context.Context, scope nativeScope, id string) (attachment.Metadata, error) {
	var metadata attachment.Metadata
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error { return s.readAttachmentRecordWith(ctx, r, scope, id, &metadata) })
	return metadata, err
}

func (s *Service) readAttachmentRecordWith(ctx context.Context, r tenantstore.Reader, scope nativeScope, id string, metadata *attachment.Metadata) error {
	if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
		return err
	}
	record, err := r.Attachment(ctx, scope.project, id)
	if err != nil {
		return projectRecordError(err)
	}
	if record.Metadata.DeletedAt != nil || record.ObjectDeletedAt != nil || record.PendingContent != nil {
		return nativeNotFound()
	}
	key, err := attachment.Key(string(scope.organization), id)
	if err != nil {
		return err
	}
	if record.CustomerOwned || record.StorageBindingID != "" || record.ObjectKey != "" && record.ObjectKey != key {
		return workItemStorageUnavailable()
	}
	references, err := r.AttachmentReferences(ctx, scope.project, id, 201)
	if err != nil {
		return err
	}
	if len(references) > 200 {
		return nativePublicInvalid("Attachment references exceed the read bound")
	}
	*metadata = record.Metadata
	visible := scope.credential.HostedRole != "reporter" || metadata.Uploader == scope.credential.ID || metadata.Uploader == scope.actor().PrincipalID
	if !visible {
		uploader, err := r.AccessCredential(ctx, metadata.Uploader)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && uploader.Kind == "personal" && uploader.UserID != "" {
			member, err := r.AccessMember(ctx, uploader.UserID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			visible = err == nil && member.MembershipID == uploader.MembershipID && member.PrincipalID == scope.actor().PrincipalID
		}
	}
	if scope.credential.HostedRole == "reporter" && metadata.WorkItemID != "" {
		item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(metadata.WorkItemID))
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err != nil || !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
			metadata.WorkItemID, metadata.CommentID = "", ""
		}
	}
	for _, reference := range references {
		if scope.credential.HostedRole == "reporter" {
			item, err := r.WorkItem(ctx, scope.project, reference.WorkItemID)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
				continue
			}
		}
		visible = true
		metadata.ReferencedBy = append(metadata.ReferencedBy, attachment.SourceReference{WorkItemID: string(reference.WorkItemID), CommentID: reference.CommentID})
	}
	if !visible {
		return nativeNotFound()
	}
	return nil
}

func (s *Service) prepareCommentAttachmentReferences(ctx context.Context, scope nativeScope, plan workItemRecordPlan) ([]string, error) {
	if plan.comment.ID == "" || !plan.create {
		return nil, nil
	}
	references, err := cloudContentAttachmentReferences(plan.comment.Body, scope)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(references))
	for _, reference := range references {
		ids = append(ids, reference.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > 200 {
		return nil, nativePublicInvalid("Comment attachment references exceed the write bound")
	}
	for _, id := range ids {
		if _, err := s.readAttachmentRecordTenant(ctx, scope, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (s *Service) authorizeAttachmentCommentLease(ctx context.Context, w tenantstore.Writer, scope nativeScope, row corestore.WorkItem, command tracker.Mutation) error {
	current, err := w.LockWorkItem(ctx, scope.project, row.NativeID)
	if err != nil {
		return err
	}
	if current.ID != row.ID || current.EventSequence != row.EventSequence || current.Revision != row.Revision {
		return corestore.ErrSerializationFailure
	}
	locker, ok := w.(leasestore.FencedLeaseLocker)
	if !ok {
		return workItemStorageUnavailable()
	}
	lease, err := locker.LockLease(ctx, string(command.LeaseID))
	if err != nil {
		return err
	}
	credential, err := w.AccessCredential(ctx, scope.credential.ID)
	if err != nil {
		return err
	}
	identity, err := w.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return err
	}
	now := s.config.now().UTC()
	if lease.OrganizationID != scope.organization || lease.IssueID != row.ID || lease.FencingToken != int64(command.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	if credential.OrganizationID != scope.organization || credential.Kind != "service" || credential.Scope != string(apiScopeWorker) || !credential.Valid(scope.credential.Hash, now) || !runnerIdentityMatchesHeldLease(identity, scope.organization, scope.credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return runnerUnauthorized()
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return nativeNotFound()
	}
	return nil
}

func (s *Service) referenceCloudAttachmentTenant(ctx context.Context, scope nativeScope, id string, input attachment.SourceReference) error {
	scope.action = operatortool.ReferenceAttachment
	row, version, err := s.readWorkItemRecord(ctx, scope, input.WorkItemID)
	if err != nil {
		return err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return err
	}
	if input.CommentID == "" {
		if err := requireReporterTriage(scope, issue); err != nil {
			return err
		}
	}
	if _, err := s.readAttachmentRecordTenant(ctx, scope, id); err != nil {
		return err
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		current, err := w.LockWorkItem(ctx, scope.project, row.NativeID)
		if err != nil {
			return err
		}
		if current.Revision != row.Revision || current.EventSequence != row.EventSequence {
			return corestore.ErrRevisionConflict
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		return w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.project, AttachmentID: id, WorkItemID: row.NativeID, CommentID: input.CommentID})
	})
}

func (s *Service) deleteCloudAttachmentTenant(ctx context.Context, scope nativeScope, id string) error {
	scope.action = operatortool.DeleteAttachment
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		if _, err := s.attachmentWriteAuthority(ctx, w, scope, nil); err != nil {
			return err
		}
		owner, ok := w.(attachmentstore.DeletionWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		return owner.DeleteAttachment(ctx, scope.project, id, s.config.now().UTC())
	})
	return projectRecordError(err)
}

func (s *Service) finishCloudAttachmentDeletionTenant(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Audience == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&input); err != nil || conversation.ValidateAttachmentID(input.ID) != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid attachment id"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	err := s.config.TenantRepository.WriteTenant(ctx, tracker.OrganizationID(claims.Audience), corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(attachmentstore.DeletionWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		return owner.FinishAttachmentDeletion(ctx, input.ID, s.config.now().UTC())
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	return c.NoContent(http.StatusNoContent)
}
