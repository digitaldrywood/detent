package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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
func (s *Service) attachmentQuotaLimit(ctx context.Context, r tenantstore.Reader) (int64, error) {
	if s.database.plansService().Plans() == nil {
		return math.MaxInt64, nil
	}
	if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, r, "collaboration", s.config.Clock())); err != nil {
		return 0, err
	}
	entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, r, s.config.Clock()))
	if err != nil {
		return 0, err
	}
	if limit, ok := entitlement.Allowances["collaboration_bytes"]; ok {
		return limit, nil
	}
	return math.MaxInt64, nil
}
func (s *Service) attachmentWriteAuthority(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, evidence *attachment.EvidenceRequest) (corestore.WorkItem, error) {
	if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
		return corestore.WorkItem{}, err
	}
	project, err := w.Project(ctx, scope.Project)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if project.DeletedAt != nil {
		return corestore.WorkItem{}, hubapi.NativeNotFound()
	}
	if scope.Credential.Scope != hubapi.APIScopeWorker {
		return corestore.WorkItem{}, nil
	}
	credential, err := w.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	now := s.config.Clock().UTC()
	if credential.OrganizationID != scope.Organization || credential.Kind != "service" || credential.Scope != string(hubapi.APIScopeWorker) || !credential.Valid(scope.Credential.Hash, now) || identity.OrganizationID != scope.Organization || identity.RunnerID != scope.Credential.Runner.RunnerID || identity.TokenID != credential.ID || identity.RemovedAt != nil || identity.State == "disabled" || !identity.ExpiresAt.After(now) {
		return corestore.WorkItem{}, hubscope.RunnerUnauthorized()
	}
	operation := runnerauth.Collaborate
	if evidence != nil {
		operation = runnerauth.Events
	}
	if !slices.Contains(identity.Operations, operation) {
		return corestore.WorkItem{}, hubscope.RunnerUnauthorized()
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if !allowed {
		return corestore.WorkItem{}, hubapi.NativeNotFound()
	}
	if evidence == nil {
		return corestore.WorkItem{}, nil
	}
	item, err := w.LockWorkItem(ctx, scope.Project, evidence.WorkItemID)
	if err != nil {
		return item, err
	}
	locker, ok := w.(leasestore.FencedLeaseLocker)
	if !ok {
		return item, hubapi.NativeWorkItemStorageUnavailable()
	}
	owner, err := w.RunnerForLease(ctx, string(evidence.LeaseID))
	if err != nil {
		return item, err
	}
	lease, err := locker.LockLease(ctx, string(evidence.LeaseID))
	if err != nil {
		return item, err
	}
	now = s.config.Clock().UTC()
	if !credential.Valid(scope.Credential.Hash, now) || item.Archived || lease.OrganizationID != scope.Organization || lease.IssueID != item.ID || lease.FencingToken != int64(evidence.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || !runnerIdentityMatchesHeldLease(owner, scope.Organization, identity.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) {
		return item, hubapi.NativeStaleExecution("Evidence producer lease is no longer current")
	}
	attempt, err := w.Attempt(ctx, evidence.AttemptID)
	if err != nil {
		return item, err
	}
	if attempt.IssueID != item.ID || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.Status != "running" {
		return item, hubapi.NativeStaleExecution("Evidence producer is not the running attempt")
	}
	return item, nil
}
func (s *Service) prepareCloudAttachmentTenant(c echo.Context, input attachment.UploadRequest) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	input.ID = conversation.NewAttachmentID()
	if input.Validate() != nil || strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil || input.Evidence != nil && input.Evidence.IdempotencyKey != input.RequestID {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid attachment upload"))
	}
	scope := hubscope.NativeRequestScope(c)
	scope.Action = operatortool.UploadAttachment
	operation := "attachment.prepare " + string(scope.Project)
	hash, err := hubapi.CommandHash(attachmentUploadIdentity(input))
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
			err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.Attachment(ctx, scope.Project, metadata.ID)
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				if stored.Metadata.DeletedAt != nil || stored.ObjectDeletedAt != nil {
					return hubapi.NativeConflict(0)
				}
				return nil
			})
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			return c.JSONBlob(http.StatusOK, raw)
		}
		metadata := input.Metadata
		metadata.ProjectID, metadata.Uploader = string(scope.Project), scope.Credential.ID
		if scope.Credential.HostedRole == "reporter" {
			metadata.Uploader = scope.Actor().PrincipalID
		}
		metadata.CreatedAt = s.config.Clock().UTC().Truncate(time.Microsecond)
		raw, err := json.Marshal(metadata)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: input.RequestID, RequestHash: hash}, raw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if _, err := s.attachmentWriteAuthority(ctx, w, scope, input.Evidence); err != nil {
				return err
			}
			if previous, err := w.Receipt(ctx, scope.Credential.ID, operation, input.RequestID); err == nil {
				if previous.RequestHash != hash {
					return hubapi.NativeConflict(0)
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
					return hubapi.AttachmentAccountingUnavailable()
				}
				if err != nil {
					return err
				}
				if metadata.Size > limit-used {
					return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
				}
			}
			if err := s.database.chargeAttachmentAPIMutationsPostgres(ctx, w, "attachment/"+scope.Credential.ID+"/"+operation+"/"+input.RequestID, s.config.Clock(), "mutation"); err != nil {
				return err
			}
			_, err := w.PutReceipt(ctx, prepared)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
		}
		return c.JSONBlob(http.StatusOK, raw)
	}
}

func (s *Service) prepareAttachmentEvidencePublication(ctx context.Context, scope hubscope.NativeScope, input *attachment.EvidenceRequest, metadata attachment.Metadata) (hubrecords.AttachmentEvidencePublication, error) {
	result := hubrecords.AttachmentEvidencePublication{}
	if input == nil {
		return result, nil
	}
	return s.prepareEvidenceComment(ctx, scope, input.WorkItemID, escapeEvidenceCaption(input.Caption)+"\n\n"+metadata.Markdown(string(scope.Organization)), 0)
}

func (s *Service) prepareEvidenceComment(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, body string, offset int64) (hubrecords.AttachmentEvidencePublication, error) {
	result := hubrecords.AttachmentEvidencePublication{}
	row, version, err := s.readWorkItemRecord(ctx, scope, string(item))
	if err != nil {
		return result, err
	}
	return s.prepareEvidenceCommentRecord(ctx, scope, row, version, body, offset)
}

func (s *Service) prepareEvidenceCommentRecord(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, version eventstore.CollaborationVersion, body string, offset int64) (hubrecords.AttachmentEvidencePublication, error) {
	result := hubrecords.AttachmentEvidencePublication{}
	issue, err := s.hydrateCanonicalWorkItemRecord(ctx, row, version)
	if err != nil {
		return result, err
	}
	row.EventSequence += offset
	at := s.config.Clock().UTC().Truncate(time.Microsecond)
	comment := tracker.NativeComment{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.NativeID, ID: hubapi.NewID("cmt"), Revision: 1, Sequence: row.EventSequence + 1, Body: body, Actor: scope.Actor(), CreatedAt: at, UpdatedAt: at}
	result.Plan = hubrecords.WorkItemRecordPlan{Current: row, Issue: issue, Comment: comment, Create: true, EventType: "comment.created"}
	result.Fields, err = s.prepareWorkItemFields(ctx, scope.Organization, result.Plan)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(comment)
	if err != nil {
		return result, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	result.Version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.NativeID, RecordID: comment.ID, Revision: 1}, raw)
	if err != nil {
		return result, err
	}
	data := tracker.CollaborationData{Revision: 1, CommentID: comment.ID}
	event := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: row.NativeID, Type: "comment.created", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: at, Data: data}
	eventRaw, err := json.Marshal(event)
	if err != nil {
		return result, err
	}
	result.Event, err = content.PrepareEvent(ctx, scope.Organization, eventRaw)
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
	result.Bytes = int64(len(comment.Body) + len(raw) + len(dataRaw) + len(actorRaw))
	result.Search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.NativeID, ID: "comment:" + comment.ID, SourceID: comment.ID, Kind: "comment", Scope: "issue", Revision: 1, UpdatedAt: at}, searchstore.Text{Body: comment.Body})
	return result, err
}
func (s *Service) recordCloudAttachmentTenant(c echo.Context, input attachment.UploadRequest) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	if input.Validate() != nil || input.WorkItemID != "" || input.CommentID != "" || len(input.ReferencedBy) != 0 || input.DeletedAt != nil || input.Evidence != nil && (input.RequestID == "" || input.Evidence.IdempotencyKey != input.RequestID) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid attachment metadata"))
	}
	if err := s.checkAttachmentEvidence(c, input.Evidence); err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	scope.Action = operatortool.UploadAttachment
	key := input.RequestID
	if key == "" {
		key = input.ID
	}
	operation := "attachment.record " + string(scope.Project)
	identity := any(input.Metadata)
	if input.Evidence != nil {
		identity = input
	}
	hash, err := hubapi.CommandHash(identity)
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
			prepareHash, err := hubapi.CommandHash(attachmentUploadIdentity(input))
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			prepared, found, err := s.workItemRecordReceipt(ctx, scope, "attachment.prepare "+string(scope.Project), input.RequestID, prepareHash)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			if !found {
				return s.nativeAPIError(c, hubapi.NativePublicInvalid("Attachment preparation receipt is required"))
			}
			raw, err := content.Receipt(ctx, prepared)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			if json.Unmarshal(raw, &metadata) != nil || !reflect.DeepEqual(metadata, input.Metadata) {
				return s.nativeAPIError(c, hubapi.NativeConflict(0))
			}
		} else {
			metadata.ProjectID, metadata.Uploader = string(scope.Project), scope.Credential.ID
			if scope.Credential.HostedRole == "reporter" {
				metadata.Uploader = scope.Actor().PrincipalID
			}
			metadata.CreatedAt = s.config.Clock().UTC().Truncate(time.Microsecond)
		}
		publication, err := s.prepareAttachmentEvidencePublication(ctx, scope, input.Evidence, metadata)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		record := attachmentstore.Record{OrganizationID: scope.Organization, Metadata: metadata, Kind: "attachment"}
		if input.Evidence != nil {
			record.Metadata.WorkItemID = string(input.Evidence.WorkItemID)
			record.Metadata.CommentID = publication.Plan.Comment.ID
			record.EvidenceAttemptID = input.Evidence.AttemptID
			record.EvidenceCaption = input.Evidence.Caption
		}
		raw, err := json.Marshal(record.Metadata)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: key, RequestHash: hash}, raw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			row, err := s.attachmentWriteAuthority(ctx, w, scope, input.Evidence)
			if err != nil {
				return err
			}
			if previous, err := w.Receipt(ctx, scope.Credential.ID, operation, key); err == nil {
				if previous.RequestHash != hash {
					return hubapi.NativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if input.Evidence != nil && (row.ID != publication.Plan.Current.ID || row.Revision != publication.Plan.Current.Revision || row.EventSequence != publication.Plan.Current.EventSequence) {
				return corestore.ErrSerializationFailure
			}
			{
				limit, err := s.attachmentQuotaLimit(ctx, w)
				if err != nil {
					return err
				}
				usage, err := w.AdjustCollaborationBytes(ctx, metadata.Size+publication.Bytes, limit)
				if errors.Is(err, corestore.ErrNotFound) {
					return hubapi.AttachmentAccountingUnavailable()
				}
				if err != nil {
					return err
				}
				if !usage.Allowed {
					return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
				}
			}
			if err := s.database.chargeAttachmentAPIMutationsPostgres(ctx, w, "attachment/"+scope.Credential.ID+"/"+operation+"/"+key, s.config.Clock(), "record", "mutation"); err != nil {
				return err
			}
			if input.Evidence != nil {
				if err := hubrecords.WriteWorkItemRecord(ctx, w, publication.Plan, publication.Fields); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(ctx, publication.Version); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, publication.Event); err != nil {
					return err
				}
				if err := w.PutSearch(ctx, publication.Search, 0); err != nil {
					return err
				}
			}
			if err := w.CreateAttachment(ctx, record); err != nil {
				return err
			}
			if input.Evidence != nil {
				if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.Project, AttachmentID: metadata.ID, WorkItemID: input.Evidence.WorkItemID, CommentID: publication.Plan.Comment.ID}); err != nil {
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
			return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
		}
		return c.JSONBlob(http.StatusCreated, raw)
	}
}

func (s *Service) readAttachmentRecordTenant(ctx context.Context, scope hubscope.NativeScope, id string) (attachment.Metadata, error) {
	var metadata attachment.Metadata
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { return s.readAttachmentRecordWith(ctx, r, scope, id, &metadata) })
	return metadata, err
}

func (s *Service) readAttachmentRecordWith(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id string, metadata *attachment.Metadata) error {
	if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
		return err
	}
	record, err := r.Attachment(ctx, scope.Project, id)
	if err != nil {
		return hubapi.ProjectRecordError(err)
	}
	if record.Metadata.DeletedAt != nil || record.ObjectDeletedAt != nil || record.PendingContent != nil {
		return hubapi.NativeNotFound()
	}
	key, err := attachment.Key(string(scope.Organization), id)
	if err != nil {
		return err
	}
	if record.CustomerOwned || record.StorageBindingID != "" || record.ObjectKey != "" && record.ObjectKey != key {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	references, err := r.AttachmentReferences(ctx, scope.Project, id, 201)
	if err != nil {
		return err
	}
	if len(references) > 200 {
		return hubapi.NativePublicInvalid("Attachment references exceed the read bound")
	}
	if len(references) > 0 {
		record.Metadata.WorkItemID, record.Metadata.CommentID = string(references[0].WorkItemID), references[0].CommentID
	}
	if len(references) == 0 && record.Metadata.WorkItemID != "" {
		_, sourceErr := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(record.Metadata.WorkItemID))
		if sourceErr == nil && record.Metadata.CommentID != "" {
			_, sourceErr = r.Comment(ctx, scope.Project, tracker.NativeWorkItemID(record.Metadata.WorkItemID), record.Metadata.CommentID)
		}
		if errors.Is(sourceErr, corestore.ErrNotFound) {
			return hubapi.NativeNotFound()
		}
		if sourceErr != nil {
			return sourceErr
		}
	}
	*metadata = record.Metadata
	visible := scope.Credential.HostedRole != "reporter" || metadata.Uploader == scope.Credential.ID || metadata.Uploader == scope.Actor().PrincipalID
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
			visible = err == nil && member.MembershipID == uploader.MembershipID && member.PrincipalID == scope.Actor().PrincipalID
		}
	}
	if scope.Credential.HostedRole == "reporter" && metadata.WorkItemID != "" {
		item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(metadata.WorkItemID))
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err != nil || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			metadata.WorkItemID, metadata.CommentID = "", ""
		}
	}
	for _, reference := range references {
		if scope.Credential.HostedRole == "reporter" {
			item, err := r.WorkItem(ctx, scope.Project, reference.WorkItemID)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
				continue
			}
		}
		visible = true
		metadata.ReferencedBy = append(metadata.ReferencedBy, attachment.SourceReference{WorkItemID: string(reference.WorkItemID), CommentID: reference.CommentID})
	}
	if !visible {
		return hubapi.NativeNotFound()
	}
	return nil
}

func (s *Service) prepareWorkItemAttachmentReferences(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemAttachmentReferences, error) {
	result := hubrecords.WorkItemAttachmentReferences{}
	if plan.Existing {
		return result, nil
	}
	body := plan.Comment.Body
	if plan.Comment.ID == "" {
		body = plan.Issue.Title + "\n" + plan.Issue.Body
	}
	for _, source := range []struct {
		body string
		ids  *[]string
	}{{plan.PreviousContent, &result.Previous}, {body, &result.Current}} {
		references, err := cloudContentAttachmentReferences(source.body, scope)
		if err != nil {
			return result, err
		}
		for _, reference := range references {
			*source.ids = append(*source.ids, reference.ID)
		}
		slices.Sort(*source.ids)
		*source.ids = slices.Compact(*source.ids)
		if len(*source.ids) > 200 {
			return result, hubapi.NativePublicInvalid("Work item attachment references exceed the write bound")
		}
	}
	if plan.Comment.ID != "" && plan.Comment.Deleted {
		result.Current = slices.Clone(result.Previous)
		return result, nil
	}
	readable := map[string]attachment.Metadata{}
	read := func(id string) (attachment.Metadata, bool, error) {
		if metadata, ok := readable[id]; ok {
			return metadata, true, nil
		}
		metadata, err := s.readAttachmentRecordTenant(ctx, scope, id)
		var failure *hubapi.NativeError
		if errors.Is(err, corestore.ErrNotFound) || errors.As(err, &failure) && failure.Status == http.StatusNotFound {
			return attachment.Metadata{}, false, nil
		}
		if err != nil {
			return attachment.Metadata{}, false, err
		}
		readable[id] = metadata
		return metadata, true, nil
	}
	if plan.Project.Settings.RequireVisualAttachments && !scope.ImportedContent && body != plan.PreviousContent {
		fields := []cloudContentField{{"body", plan.Comment.Body}}
		if plan.Comment.ID == "" {
			fields = []cloudContentField{{"title", plan.Issue.Title}, {"body", plan.Issue.Body}}
		}
		resolve := func(id string) (attachment.Metadata, error) {
			metadata, _, err := read(id)
			return metadata, err
		}
		for _, field := range fields {
			err := attachment.ValidateVisualReferences(field.value, string(scope.Organization), string(scope.Project), resolve)
			var refusal *attachment.VisualReferenceError
			if errors.As(err, &refusal) {
				return result, hubapi.NativePublicInvalid(fmt.Sprintf("%s block %d: %s", field.name, refusal.Block, attachment.VisualReferenceMessage))
			}
			if err != nil {
				return result, err
			}
		}
	}
	valid := result.Current[:0]
	for _, id := range result.Current {
		if slices.Contains(result.Previous, id) {
			valid = append(valid, id)
			continue
		}
		_, found, err := read(id)
		if err != nil {
			return result, err
		}
		if found {
			valid = append(valid, id)
		}
	}
	result.Current = valid
	return result, nil
}

func (s *Service) authorizeAttachmentCommentLease(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, row corestore.WorkItem, command tracker.Mutation) error {
	current, err := w.LockWorkItem(ctx, scope.Project, row.NativeID)
	if err != nil {
		return err
	}
	if current.ID != row.ID || current.EventSequence != row.EventSequence || current.Revision != row.Revision {
		return corestore.ErrSerializationFailure
	}
	locker, ok := w.(leasestore.FencedLeaseLocker)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	identity, err := w.RunnerForLease(ctx, string(command.LeaseID))
	if err != nil {
		return err
	}
	lease, err := locker.LockLease(ctx, string(command.LeaseID))
	if err != nil {
		return err
	}
	credential, err := w.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		return err
	}
	now := s.config.Clock().UTC()
	if lease.OrganizationID != scope.Organization || lease.IssueID != row.ID || lease.FencingToken != int64(command.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	if credential.OrganizationID != scope.Organization || credential.Kind != "service" || credential.Scope != string(hubapi.APIScopeWorker) || !credential.Valid(scope.Credential.Hash, now) || !runnerIdentityMatchesHeldLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return hubscope.RunnerUnauthorized()
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFound()
	}
	return nil
}

func (s *Service) referenceCloudAttachmentTenant(ctx context.Context, scope hubscope.NativeScope, id string, input attachment.SourceReference) error {
	scope.Action = operatortool.ReferenceAttachment
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
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		current, err := w.LockWorkItem(ctx, scope.Project, row.NativeID)
		if err != nil {
			return err
		}
		if current.Revision != row.Revision || current.EventSequence != row.EventSequence {
			return corestore.ErrRevisionConflict
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		return w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.Project, AttachmentID: id, WorkItemID: row.NativeID, CommentID: input.CommentID})
	})
}

func (s *Service) deleteCloudAttachmentTenant(ctx context.Context, scope hubscope.NativeScope, id string) error {
	scope.Action = operatortool.DeleteAttachment
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if _, err := s.attachmentWriteAuthority(ctx, w, scope, nil); err != nil {
			return err
		}
		owner, ok := w.(attachmentstore.DeletionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return owner.DeleteAttachment(ctx, scope.Project, id, s.config.Clock().UTC())
	})
	return hubapi.ProjectRecordError(err)
}

func (s *Service) finishCloudAttachmentDeletionTenant(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Audience == "" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&input); err != nil || conversation.ValidateAttachmentID(input.ID) != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid attachment id"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	err := s.config.TenantRepository.WriteTenant(ctx, tracker.OrganizationID(claims.Audience), corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(attachmentstore.DeletionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return owner.FinishAttachmentDeletion(ctx, input.ID, s.config.Clock().UTC())
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) expiredCloudAttachmentsTenant(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Audience == "" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	organization := tracker.OrganizationID(claims.Audience)
	now := s.config.Clock().UTC()
	cutoff := now.Add(-attachment.OrphanTTL)
	var candidates []attachmentstore.Record
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(attachmentstore.ExpiryReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		candidates, err = owner.AttachmentDeletionCandidates(ctx, cutoff, 100)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	items := []attachment.Metadata{}
	for _, candidate := range candidates {
		metadata := candidate.Metadata
		if metadata.DeletedAt == nil {
			expired := false
			err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
				project := tracker.ProjectID(metadata.ProjectID)
				owner, ok := w.(attachmentstore.ExpiryWriter)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				var err error
				expired, err = owner.ExpireAttachment(ctx, project, metadata.ID, cutoff, now)
				return err
			})
			if err != nil {
				return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
			}
			if !expired {
				continue
			}
			metadata.DeletedAt = &now
		}
		items = append(items, metadata)
	}
	return c.JSON(http.StatusOK, items)
}
