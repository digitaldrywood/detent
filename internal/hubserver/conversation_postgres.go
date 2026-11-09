package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func conversationDispatchUnavailable() error {
	return &nativeError{Code: "unavailable", Message: "Conversation dispatch authority is unavailable", status: 503}
}

func (s *Service) conversationRecordsSelected() bool {
	return s.config.TenantRepository != nil
}

func (s *Service) authorizeConversationScopePG(ctx context.Context, scope nativeScope, write bool) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.authorizeAccessScope(ctx, reader, scope, write)
	})
}

func (s *Service) requireConversationFeaturePG(ctx context.Context, organization tracker.OrganizationID, feature string, now time.Time) error {
	return s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.database.requireHostedFeatureRecord(ctx, reader, feature, now)
	})
}

func (s *Service) authorizeConversationWorkerPG(ctx context.Context, reader tenantstore.Reader, scope nativeScope, record conversationRecord, identity conversationWorkerIdentity, runID string, allowReleased bool) (leasestore.Lease, error) {
	if err := requireConversationRecordOwner(record, identity, allowReleased); err != nil {
		return leasestore.Lease{}, err
	}
	if runID == "" {
		runID = record.Execution.Owner.RunID
	}
	lease, attempt, err := s.authorizeConversationWorkerLeasePG(ctx, reader, scope, record.WorkItemID, identity, runID, allowReleased, record.Execution.Status == conversation.ExecutionStarting)
	if err != nil {
		return leasestore.Lease{}, err
	}
	if record.Execution.Owner.RunID != "" && attempt.RunID != record.Execution.Owner.RunID {
		return leasestore.Lease{}, conversationStale("The attempt is not the current work item owner")
	}
	return lease, nil
}

func (s *Service) authorizeConversationWorkerLeasePG(ctx context.Context, reader tenantstore.Reader, scope nativeScope, itemID string, identity conversationWorkerIdentity, runID string, allowReleased, allowUnstarted bool) (leasestore.Lease, leasestore.Attempt, error) {
	if scope.credential.Scope != apiScopeWorker || scope.credential.Runner.RunnerID == "" || !scope.credential.Runner.Valid() || scope.credential.Runner.OrganizationID != scope.organization {
		return leasestore.Lease{}, leasestore.Attempt{}, conversationWorkerAuthorityUnavailable()
	}
	if scope.credential.Runner.Scope == "projects" && !slices.Contains(scope.credential.Runner.ProjectIDs, scope.project) {
		return leasestore.Lease{}, leasestore.Attempt{}, nativeNotFound()
	}
	credential, err := reader.AccessCredential(ctx, scope.credential.ID)
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	if credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, s.config.now()) {
		return leasestore.Lease{}, leasestore.Attempt{}, accessstore.ErrDenied
	}
	if s.config.ConversationWorkerAuthority == nil {
		return leasestore.Lease{}, leasestore.Attempt{}, conversationWorkerAuthorityUnavailable()
	}
	worker := ConversationWorkerIdentity{Credential: credential, Runner: scope.credential.Runner, Organization: scope.organization, Project: scope.project}
	if err := s.config.ConversationWorkerAuthority.VerifyConversationWorker(ctx, reader, worker); err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	lease, err := reader.Lease(ctx, string(identity.LeaseID))
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	item, err := reader.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(itemID))
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	if lease.OrganizationID != scope.organization || lease.MachineID != string(scope.credential.Runner.MachineID) || lease.IssueID != item.ID || lease.FencingToken != int64(identity.FencingToken) {
		return leasestore.Lease{}, leasestore.Attempt{}, conversationStale("The lease does not belong to this worker and work item")
	}
	attempt, err := reader.Attempt(ctx, identity.AttemptID)
	if errors.Is(err, corestore.ErrNotFound) && allowUnstarted && !allowReleased && validNativeID(runID, "run") && validNativeID(identity.AttemptID, "attempt") {
		latest, latestErr := reader.LatestAttempt(ctx, item.ID)
		if latestErr != nil && !errors.Is(latestErr, corestore.ErrNotFound) {
			return leasestore.Lease{}, leasestore.Attempt{}, latestErr
		}
		if latestErr == nil && latest.LeaseID == lease.LeaseID {
			return leasestore.Lease{}, leasestore.Attempt{}, conversationStale("The lease is already bound to another attempt")
		}
		now := s.config.now()
		if lease.RenewedAt.After(now) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
			return leasestore.Lease{}, leasestore.Attempt{}, conversationStale("The lease is no longer current")
		}
		return lease, leasestore.Attempt{OrganizationID: scope.organization, ID: identity.AttemptID, IssueID: item.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: runID, Status: "running"}, nil
	}
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	if attempt.OrganizationID != scope.organization || attempt.IssueID != item.ID || attempt.LeaseID != string(identity.LeaseID) || attempt.FencingToken != int64(identity.FencingToken) || (runID != "" && attempt.RunID != runID) {
		return leasestore.Lease{}, leasestore.Attempt{}, conversationStale("The attempt is not the current work item owner")
	}
	now := s.config.now()
	if !allowReleased && (lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || attempt.Status != "running") {
		return leasestore.Lease{}, leasestore.Attempt{}, conversationStale("The lease is no longer current")
	}
	if lease.RenewedAt.After(now) {
		return leasestore.Lease{}, leasestore.Attempt{}, conversationStale("The lease renewal is in the future")
	}
	if allowReleased && attempt.Status != "running" && attempt.Status != "succeeded" && attempt.Status != "failed" && attempt.Status != "cancelled" && attempt.Status != "interrupted" {
		return leasestore.Lease{}, leasestore.Attempt{}, conversationStale("The attempt is no longer current")
	}
	return lease, attempt, nil
}

func authorizeConversationLinkedItemPG(ctx context.Context, reader tenantstore.Reader, scope nativeScope, record conversationRecord) error {
	if scope.credential.HostedRole != "reporter" || record.WorkItemID == "" {
		return nil
	}
	item, err := reader.WorkItem(ctx, record.ProjectID, tracker.NativeWorkItemID(record.WorkItemID))
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return nativeNotFound()
		}
		return err
	}
	principal := ""
	if item.ActorPrincipalID != nil {
		principal = *item.ActorPrincipalID
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: principal}) {
		return nativeNotFound()
	}
	return nil
}

type conversationLinkMutation struct {
	service     *Service
	scope       nativeScope
	original    conversationRecord
	request     conversationLinkRequest
	now         time.Time
	publication *conversationPublicationPlan
	issue       tracker.NativeIssue
}

func (m *conversationLinkMutation) prepare(ctx context.Context, workItem workItemRecordPlan) error {
	if workItem.existing || !workItem.create || workItem.issue.WorkItemID == "" {
		return workItemStorageUnavailable()
	}
	record := m.original
	record.WorkItemID = string(workItem.issue.WorkItemID)
	linkedAt := workItem.issue.CreatedAt
	record.LinkedAt = &linkedAt
	record.Visibility = conversation.VisibilityShared
	record.Execution = conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: record.Execution.Owner.ThreadID}, UpdatedAt: m.now}
	record.WorkItem = &conversationWorkItem{ID: string(workItem.issue.WorkItemID), Identifier: fmt.Sprintf("%s#%d", workItem.project.Core.Name, workItem.issue.Number), Title: workItem.issue.Title, Lane: workItem.issue.State}
	publication := &conversationPublicationPlan{
		service: m.service.conversations, record: record, original: m.original, now: m.now,
		prepared: make(preparedConversationContents), reads: &conversationArchiveReads{content: map[string][]byte{}},
	}
	if err := publication.prepareRecord(ctx, record); err != nil {
		return err
	}
	raw, err := recordPayload(record)
	if err != nil {
		return err
	}
	archive, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	ref, ok := publication.prepared[string(m.scope.organization)+"/"+artifact.Digest(archive)]
	if !ok {
		return errors.New("linked conversation record was not prepared before the transaction")
	}
	record.ContentID = contentArchiveID(string(m.scope.organization), ref)
	publication.record.ContentID = record.ContentID
	m.publication, m.issue = publication, workItem.issue
	return nil
}

func (m *conversationLinkMutation) write(ctx context.Context, writer tenantstore.Writer, workItem workItemRecordPlan) error {
	if m.publication == nil || m.issue.WorkItemID != workItem.issue.WorkItemID {
		return errors.New("conversation link was not prepared before the transaction")
	}
	if err := m.service.authorizeAccessScope(ctx, writer, m.scope, true); err != nil {
		return err
	}
	current, err := writer.LockConversation(ctx, m.scope.project, m.original.ID)
	if err != nil {
		return err
	}
	if current.Revision != m.original.Revision {
		return conversationStale("The conversation changed; reload and retry")
	}
	if current.SubjectWorkItemID != "" {
		return nativePublicInvalid("Issue questions cannot be linked or shared")
	}
	if current.WorkItemID != "" {
		return conversationAlreadyLinked(current.ID)
	}
	linkedRecord := conversationRecordFromStore(current)
	linkedRecord.WorkItemID = m.publication.record.WorkItemID
	if err := authorizeConversationLinkedItemPG(ctx, writer, m.scope, linkedRecord); err != nil {
		return err
	}
	if !m.request.ShareHistory {
		return conversationShareRequired()
	}
	if err := registerPreparedConversationObjects(ctx, writer, m.scope.organization, m.publication.prepared, m.now); err != nil {
		return err
	}
	record := m.publication.record
	previousVisibility := current.Visibility
	stored := conversationRecordToStore(record)
	stored.Revision = current.Revision
	stored.EventSequence = current.EventSequence
	stored.MessageCount = current.MessageCount
	updated, err := writer.UpdateConversation(ctx, stored, current.Revision)
	if err != nil {
		return err
	}
	updatedRecord := conversationRecordFromStore(updated)
	updatedRecord.WorkItem = record.WorkItem
	if previousVisibility != conversation.VisibilityShared {
		if err := writer.RecordAudience(ctx, record.ID, m.scope.credential.ID, previousVisibility, conversation.VisibilityShared, m.now); err != nil {
			return err
		}
	}
	if err := appendPGConversationEvent(ctx, writer, m.scope.organization, record.ID, conversation.EventConversationUpdated, projectConversation(updatedRecord), m.now); err != nil {
		return err
	}
	if err := appendPGConversationEvent(ctx, writer, m.scope.organization, record.ID, conversation.EventExecutionUpdated, projectExecution(record.Execution), m.now); err != nil {
		return err
	}
	if err := m.queueSavedMessages(ctx, writer, record.ID); err != nil {
		return err
	}
	result, err := marshalNative(map[string]any{"issue": conversationIssueResult(updatedRecord)})
	if err != nil {
		return err
	}
	message := conversationMessageRecord{
		ID: conversation.NewMessageID(), ConversationID: record.ID, Role: conversation.RoleSystem,
		Kind: conversation.MessageStatus, Text: fmt.Sprintf("Linked to issue #%d: %s", m.issue.Number, m.issue.Title),
		Data: json.RawMessage(result), Delivery: conversation.DeliverySaved, Actor: conversationActorFor(m.scope),
		CommandKey: m.request.Key, CreatedAt: m.now, UpdatedAt: m.now, TextBytes: int64(len(fmt.Sprintf("Linked to issue #%d: %s", m.issue.Number, m.issue.Title))),
	}
	created, err := writer.CreateMessage(ctx, conversationMessageToStore(m.scope.organization, message))
	if err != nil {
		return err
	}
	message = conversationMessageFromStore(created)
	return appendPGConversationEvent(ctx, writer, m.scope.organization, record.ID, conversation.EventMessageAccepted, projectMessage(message), m.now)
}

func (m *conversationLinkMutation) queueSavedMessages(ctx context.Context, writer tenantstore.Writer, conversationID string) error {
	before := int64(0)
	for {
		page, err := writer.Messages(ctx, conversationID, before, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		for _, message := range page.Items {
			if message.Role != conversation.RoleUser || message.Kind != conversation.MessageText || message.Delivery != conversation.DeliverySaved {
				continue
			}
			message.Delivery = conversation.DeliveryQueued
			message.UpdatedAt = m.now
			updated, err := writer.UpdateMessage(ctx, message, message.Revision)
			if err != nil {
				return err
			}
			if err := appendPGConversationEvent(ctx, writer, m.scope.organization, conversationID, conversation.EventMessageUpdated, projectMessage(conversationMessageFromStore(updated)), m.now); err != nil {
				return err
			}
		}
		if page.Next == 0 {
			return nil
		}
		before = page.Next
	}
}

func (s *Service) commandLinkConversationPG(ctx context.Context, scope nativeScope, id string, request conversationLinkRequest) (json.RawMessage, error) {
	preview, err := s.readPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	write := preview.OwnerPrincipalID != scope.credential.ID
	if err := s.authorizeConversationScopePG(ctx, scope, write); err != nil {
		return nil, err
	}
	operation := nativeOperation(scope, "POST", "/conversations/"+id+"/link")
	if err := s.requireConversationFeaturePG(ctx, scope.organization, "collaboration", s.config.now()); err != nil {
		return nil, err
	}
	if !request.ShareHistory {
		return nil, conversationShareRequired()
	}
	if preview.SubjectWorkItemID != "" {
		return nil, nativePublicInvalid("Issue questions cannot be linked or shared")
	}
	projectRecord, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	project, err := s.projectRecordNative(ctx, projectRecord)
	if err != nil {
		return nil, err
	}
	next, err := resolveConversationNext(project, request)
	if err != nil {
		return nil, err
	}
	release, err := s.conversations.coordinator.Hold(id)
	if err != nil {
		return nil, err
	}
	defer release()
	preview, err = s.readPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConversationScopePG(ctx, scope, preview.OwnerPrincipalID != scope.credential.ID); err != nil {
		return nil, err
	}
	if preview.SubjectWorkItemID != "" {
		return nil, nativePublicInvalid("Issue questions cannot be linked or shared")
	}
	now := s.config.now()
	link := &conversationLinkMutation{service: s, scope: scope, original: preview, request: request, now: now}
	issueRequest := conversationLinkedIssueRequest(preview, request, next)
	hash, err := nativeCommandHash(issueRequest)
	if err != nil {
		return nil, err
	}
	if _, found, err := s.workItemRecordReceipt(ctx, scope, operation, issueRequest.IdempotencyKey, hash); err != nil {
		return nil, err
	} else if found {
		if preview.WorkItemID == "" {
			return nil, errors.New("conversation link receipt exists without a linked work item")
		}
		return s.conversationLinkResultPG(ctx, scope, project, id, next)
	}
	if preview.WorkItemID != "" {
		return nil, conversationAlreadyLinked(preview.ID)
	}
	if _, err := s.executeWorkItemRecordMutationWithConversationLink(ctx, scope, nativeCommandOptions{OperationID: operation, Feature: "collaboration"}, issueRequest.Mutation, issueRequest, "", link); err != nil {
		return nil, err
	}
	return s.conversationLinkResultPG(ctx, scope, project, id, next)
}

func (s *Service) conversationLinkResultPG(ctx context.Context, scope nativeScope, project tracker.NativeProject, id string, next conversationLinkNextApplied) (json.RawMessage, error) {
	linked, err := s.readPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if linked.WorkItemID == "" {
		return nil, errors.New("conversation link committed without a work item")
	}
	issue, err := s.readWorkItem(ctx, scope, linked.WorkItemID)
	if err != nil {
		return nil, err
	}
	s.conversations.committed(linked)
	resource := projectConversation(linked)
	result := conversationLinkResult{
		Conversation: resource,
		Issue:        conversationLinkedIssue{ID: string(issue.WorkItemID), Identifier: fmt.Sprintf("%s#%d", project.Name, issue.Number), NativeIssue: issue},
		Scheduling:   conversationScheduling{Lane: issue.State},
		Next:         next,
	}
	return json.Marshal(result)
}

func (s *Service) readPGConversation(ctx context.Context, scope nativeScope, id string) (conversationRecord, error) {
	var result conversationRecord
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		stored, err := reader.Conversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		result = conversationRecordFromStore(stored)
		if result.ReadAudience != "" {
			audience, err := conversationReadAudienceRecord(ctx, reader, scope.organization, scope.credential)
			if err != nil || audience != result.ReadAudience {
				return nativeNotFound()
			}
		}
		if err := authorizeConversationLinkedItemPG(ctx, reader, scope, result); err != nil {
			return err
		}
		if result.WorkItemID != "" {
			item, err := reader.WorkItem(ctx, result.ProjectID, tracker.NativeWorkItemID(result.WorkItemID))
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				return err
			}
			project, err := reader.Project(ctx, result.ProjectID)
			if err != nil {
				return err
			}
			result.WorkItem = &conversationWorkItem{ID: string(item.NativeID), Identifier: fmt.Sprintf("%s#%d", project.Name, item.Number), Title: item.Title}
			if item.WorkflowStateName != nil {
				result.WorkItem.Lane = *item.WorkflowStateName
			}
		}
		return nil
	})
	if err != nil {
		return conversationRecord{}, translateConversationError(err)
	}
	if result.OrganizationID != scope.organization || result.ProjectID != scope.project {
		return conversationRecord{}, nativeNotFound()
	}
	if result.Visibility == conversation.VisibilityPrivate && result.OwnerPrincipalID != scope.credential.ID {
		return conversationRecord{}, nativeNotFound()
	}
	return result, nil
}

func conversationContentUnavailable() error {
	return conversationUnsupported("Conversation content references require the attachments migration")
}

func (s *Service) readConversationObject(ctx context.Context, organization string, ref attachment.ArchiveReference, target any) error {
	if s.conversations.store.contents == nil {
		return conversationContentUnavailable()
	}
	content, err := s.conversations.store.contents.ReadContent(ctx, organization, ref)
	if err != nil {
		return fmt.Errorf("read conversation content object: %w", err)
	}
	if err := json.Unmarshal(content, target); err != nil {
		return fmt.Errorf("decode conversation content object: %w", err)
	}
	return nil
}

func conversationRecordFromStore(record conversationstore.Conversation) conversationRecord {
	return conversationRecord{
		ID: record.ID, CreationKey: record.CreationKey, CreationHash: record.CreationHash, ReadAudience: record.ReadAudience, OrganizationID: record.OrganizationID,
		ProjectID: record.ProjectID, OwnerPrincipalID: record.OwnerPrincipalID, OwnerSubject: record.OwnerSubject,
		Origin: record.Origin, Title: record.Title, Visibility: record.Visibility, Status: record.Status,
		Archived: record.Archived, SubjectWorkItemID: record.SubjectWorkItemID, WorkItemID: record.WorkItemID,
		LinkedAt: record.LinkedAt, Revision: record.Revision, ProviderThreadID: record.ProviderThreadID,
		ProviderThreadRunnerID: record.ProviderThreadRunnerID, ProviderThreadOrigin: record.ProviderThreadOrigin,
		Execution: record.Execution, ExecutionContinuation: record.ExecutionContinuation, EventSeq: record.EventSequence, CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt, LastMessageAt: record.LastMessageAt, SettledAt: record.SettledAt,
		Preferences: record.Preferences, MessageCount: record.MessageCount, ContentID: record.ContentReference,
	}
}

func conversationMessageFromStore(record conversationstore.Message) conversationMessageRecord {
	return conversationMessageRecord{
		Revision: record.Revision, ID: record.ID, ContentID: record.ContentReference,
		TextBytes: record.TextBytes, StoredTextBytes: conversationStoredInt64(record.StoredTextBytes),
		ConversationID: record.ConversationID, Seq: record.Sequence, Role: record.Role,
		Kind: record.Kind, Text: record.Text, Data: record.Data, Delivery: record.Delivery,
		AttemptID: record.AttemptID, ThreadID: record.ThreadID, TurnID: record.TurnID,
		ProviderItemID: record.ProviderItemID, Actor: record.Actor, CommandKey: record.CommandKey,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

func conversationStoredInt64(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: true}
}

func (s *Service) readPGConversationPage(ctx context.Context, scope nativeScope, id string, before int64, limit int) (conversationPageRead, error) {
	var page conversationPageRead
	contentReferences := map[string]attachment.ArchiveReference{}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		stored, err := reader.Conversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		page.record = conversationRecordFromStore(stored)
		if page.record.ReadAudience != "" {
			audience, err := conversationReadAudienceRecord(ctx, reader, scope.organization, scope.credential)
			if err != nil || audience != page.record.ReadAudience {
				return nativeNotFound()
			}
		}
		if page.record.Visibility == conversation.VisibilityPrivate && page.record.OwnerPrincipalID != scope.credential.ID {
			return nativeNotFound()
		}
		if err := authorizeConversationLinkedItemPG(ctx, reader, scope, page.record); err != nil {
			return err
		}
		messages, err := reader.Messages(ctx, id, before, limit)
		if err != nil {
			return err
		}
		owner, ok := reader.(conversationstore.AttachmentReader)
		if !ok {
			return conversationUnsupported("Conversation attachment owner is unavailable")
		}
		ids := make([]string, 0, len(messages.Items))
		for _, item := range messages.Items {
			ids = append(ids, item.ID)
		}
		attachments, err := owner.MessageAttachments(ctx, scope.project, id, ids)
		if err != nil {
			return err
		}
		for _, item := range messages.Items {
			if item.ContentReference != "" {
				object, err := reader.ConversationContent(ctx, item.ContentReference)
				if err != nil {
					return err
				}
				contentReferences[item.ContentReference] = object.Reference
			}
			message := conversationMessageFromStore(item)
			for _, a := range attachments[item.ID] {
				message.Attachments = append(message.Attachments, conversationAttachmentFromStore(a))
			}

			references, err := reader.MessageReferences(ctx, id, item.ID)
			if err != nil {
				return err
			}
			for _, reference := range references {
				urlPath := "/chat/c/" + url.PathEscape(reference.TargetID)
				if reference.Kind == conversation.ReferenceIssue {
					organization, err := reader.AccessOrganization(ctx)
					if err != nil {
						return err
					}
					project, err := reader.Project(ctx, page.record.ProjectID)
					if err != nil {
						return err
					}
					item, err := reader.WorkItem(ctx, page.record.ProjectID, tracker.NativeWorkItemID(reference.TargetID))
					if err != nil {
						return err
					}
					urlPath = fmt.Sprintf("/%s/%s/issues/%d", url.PathEscape(organization.Slug), url.PathEscape(project.Slug), item.Number)
				}
				message.References = append(message.References, conversationMessageReference{Kind: reference.Kind, ID: reference.TargetID, Label: reference.Label, URL: urlPath})
			}
			page.messages = append(page.messages, message)
		}
		return nil
	})
	if err != nil {
		return conversationPageRead{}, translateConversationError(err)
	}
	for index := range page.messages {
		message := &page.messages[index]
		if message.ContentID == "" {
			continue
		}
		ref, ok := contentReferences[message.ContentID]
		if !ok {
			return conversationPageRead{}, conversationContentUnavailable()
		}
		var payload conversationContent
		if err := s.readConversationObject(ctx, string(scope.organization), ref, &payload); err != nil {
			return conversationPageRead{}, err
		}
		message.Text, message.Data = payload.Text, payload.Data
	}
	return page, nil
}

func (s *Service) replayPGConversationEvents(ctx context.Context, scope nativeScope, stream *conversationStream, id string, cursor, through int64) (int64, error) {
	for cursor < through {
		var page conversationstore.EventPage
		refs := map[string]attachment.ArchiveReference{}
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
				return err
			}
			record, err := reader.Conversation(ctx, scope.project, id)
			if err != nil {
				return err
			}
			current := conversationRecordFromStore(record)
			if current.ReadAudience != "" {
				audience, err := conversationReadAudienceRecord(ctx, reader, scope.organization, scope.credential)
				if err != nil || audience != current.ReadAudience {
					return nativeNotFound()
				}
			}
			if current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.credential.ID {
				return nativeNotFound()
			}
			if err := authorizeConversationLinkedItemPG(ctx, reader, scope, current); err != nil {
				return err
			}
			page, err = reader.ConversationEvents(ctx, id, cursor, conversationEventPage)
			if err != nil {
				return err
			}
			for _, stored := range page.Items {
				if stored.Seq > through {
					break
				}
				if stored.ContentReference == "" {
					continue
				}
				object, err := reader.ConversationContent(ctx, stored.ContentReference)
				if err != nil {
					return err
				}
				refs[stored.ContentReference] = object.Reference
			}
			return nil
		})
		if err != nil {
			return cursor, translateConversationError(err)
		}
		if len(page.Items) == 0 {
			return cursor, nil
		}
		accepted := map[int]conversationMessageResource{}
		ids := make([]string, 0, len(page.Items))
		for index := range page.Items {
			stored := &page.Items[index]
			if stored.Seq > through {
				break
			}
			if stored.ContentReference == "" {
				continue
			}
			var payload conversationContent
			if err := s.readConversationObject(ctx, string(scope.organization), refs[stored.ContentReference], &payload); err != nil {
				return cursor, err
			}
			stored.Data = payload.Data
			if stored.Type == conversation.EventMessageAccepted {
				var message conversationMessageResource
				if err := json.Unmarshal(payload.Data, &message); err != nil {
					return cursor, err
				}
				accepted[index] = message
				ids = append(ids, message.ID)
			}
		}
		if len(ids) > 0 {
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
					return err
				}
				record, err := reader.Conversation(ctx, scope.project, id)
				if err != nil {
					return err
				}
				current := conversationRecordFromStore(record)
				if current.ReadAudience != "" {
					audience, err := conversationReadAudienceRecord(ctx, reader, scope.organization, scope.credential)
					if err != nil || audience != current.ReadAudience {
						return nativeNotFound()
					}
				}
				if current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.credential.ID {
					return nativeNotFound()
				}
				if err := authorizeConversationLinkedItemPG(ctx, reader, scope, current); err != nil {
					return err
				}
				owner, ok := reader.(conversationstore.AttachmentReader)
				if !ok {
					return conversationUnsupported("Conversation attachment owner is unavailable")
				}
				sequences, err := owner.MessageSequences(ctx, id, ids)
				if err != nil {
					return err
				}
				for index, message := range accepted {
					sequence, ok := sequences[message.ID]
					if !ok {
						return corestore.ErrNotFound
					}
					message.Seq = sequence
					data, err := json.Marshal(message)
					if err != nil {
						return err
					}
					page.Items[index].Data = data
				}
				return nil
			}); err != nil {
				return cursor, translateConversationError(err)
			}
		}
		for _, stored := range page.Items {
			if stored.Seq > through {
				return cursor, nil
			}
			if err := stream.event(stored.Event); err != nil {
				return cursor, err
			}
			cursor = stored.Seq
		}
		if len(page.Items) < conversationEventPage {
			return cursor, nil
		}
	}
	return cursor, nil
}

func conversationCursorURL(record conversationRecord) string {
	return "/chat/c/" + url.PathEscape(record.ID) + "?project_id=" + url.QueryEscape(string(record.ProjectID))
}

func (s *Service) commandPostConversationPG(ctx context.Context, scope nativeScope, id string, command conversation.Command) (json.RawMessage, error) {
	service := s.conversations
	current, err := s.readPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	var prior conversationstore.Command
	var priorFound bool
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		prior, priorFound, err = reader.Command(ctx, id, command.Key)
		return err
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	if priorFound {
		if prior.RequestHash != conversation.RequestHash(command) {
			return nil, &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: 409, publicMessage: true}
		}
		return json.Marshal(prior.Receipt)
	}
	if !s.ready.Load() {
		return nil, &nativeError{Code: "unavailable", Message: "The Hub is shutting down; try again", status: 503}
	}
	if err := s.requireConversationFeaturePG(ctx, scope.organization, "collaboration", s.config.now()); err != nil {
		return nil, err
	}
	plan := &conversationPublicationPlan{service: service, record: current, original: current, now: s.config.now()}
	ctx = plan.context(ctx)
	dispatchFence := leasestore.Fence{LeaseID: current.Execution.Owner.LeaseID, FencingToken: current.Execution.Owner.FencingToken}
	var question conversationstore.Question
	if command.Kind == conversation.CommandAnswer {
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			question, err = reader.Question(ctx, id, command.QuestionID)
			return err
		})
		if err != nil {
			return nil, translateConversationError(err)
		}
		if question.Status != conversation.QuestionPending || (question.ExpiresAt != nil && !s.config.now().Before(*question.ExpiresAt)) {
			return nil, conversationStale("The question is no longer open")
		}
		if err := conversationRequireOwner(current.Execution.Owner, conversation.Expected{AttemptID: question.Owner.AttemptID}); err != nil {
			return nil, err
		}
		if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
			return nil, err
		}
		if err := conversation.ValidateAnswers(question.Question, command.Answers); err != nil {
			return nil, nativeInvalid(err.Error())
		}
	}
	message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: id, Role: conversation.RoleUser, Actor: conversationActorFor(scope), CommandKey: command.Key, CreatedAt: plan.now, UpdatedAt: plan.now}
	var retryRevision int64
	switch command.Kind {
	case conversation.CommandMessage:
		message.Kind, message.Text, message.Data = conversation.MessageText, command.Text, coordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
		if current.WorkItemID != "" {
			if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
				return nil, err
			}
			if conversationBetweenTurns(current.Execution) {
				return nil, conversationStale("No provider turn is active; send the message after the next turn starts or this attempt finishes")
			}
			message.Delivery, message.AttemptID, message.TurnID = conversation.DeliveryQueued, current.Execution.Owner.AttemptID, current.Execution.Owner.TurnID
		}
	case conversation.CommandAnswer:
		answers, err := marshalNative(command.Answers)
		if err != nil {
			return nil, err
		}
		message.Kind, message.Text = conversation.MessageAnswer, renderAnswer(question.Question, command.Answers)
		message.Data = json.RawMessage(answers)
		message.Delivery = conversation.DeliveryQueued
		message.AttemptID, message.ThreadID, message.TurnID = question.Owner.AttemptID, question.Owner.ThreadID, question.Owner.TurnID
	case conversation.CommandInterrupt:
		if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
			return nil, err
		}
		switch current.Execution.Status {
		case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput:
		default:
			return nil, conversationStale("No attempt is running")
		}
		message.Kind, message.Delivery = conversation.MessageInterrupt, conversation.DeliveryQueued
		message.AttemptID, message.ThreadID, message.TurnID = current.Execution.Owner.AttemptID, current.Execution.Owner.ThreadID, current.Execution.Owner.TurnID
	case conversation.CommandContinue:
		if current.WorkItemID == "" {
			return nil, conversationUnsupported("Continue requires a linked issue")
		}
		if conversationExecutionLive(current.Execution.Status) {
			return nil, conversationStale("An attempt is still running")
		}
		if strings.TrimSpace(command.Expected.AttemptID) == "" {
			var hadAttempt bool
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				hadAttempt, err = reader.HasConversationStart(ctx, id)
				return err
			})
			if err != nil {
				return nil, translateConversationError(err)
			}
			if current.Execution.Owner.AttemptID != "" || hadAttempt {
				return nil, nativePublicInvalid("expected.attempt_id is required to continue a conversation that had an attempt")
			}
		}
		if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
			return nil, err
		}
		message.Kind, message.Text, message.Data = conversation.MessageContinue, command.Text, coordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
	case conversation.CommandRetry:
		var original conversationstore.Message
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			original, err = reader.Message(ctx, id, command.MessageID)
			return err
		})
		if err != nil {
			return nil, translateConversationError(err)
		}
		if original.Role != conversation.RoleUser {
			return nil, nativePublicInvalid("Only a message you sent can be retried")
		}
		switch original.Delivery {
		case conversation.DeliveryUnknown, conversation.DeliveryFailed, conversation.DeliveryRejected:
		default:
			return nil, nativePublicInvalid(fmt.Sprintf("A message with delivery %s cannot be retried", original.Delivery))
		}
		message = conversationMessageFromStore(original)
		if original.ContentReference != "" {
			var ref attachment.ArchiveReference
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				object, err := reader.ConversationContent(ctx, original.ContentReference)
				ref = object.Reference
				return err
			})
			if err != nil {
				return nil, translateConversationError(err)
			}
			var content conversationContent
			if err := s.readConversationObject(ctx, string(scope.organization), ref, &content); err != nil {
				return nil, err
			}
			message.Text, message.Data = content.Text, content.Data
		}
		retryRevision = original.Revision
		message.Data = coordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
		if current.WorkItemID != "" {
			message.Delivery = conversation.DeliveryQueued
		}
		message.AttemptID, message.ThreadID, message.TurnID = "", "", ""
		message.UpdatedAt = plan.now
	default:
		return nil, conversationUnsupported("This conversation command requires the worker control migration")
	}

	if len(command.Attachments) > 0 {
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			message.Attachments, err = loadCommandAttachmentsPG(ctx, reader, scope, id, command.Attachments, plan.now)
			return err
		})
		if err != nil {
			return nil, translateConversationError(err)
		}
	}
	if command.Kind == conversation.CommandMessage || command.Kind == conversation.CommandAnswer || command.Kind == conversation.CommandInterrupt || command.Kind == conversation.CommandContinue || command.Kind == conversation.CommandRetry {
		if err := preparePGConversationMessage(ctx, plan, &message); err != nil {
			return nil, err
		}
	}
	prepared := plan.messages[message.ID]
	var attachmentEventContent string
	if len(command.Attachments) > 0 {
		raw, err := json.Marshal(projectMessage(prepared))
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(conversationContent{Data: raw})
		if err != nil {
			return nil, err
		}
		ref, err := plan.prepare(ctx, payload)
		if err != nil {
			return nil, err
		}
		attachmentEventContent = contentArchiveID(string(scope.organization), ref)
	}
	result := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: prepared.Delivery, MessageID: message.ID, UpdatedAt: plan.now}
	if command.Kind == conversation.CommandAnswer {
		result.QuestionID = question.ID
		question.Status, question.Answers, question.AnsweredBy, question.UpdatedAt = conversation.QuestionSending, command.Answers, scope.credential.ID, plan.now
	}
	if command.Kind == conversation.CommandInterrupt {
		current.Execution.Status = conversation.ExecutionInterrupting
		current.Execution.UpdatedAt = plan.now
	}
	if command.Kind == conversation.CommandContinue {
		current.Execution = conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: current.Execution.Owner.ThreadID}, UpdatedAt: plan.now}
	}
	var stored conversationRecord
	var receipt conversation.Receipt
	var replay bool
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if current.WorkItemID != "" && (command.Kind == conversation.CommandContinue || command.Kind == conversation.CommandRetry) {
			if _, err := writer.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(current.WorkItemID)); err != nil {
				return err
			}
		}
		if err := registerPreparedConversationObjects(ctx, writer, scope.organization, plan.prepared, plan.now); err != nil {
			return err
		}
		storedConversation, err := writer.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if storedConversation.Revision != current.Revision || storedConversation.WorkItemID != current.WorkItemID {
			return conversationStale("The conversation changed; retry the command")
		}
		if storedConversation.ReadAudience != "" {
			audience, err := conversationReadAudienceRecord(ctx, writer, scope.organization, scope.credential)
			if err != nil || audience != storedConversation.ReadAudience {
				return nativeNotFound()
			}
		}
		if storedConversation.Visibility == conversation.VisibilityPrivate && storedConversation.OwnerPrincipalID != scope.credential.ID {
			return nativeNotFound()
		}
		if err := authorizeConversationLinkedItemPG(ctx, writer, scope, conversationRecordFromStore(storedConversation)); err != nil {
			return err
		}
		if command.Kind == conversation.CommandAnswer {
			lockedQuestion, err := writer.Question(ctx, id, command.QuestionID)
			if err != nil {
				return translateConversationError(err)
			}
			if lockedQuestion.Status != conversation.QuestionPending || (lockedQuestion.ExpiresAt != nil && !plan.now.Before(*lockedQuestion.ExpiresAt)) {
				return conversationStale("The question is no longer open")
			}
			if lockedQuestion.Owner.AttemptID != question.Owner.AttemptID || lockedQuestion.Owner.ThreadID != question.Owner.ThreadID || lockedQuestion.Owner.TurnID != question.Owner.TurnID {
				return conversationStale("The question changed; retry the command")
			}
			question = lockedQuestion
			question.Status, question.Answers, question.AnsweredBy, question.UpdatedAt = conversation.QuestionSending, command.Answers, scope.credential.ID, plan.now
		}
		reservation := conversationstore.Command{OrganizationID: scope.organization, ConversationID: id, Key: command.Key, Kind: command.Kind, RequestHash: conversation.RequestHash(command), Receipt: conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliverySaved, UpdatedAt: plan.now}, CreatedAt: plan.now, UpdatedAt: plan.now}
		existing, created, err := writer.ReserveCommand(ctx, reservation)
		if err != nil {
			if errors.Is(err, corestore.ErrUniqueViolation) {
				return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: 409, publicMessage: true}
			}
			return err
		}
		if !created {
			receipt, replay = existing.Receipt, true
			return nil
		}

		var owner conversationstore.AttachmentWriter
		var attachments []conversationAttachmentRecord
		if len(command.Attachments) > 0 {
			var ok bool
			owner, ok = writer.(conversationstore.AttachmentWriter)
			if !ok {
				return conversationUnsupported("Conversation attachment owner is unavailable")
			}
			if err := s.authorizeConversationAttachmentPG(ctx, writer, scope, storedConversation, true); err != nil {
				return err
			}
			attachments, err = loadCommandAttachmentsPG(ctx, writer, scope, id, command.Attachments, s.config.now())
			if err != nil {
				return err
			}
		}
		pending, err := writer.PendingUserMessages(ctx, id)
		if err != nil {
			return err
		}
		if pending >= service.config.ControlQueueSize {
			return conversationQueueFull()
		}
		var createdMessage conversationstore.Message
		if command.Kind == conversation.CommandRetry {
			createdMessage, err = writer.Message(ctx, id, prepared.ID)
		} else {
			createdMessage, err = writer.CreateMessage(ctx, conversationMessageToStore(scope.organization, prepared))
		}
		if command.Kind == conversation.CommandRetry {
			if err != nil {
				return err
			}
			if createdMessage.Revision != retryRevision || createdMessage.Role != conversation.RoleUser || (createdMessage.Delivery != conversation.DeliveryUnknown && createdMessage.Delivery != conversation.DeliveryFailed && createdMessage.Delivery != conversation.DeliveryRejected) {
				return conversationStale("The message changed; retry the command")
			}
			createdMessage, err = writer.UpdateMessage(ctx, conversationMessageToStore(scope.organization, prepared), retryRevision)
			if err != nil {
				return err
			}
			prepared.Seq, prepared.Revision = createdMessage.Sequence, createdMessage.Revision
			if err := appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventMessageUpdated, projectMessage(prepared), plan.now); err != nil {
				return err
			}
		} else {
			if err != nil {
				return err
			}
			prepared.Seq, prepared.Revision = createdMessage.Sequence, createdMessage.Revision
			message = prepared
		}

		for i := range attachments {
			if attachments[i].ID != prepared.Attachments[i].ID || attachments[i].ContentID != prepared.Attachments[i].ContentID || attachments[i].resource() != prepared.Attachments[i].resource() {
				return conversationStale("Attachment changed; retry the command")
			}
			if err := owner.BindConversationAttachment(ctx, scope.project, id, attachments[i].ID, scope.credential.ID, createdMessage.ID, s.config.now()); err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nativeInvalid("Attachment is no longer available")
				}
				return err
			}
			attachments[i].MessageID, attachments[i].ExpiresAt = createdMessage.ID, nil
		}
		message.Attachments = attachments
		if command.Kind == conversation.CommandAnswer {
			if err := writer.UpsertQuestion(ctx, question); err != nil {
				return err
			}
			if err := appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventQuestionUpdated, question.Question, plan.now); err != nil {
				return err
			}
		}
		if command.Kind == conversation.CommandInterrupt {
			current.Execution = storedConversation.Execution
			current.Execution.Status = conversation.ExecutionInterrupting
			current.Execution.UpdatedAt = plan.now
			storedConversation.Execution = current.Execution
			storedConversation.UpdatedAt = plan.now
		}
		messageBody, err := json.Marshal(projectMessage(message))
		if err != nil {
			return err
		}
		if attachmentEventContent != "" {
			messageBody = json.RawMessage(`{}`)
		}
		acceptedEvent, err := writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.organization, ConversationID: id, ContentReference: attachmentEventContent, Event: conversation.Event{Type: conversation.EventMessageAccepted, Data: messageBody, CreatedAt: plan.now}})
		if err != nil {
			return err
		}
		storedConversation.EventSequence = acceptedEvent.Seq
		if command.Kind == conversation.CommandInterrupt {
			if err := appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventExecutionUpdated, projectExecution(current.Execution), plan.now); err != nil {
				return err
			}
		}
		if command.Kind == conversation.CommandContinue || storedConversation.Status == conversation.StatusSettled || command.Kind == conversation.CommandInterrupt {
			previousRevision := storedConversation.Revision
			wasSettled := storedConversation.Status == conversation.StatusSettled
			if wasSettled {
				storedConversation.Status = conversation.StatusActive
				storedConversation.SettledAt = nil
			}
			if command.Kind == conversation.CommandInterrupt || command.Kind == conversation.CommandContinue {
				storedConversation.Execution = current.Execution
			}
			if command.Kind == conversation.CommandContinue {
				storedConversation.ExecutionContinuation = true
			}
			storedConversation.UpdatedAt = plan.now
			storedConversation, err = writer.UpdateConversation(ctx, storedConversation, previousRevision)
			if err != nil {
				return err
			}
			if wasSettled {
				if err := appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventConversationUpdated, projectConversation(conversationRecordFromStore(storedConversation)), plan.now); err != nil {
					return err
				}
			}
		}
		if command.Kind == conversation.CommandContinue {
			if s.config.ConversationDispatch == nil {
				return conversationDispatchUnavailable()
			}
			if err := s.config.ConversationDispatch.RequestDispatch(ctx, writer, scope.project, current.WorkItemID, dispatchFence, plan.now); err != nil {
				return err
			}
			if err := appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventExecutionUpdated, projectExecution(current.Execution), plan.now); err != nil {
				return err
			}
		}
		if command.Kind == conversation.CommandRetry && current.WorkItemID != "" && !conversationExecutionLive(current.Execution.Status) {
			if s.config.ConversationDispatch == nil {
				return conversationDispatchUnavailable()
			}
			if err := s.config.ConversationDispatch.RequestDispatch(ctx, writer, scope.project, current.WorkItemID, dispatchFence, plan.now); err != nil {
				return err
			}
		}
		result.Status, result.MessageID = createdMessage.Delivery, createdMessage.ID
		result.UpdatedAt = plan.now
		if command.Kind == conversation.CommandAnswer {
			result.QuestionID = question.ID
		}
		if err := writer.UpdateCommand(ctx, conversationstore.Command{OrganizationID: scope.organization, ConversationID: id, Key: command.Key, Kind: command.Kind, RequestHash: reservation.RequestHash, Receipt: result, UpdatedAt: plan.now}); err != nil {
			return err
		}
		if err := appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventCommandReceipt, result, plan.now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	if replay {
		return json.Marshal(receipt)
	}
	stored = current
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		stored, err = readConversationRecordFromTenant(ctx, reader, scope.project, id)
		return err
	}); err != nil {
		return nil, err
	}
	service.committed(stored)
	return json.Marshal(result)
}

func (s *Service) commandCreateConversationPG(ctx context.Context, scope nativeScope, request conversationCreateRequest, command conversation.Command, record conversationRecord, plan *conversationPublicationPlan) (json.RawMessage, error) {
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	payload, err := recordPayload(record)
	if err != nil {
		return nil, err
	}
	archive, err := messageContent(conversationMessageRecord{Data: payload})
	if err != nil {
		return nil, err
	}
	ref, ok := plan.prepared[string(scope.organization)+"/"+artifact.Digest(archive)]
	if !ok {
		return nil, errors.New("conversation record was not prepared before the transaction")
	}
	record.CreationKey, record.CreationHash = request.Key, hash
	record.ContentID = contentArchiveID(string(scope.organization), ref)
	var stored conversationRecord
	var receipt *conversation.Receipt
	created := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if err := registerPreparedConversationObjects(ctx, writer, scope.organization, plan.prepared, plan.now); err != nil {
			return err
		}
		existing, err := writer.ConversationByCreationKey(ctx, scope.credential.ID, request.Key)
		if err == nil {
			if existing.CreationHash != hash {
				return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: 409, publicMessage: true}
			}
			stored = conversationRecordFromStore(existing)
			if err := authorizeConversationLinkedItemPG(ctx, writer, scope, stored); err != nil {
				return err
			}
			if request.FirstMessage != nil {
				commandRecord, found, err := writer.Command(ctx, existing.ID, command.Key)
				if err != nil {
					return err
				}
				if !found {
					return errors.New("conversation creation receipt is missing")
				}
				copy := commandRecord.Receipt
				receipt = &copy
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if record.SubjectWorkItemID != "" {
			if _, err := writer.WorkItem(ctx, record.ProjectID, tracker.NativeWorkItemID(record.SubjectWorkItemID)); err != nil {
				return err
			}
		}
		if err := authorizeConversationLinkedItemPG(ctx, writer, scope, record); err != nil {
			return err
		}
		createdRecord, err := writer.CreateConversation(ctx, conversationRecordToStore(record))
		if err != nil {
			return err
		}
		stored = conversationRecordFromStore(createdRecord)
		created = true
		if request.FirstMessage != nil {
			if len(plan.messageIDs) == 0 {
				return errors.New("first message was not prepared before the transaction")
			}
			prepared, found := plan.messages[plan.messageIDs[0]]
			if !found {
				return errors.New("first message content was not prepared before the transaction")
			}
			initial := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliverySaved, UpdatedAt: plan.now}
			commandRecord := conversationstore.Command{OrganizationID: scope.organization, ConversationID: record.ID, Key: command.Key, Kind: command.Kind, RequestHash: conversation.RequestHash(command), Receipt: initial, CreatedAt: plan.now, UpdatedAt: plan.now}
			if _, created, err := writer.ReserveCommand(ctx, commandRecord); err != nil {
				return err
			} else if !created {
				return corestore.ErrUniqueViolation
			}
			message, err := writer.CreateMessage(ctx, conversationMessageToStore(scope.organization, prepared))
			if err != nil {
				return err
			}
			prepared.Seq, prepared.Revision = message.Sequence, message.Revision
			body, err := json.Marshal(projectMessage(prepared))
			if err != nil {
				return err
			}
			if _, err := writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.organization, ConversationID: record.ID, Event: conversation.Event{Type: conversation.EventMessageAccepted, Data: body, CreatedAt: plan.now}}); err != nil {
				return err
			}
			result := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: prepared.Delivery, MessageID: prepared.ID, UpdatedAt: plan.now}
			if err := writer.UpdateCommand(ctx, conversationstore.Command{OrganizationID: scope.organization, ConversationID: record.ID, Key: command.Key, Kind: command.Kind, RequestHash: commandRecord.RequestHash, Receipt: result, UpdatedAt: plan.now}); err != nil {
				return err
			}
			if err := appendPGConversationEvent(ctx, writer, scope.organization, record.ID, conversation.EventCommandReceipt, result, plan.now); err != nil {
				return err
			}
			receipt = &result
		}
		return nil
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	resource := projectConversation(stored)
	resource.WebURL, err = s.canonicalBrowserLink(ctx, conversationCursorURL(stored))
	if err != nil {
		return nil, err
	}
	if created && receipt != nil {
		s.conversations.committed(stored)
	}
	return json.Marshal(conversationCreatedResponse{Conversation: resource, Receipt: receipt})
}

func (s *Service) commandPatchConversationPG(ctx context.Context, scope nativeScope, id string, request conversationPatchRequest, title string) (json.RawMessage, error) {
	var updated conversationstore.Conversation
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		current, err := writer.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if current.ReadAudience != "" {
			audience, err := conversationReadAudienceRecord(ctx, writer, scope.organization, scope.credential)
			if err != nil || audience != current.ReadAudience {
				return nativeNotFound()
			}
		}
		if current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.credential.ID {
			return nativeNotFound()
		}
		if err := authorizeConversationLinkedItemPG(ctx, writer, scope, conversationRecordFromStore(current)); err != nil {
			return err
		}
		if current.OwnerPrincipalID == scope.credential.ID {
			if err := s.authorizeAccessScope(ctx, writer, scope, false); err != nil {
				return err
			}
		} else if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if request.ProjectID != "" && tracker.ProjectID(request.ProjectID) != current.ProjectID {
			return conversationProjectFixed()
		}
		if request.Preferences != nil {
			preferences := request.Preferences.apply(current.Preferences)
			if err := conversation.ValidatePreferences(preferences, nil); err != nil {
				return nativePublicInvalid("Conversation preferences are invalid")
			}
			current.Preferences = preferences
		}
		previousRevision := current.Revision
		if request.Title != nil || request.Preferences == nil {
			current.Title = title
		}
		current.UpdatedAt = s.config.now()
		updated, err = writer.UpdateConversation(ctx, current, previousRevision)
		if err != nil {
			return err
		}
		return appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventConversationUpdated, projectConversation(conversationRecordFromStore(updated)), current.UpdatedAt)
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	record, err := s.readPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	s.conversations.committed(record)
	return json.Marshal(projectConversation(record))
}

func conversationRecordToStore(record conversationRecord) conversationstore.Conversation {
	return conversationstore.Conversation{
		OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, ID: record.ID,
		CreationKey: record.CreationKey, CreationHash: record.CreationHash, OwnerPrincipalID: record.OwnerPrincipalID,
		OwnerSubject: record.OwnerSubject, Origin: record.Origin, Title: record.Title, Visibility: record.Visibility,
		Status: record.Status, SubjectWorkItemID: record.SubjectWorkItemID, WorkItemID: record.WorkItemID,
		LinkedAt: record.LinkedAt, Revision: record.Revision, ProviderThreadID: record.ProviderThreadID,
		ProviderThreadRunnerID: record.ProviderThreadRunnerID, ProviderThreadOrigin: record.ProviderThreadOrigin,
		Execution: record.Execution, ExecutionContinuation: record.ExecutionContinuation, EventSequence: record.EventSeq, Preferences: record.Preferences,
		ReadAudience: record.ReadAudience, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		LastMessageAt: record.LastMessageAt, SettledAt: record.SettledAt, Archived: record.Archived,
		ContentReference: record.ContentID,
	}
}

func conversationMessageToStore(organization tracker.OrganizationID, message conversationMessageRecord) conversationstore.Message {
	text, data := message.Text, message.Data
	textBytes := message.TextBytes
	if textBytes == 0 {
		textBytes = int64(len(message.Text))
	}
	if message.ContentID != "" {
		text, data = "", json.RawMessage(`{}`)
	}
	return conversationstore.Message{OrganizationID: organization, ConversationID: message.ConversationID, ID: message.ID, Role: message.Role, Kind: message.Kind, Text: text, Data: data, Delivery: message.Delivery, AttemptID: message.AttemptID, ThreadID: message.ThreadID, TurnID: message.TurnID, ProviderItemID: message.ProviderItemID, Actor: message.Actor, CommandKey: message.CommandKey, CreatedAt: message.CreatedAt, UpdatedAt: message.UpdatedAt, ContentReference: message.ContentID, Revision: 1, TextBytes: textBytes, StoredTextBytes: int64(len(message.Text))}
}

func registerPreparedConversationObjects(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, prepared preparedConversationContents, at time.Time) error {
	registered := make(map[string]struct{}, len(prepared))
	for _, ref := range prepared {
		id := contentArchiveID(string(organization), ref)
		if _, ok := registered[id]; ok {
			continue
		}
		if err := writer.PutConversationContent(ctx, conversationstore.ContentObject{OrganizationID: organization, ID: id, Reference: ref, CreatedAt: at}); err != nil {
			return err
		}
		registered[id] = struct{}{}
	}
	return nil
}

func preparePGConversationMessage(ctx context.Context, plan *conversationPublicationPlan, message *conversationMessageRecord) error {
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	message.ConversationID = plan.record.ID
	if message.CreatedAt.IsZero() {
		message.CreatedAt = plan.now
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	prepared, err := plan.message(ctx, *message)
	if err != nil {
		return err
	}
	prepared.Revision = 1
	plan.messageIDs = append(plan.messageIDs, prepared.ID)
	if plan.revisions == nil {
		plan.revisions = map[string]int64{}
	}
	plan.revisions[prepared.ID] = 1
	plan.rememberMessage(prepared)
	if plan.record.LastMessageAt == nil || plan.record.LastMessageAt.Before(prepared.CreatedAt) {
		at := prepared.CreatedAt
		plan.record.LastMessageAt = &at
	}
	*message = prepared
	return nil
}

func appendPGConversationEvent(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, conversationID string, eventType conversation.EventType, body any, at time.Time) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: conversationID, Event: conversation.Event{Type: eventType, Data: data, CreatedAt: at}})
	return err
}

func readConversationRecordFromTenant(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, id string) (conversationRecord, error) {
	stored, err := reader.Conversation(ctx, project, id)
	if err != nil {
		return conversationRecord{}, err
	}
	return conversationRecordFromStore(stored), nil
}
