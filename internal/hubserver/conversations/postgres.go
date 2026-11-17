package conversations

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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ConversationDispatchUnavailable() error {
	return &hubapi.Error{Code: "unavailable", Message: "Conversation dispatch authority is unavailable", Status: 503}
}

func conversationSnapshotUnavailable() error {
	return &hubapi.Error{Code: "unavailable", Message: "Conversation storage is unavailable", Status: 503}
}

func (s *Server) conversationRecordsSelected() bool {
	return s.config().TenantRepository != nil
}

func (s *Server) AuthorizeConversationScopePG(ctx context.Context, scope hubauth.Input, write bool) error {
	return s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.authorizeAccessScope(ctx, reader, scope, write)
	})
}

func (s *Server) requireConversationFeaturePG(ctx context.Context, organization tracker.OrganizationID, feature string, now time.Time) error {
	return s.config().TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.deps.RequireFeature(ctx, reader, feature, now)
	})
}

func (s *Server) AuthorizeConversationWorkerPG(ctx context.Context, reader tenantstore.Reader, scope hubauth.Input, record ConversationRecord, identity WorkerIdentity, runID string, allowReleased, allowUnstarted bool) (leasestore.Lease, error) {
	if err := RequireConversationRecordOwner(record, identity, allowReleased); err != nil {
		return leasestore.Lease{}, err
	}
	if runID == "" {
		runID = record.Execution.Owner.RunID
	}
	lease, attempt, err := s.AuthorizeConversationWorkerLeasePG(ctx, reader, scope, record.WorkItemID, identity, runID, allowReleased, allowUnstarted && record.Execution.Status == conversation.ExecutionStarting)
	if err != nil {
		return leasestore.Lease{}, err
	}
	if record.Execution.Owner.RunID != "" && attempt.RunID != record.Execution.Owner.RunID {
		return leasestore.Lease{}, ConversationStale("The attempt is not the current work item owner")
	}
	return lease, nil
}

func (s *Server) AuthorizeConversationWorkerLeasePG(ctx context.Context, reader tenantstore.Reader, scope hubauth.Input, itemID string, identity WorkerIdentity, runID string, allowReleased, allowUnstarted bool) (leasestore.Lease, leasestore.Attempt, error) {
	if scope.Credential.Scope != hubauth.Worker || scope.Credential.Runner.RunnerID == "" || !scope.Credential.Runner.Valid() || scope.Credential.Runner.OrganizationID != scope.Organization {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationWorkerAuthorityUnavailable()
	}
	if scope.Credential.Runner.Scope == "projects" && !slices.Contains(scope.Credential.Runner.ProjectIDs, scope.Project) {
		return leasestore.Lease{}, leasestore.Attempt{}, hubapi.NotFound()
	}
	credential, err := reader.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	if credential.Scope != string(hubauth.Worker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, s.config().Now()) {
		return leasestore.Lease{}, leasestore.Attempt{}, accessstore.ErrDenied
	}
	if s.config().ConversationWorkerAuthority == nil {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationWorkerAuthorityUnavailable()
	}
	worker := ConversationWorkerIdentity{Credential: credential, Runner: scope.Credential.Runner, Organization: scope.Organization, Project: scope.Project}
	if err := s.config().ConversationWorkerAuthority.VerifyConversationWorker(ctx, reader, worker); err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	lease, err := reader.Lease(ctx, string(identity.LeaseID))
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	item, err := reader.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(itemID))
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	if lease.IssueID != item.ID || lease.MachineID != string(scope.Credential.Runner.MachineID) {
		return leasestore.Lease{}, leasestore.Attempt{}, hubapi.NotFound()
	}
	if lease.OrganizationID != scope.Organization || lease.FencingToken != int64(identity.FencingToken) {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The lease does not belong to this worker and work item")
	}
	if !allowReleased {
		if err := s.deps.CheckMonthlyLease(ctx, reader, scope.Project, lease.LeaseID, s.config().Now()); err != nil {
			return leasestore.Lease{}, leasestore.Attempt{}, err
		}
	}
	attempt, err := reader.Attempt(ctx, identity.AttemptID)
	if errors.Is(err, corestore.ErrNotFound) && allowUnstarted && hubapi.ValidID(runID, "run") && hubapi.ValidID(identity.AttemptID, "attempt") {
		latest, latestErr := reader.LatestAttempt(ctx, item.ID)
		if latestErr != nil && !errors.Is(latestErr, corestore.ErrNotFound) {
			return leasestore.Lease{}, leasestore.Attempt{}, latestErr
		}
		if latestErr == nil && latest.LeaseID == lease.LeaseID {
			return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The lease is already bound to another attempt")
		}
		now := s.config().Now()
		if lease.RenewedAt.After(now) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
			return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The lease is no longer current")
		}
		return lease, leasestore.Attempt{OrganizationID: scope.Organization, ID: identity.AttemptID, IssueID: item.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: runID}, nil
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The attempt is not the running owner of the lease")
	}
	if err != nil {
		return leasestore.Lease{}, leasestore.Attempt{}, err
	}
	if attempt.OrganizationID != scope.Organization || attempt.IssueID != item.ID || attempt.LeaseID != string(identity.LeaseID) || attempt.FencingToken != int64(identity.FencingToken) || (runID != "" && attempt.RunID != runID) {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The attempt is not the current work item owner")
	}
	now := s.config().Now()
	if !allowReleased && (lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || attempt.Status != "running") {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The lease is no longer current")
	}
	if lease.RenewedAt.After(now) {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The lease renewal is in the future")
	}
	if allowReleased && attempt.Status != "running" && attempt.Status != "succeeded" && attempt.Status != "failed" && attempt.Status != "cancelled" && attempt.Status != "interrupted" {
		return leasestore.Lease{}, leasestore.Attempt{}, ConversationStale("The attempt is no longer current")
	}
	return lease, attempt, nil
}

func AuthorizeConversationLinkedItemPG(ctx context.Context, reader tenantstore.Reader, scope hubauth.Input, record ConversationRecord) error {
	if scope.Credential.HostedRole != "reporter" || record.WorkItemID == "" {
		return nil
	}
	item, err := reader.WorkItem(ctx, record.ProjectID, tracker.NativeWorkItemID(record.WorkItemID))
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.NotFound()
		}
		return err
	}
	principal := ""
	if item.ActorPrincipalID != nil {
		principal = *item.ActorPrincipalID
	}
	if !hubauth.ReporterOwns(scope, tracker.Actor{PrincipalID: principal}) {
		return hubapi.NotFound()
	}
	return nil
}

type ConversationLinkMutation struct {
	service     *Server
	scope       hubauth.Input
	original    ConversationRecord
	request     ConversationLinkRequest
	now         time.Time
	publication *ConversationPublicationPlan
	message     ConversationMessageRecord
	issue       tracker.NativeIssue
}

func (m *ConversationLinkMutation) Prepare(ctx context.Context, workItem LinkedWorkItem) error {
	if workItem.Existing || !workItem.Create || workItem.Issue.WorkItemID == "" {
		return hubapi.WorkItemStorageUnavailable()
	}
	record := m.original
	record.WorkItemID = string(workItem.Issue.WorkItemID)
	linkedAt := workItem.Issue.CreatedAt
	record.LinkedAt = &linkedAt
	record.Visibility = conversation.VisibilityShared
	record.Execution = conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: record.Execution.Owner.ThreadID}, UpdatedAt: m.now}
	record.WorkItem = &ConversationWorkItem{ID: string(workItem.Issue.WorkItemID), Identifier: fmt.Sprintf("%s#%d", workItem.ProjectName, workItem.Issue.Number), Title: workItem.Issue.Title, Lane: workItem.Issue.State}
	publication := &ConversationPublicationPlan{
		Service: m.service.conversations(), Record: record, Original: m.original, Now: m.now,
		Prepared: make(PreparedConversationContents), Reads: &ConversationArchiveReads{Content: map[string][]byte{}},
	}
	result, err := hubapi.MarshalString(map[string]any{"issue": ConversationIssueResult(record)})
	if err != nil {
		return err
	}
	message := ConversationMessageRecord{
		ID: conversation.NewMessageID(), ConversationID: record.ID, Role: conversation.RoleSystem,
		Kind: conversation.MessageStatus, Text: fmt.Sprintf("Linked to issue #%d: %s", workItem.Issue.Number, workItem.Issue.Title),
		Data: json.RawMessage(result), Delivery: conversation.DeliverySaved, Actor: ConversationActorFor(m.scope),
		CommandKey: m.request.Key, CreatedAt: m.now, UpdatedAt: m.now, TextBytes: int64(len(fmt.Sprintf("Linked to issue #%d: %s", workItem.Issue.Number, workItem.Issue.Title))),
	}
	if err := PreparePGConversationMessage(ctx, publication, &message); err != nil {
		return err
	}
	m.message = message
	record = publication.Record
	if err := publication.prepareRecord(ctx, record); err != nil {
		return err
	}
	raw, err := RecordPayload(record)
	if err != nil {
		return err
	}
	archive, err := MessageContent(ConversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	ref, ok := publication.Prepared[string(m.scope.Organization)+"/"+artifact.Digest(archive)]
	if !ok {
		return errors.New("linked conversation record was not prepared before the transaction")
	}
	record.ContentID = ContentArchiveID(string(m.scope.Organization), ref)
	publication.Record.ContentID = record.ContentID
	m.publication, m.issue = publication, workItem.Issue
	return nil
}

func (m *ConversationLinkMutation) Write(ctx context.Context, writer tenantstore.Writer, workItem LinkedWorkItem) error {
	if m.publication == nil || m.issue.WorkItemID != workItem.Issue.WorkItemID {
		return errors.New("conversation link was not prepared before the transaction")
	}
	if err := m.service.authorizeAccessScope(ctx, writer, m.scope, true); err != nil {
		return err
	}
	current, err := writer.LockConversation(ctx, m.scope.Project, m.original.ID)
	if err != nil {
		return err
	}
	if current.Revision != m.original.Revision {
		return ConversationStale("The conversation changed; reload and retry")
	}
	if current.SubjectWorkItemID != "" {
		return hubapi.PublicInvalid("Issue questions cannot be linked or shared")
	}
	if current.WorkItemID != "" {
		return ConversationAlreadyLinked(current.ID)
	}
	linkedRecord := ConversationRecordFromStore(current)
	linkedRecord.WorkItemID = m.publication.Record.WorkItemID
	if err := AuthorizeConversationLinkedItemPG(ctx, writer, m.scope, linkedRecord); err != nil {
		return err
	}
	if !m.request.ShareHistory {
		return conversationShareRequired()
	}
	if err := RegisterPreparedConversationObjects(ctx, writer, m.scope.Organization, m.publication.Prepared, m.now); err != nil {
		return err
	}
	record := m.publication.Record
	previousVisibility := current.Visibility
	stored := ConversationRecordToStore(record)
	stored.Revision = current.Revision
	stored.EventSequence = current.EventSequence
	stored.MessageCount = current.MessageCount
	updated, err := writer.UpdateConversation(ctx, stored, current.Revision)
	if err != nil {
		return err
	}
	updatedRecord := ConversationRecordFromStore(updated)
	updatedRecord.WorkItem = record.WorkItem
	if previousVisibility != conversation.VisibilityShared {
		if err := writer.RecordAudience(ctx, record.ID, m.scope.Credential.ID, previousVisibility, conversation.VisibilityShared, m.now); err != nil {
			return err
		}
	}
	if err := AppendPGConversationEvent(ctx, writer, m.scope.Organization, record.ID, conversation.EventConversationUpdated, ProjectConversation(updatedRecord), m.now); err != nil {
		return err
	}
	if err := AppendPGConversationEvent(ctx, writer, m.scope.Organization, record.ID, conversation.EventExecutionUpdated, ProjectExecution(record.Execution), m.now); err != nil {
		return err
	}
	if err := m.queueSavedMessages(ctx, writer, record.ID); err != nil {
		return err
	}
	message := m.message

	created, err := writer.CreateMessage(ctx, ConversationMessageToStore(m.scope.Organization, message))
	if err != nil {
		return err
	}
	message.Seq, message.Revision = created.Sequence, created.Revision
	return AppendPGConversationEvent(ctx, writer, m.scope.Organization, record.ID, conversation.EventMessageAccepted, ProjectMessage(message), m.now)
}

func (m *ConversationLinkMutation) queueSavedMessages(ctx context.Context, writer tenantstore.Writer, conversationID string) error {
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
			if err := AppendPGConversationEvent(ctx, writer, m.scope.Organization, conversationID, conversation.EventMessageUpdated, ProjectMessage(ConversationMessageFromStore(updated)), m.now); err != nil {
				return err
			}
		}
		if page.Next == 0 {
			return nil
		}
		before = page.Next
	}
}

func (s *Server) commandLinkConversationPG(ctx context.Context, scope hubauth.Input, id string, request ConversationLinkRequest) (json.RawMessage, error) {
	preview, err := s.ReadPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	write := preview.OwnerPrincipalID != scope.Credential.ID
	if err := s.AuthorizeConversationScopePG(ctx, scope, write); err != nil {
		return nil, err
	}
	operation := "POST /api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/conversations/" + id + "/link"
	if err := s.requireConversationFeaturePG(ctx, scope.Organization, "collaboration", s.config().Now()); err != nil {
		return nil, err
	}
	if !request.ShareHistory {
		return nil, conversationShareRequired()
	}
	if preview.SubjectWorkItemID != "" {
		return nil, hubapi.PublicInvalid("Issue questions cannot be linked or shared")
	}
	project, err := s.deps.ReadProject(ctx, scope)
	if err != nil {
		return nil, err
	}
	next, err := resolveConversationNext(project, request)
	if err != nil {
		return nil, err
	}
	release, err := s.conversations().Coordinator.Hold(id)
	if err != nil {
		return nil, err
	}
	defer release()
	preview, err = s.ReadPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if err := s.AuthorizeConversationScopePG(ctx, scope, preview.OwnerPrincipalID != scope.Credential.ID); err != nil {
		return nil, err
	}
	if preview.SubjectWorkItemID != "" {
		return nil, hubapi.PublicInvalid("Issue questions cannot be linked or shared")
	}
	now := s.config().Now().UTC().Truncate(time.Microsecond)
	link := &ConversationLinkMutation{service: s, scope: scope, original: preview, request: request, now: now}
	issueRequest := conversationLinkedIssueRequest(preview, request, next)
	hash, err := hubapi.CommandHash(issueRequest)
	if err != nil {
		return nil, err
	}
	if found, err := s.deps.WorkItemReceipt(ctx, scope, operation, issueRequest.IdempotencyKey, hash); err != nil {
		return nil, err
	} else if found {
		if preview.WorkItemID == "" {
			return nil, errors.New("conversation link receipt exists without a linked work item")
		}
		return s.conversationLinkResultPG(ctx, scope, project, id, next)
	}
	if preview.WorkItemID != "" {
		return nil, ConversationAlreadyLinked(preview.ID)
	}
	if err := s.deps.LinkWorkItem(ctx, scope, operation, issueRequest, link); err != nil {
		return nil, err
	}
	return s.conversationLinkResultPG(ctx, scope, project, id, next)
}

func (s *Server) conversationLinkResultPG(ctx context.Context, scope hubauth.Input, project tracker.NativeProject, id string, next conversationLinkNextApplied) (json.RawMessage, error) {
	linked, err := s.ReadPGConversation(ctx, scope, id)
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
	s.conversations().Committed(linked)
	resource := ProjectConversation(linked)
	result := ConversationLinkResult{
		Conversation: resource,
		Issue:        ConversationLinkedIssue{ID: string(issue.WorkItemID), Identifier: fmt.Sprintf("%s#%d", project.Name, issue.Number), NativeIssue: issue},
		Scheduling:   ConversationScheduling{Lane: issue.State},
		Next:         next,
	}
	return json.Marshal(result)
}

func (s *Server) ReadPGConversation(ctx context.Context, scope hubauth.Input, id string) (ConversationRecord, error) {
	var result ConversationRecord
	err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		stored, err := reader.Conversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		result = ConversationRecordFromStore(stored)
		if result.ReadAudience != "" {
			audience, err := ConversationReadAudienceRecord(ctx, reader, scope.Organization, scope.Credential)
			if err != nil || audience != result.ReadAudience {
				return hubapi.NotFound()
			}
		}
		if err := AuthorizeConversationLinkedItemPG(ctx, reader, scope, result); err != nil {
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
			result.WorkItem = &ConversationWorkItem{ID: string(item.NativeID), Identifier: fmt.Sprintf("%s#%d", project.Name, item.Number), Title: item.Title}
			if item.WorkflowStateName != nil {
				result.WorkItem.Lane = *item.WorkflowStateName
			}
		}
		return nil
	})
	if err != nil {
		return ConversationRecord{}, TranslateConversationError(err)
	}
	if result.OrganizationID != scope.Organization || result.ProjectID != scope.Project {
		return ConversationRecord{}, hubapi.NotFound()
	}
	if result.Visibility == conversation.VisibilityPrivate && result.OwnerPrincipalID != scope.Credential.ID {
		return ConversationRecord{}, hubapi.NotFound()
	}
	return result, nil
}

func ConversationContentUnavailable() error {
	return ConversationUnsupported("Conversation content references require the attachments migration")
}

func (c *ConversationService) logReceipt(conversationID, attemptID string, receipt conversation.Receipt) {
	attributes := []any{"conversation_id", conversationID, "command_key", receipt.Key, "kind", string(receipt.Kind), "status", string(receipt.Status), "attempt_id", attemptID}
	if receipt.Error != nil {
		attributes = append(attributes, "error_code", receipt.Error.Code)
	}
	c.Logger.Info("conversation.receipt", attributes...)
}

func (s *Server) ReadConversationObject(ctx context.Context, organization string, ref attachment.ArchiveReference, target any) error {
	if s.conversations().Store.Contents == nil {
		return ConversationContentUnavailable()
	}
	content, err := s.conversations().Store.Contents.ReadContent(ctx, organization, ref)
	if err != nil {
		return fmt.Errorf("read conversation content object: %w", err)
	}
	if err := json.Unmarshal(content, target); err != nil {
		return fmt.Errorf("decode conversation content object: %w", err)
	}
	return nil
}

func ConversationRecordFromStore(record conversationstore.Conversation) ConversationRecord {
	return ConversationRecord{
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

func ConversationMessageFromStore(record conversationstore.Message) ConversationMessageRecord {
	return ConversationMessageRecord{
		Revision: record.Revision, ID: record.ID, ContentID: record.ContentReference,
		TextBytes: record.TextBytes, StoredTextBytes: sql.NullInt64{Int64: record.StoredTextBytes, Valid: !record.StoredTextBytesUnknown},
		ConversationID: record.ConversationID, Seq: record.Sequence, Role: record.Role,
		Kind: record.Kind, Text: record.Text, Data: record.Data, Delivery: record.Delivery,
		AttemptID: record.AttemptID, ThreadID: record.ThreadID, TurnID: record.TurnID,
		ProviderItemID: record.ProviderItemID, Actor: record.Actor, CommandKey: record.CommandKey,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

func (s *Server) ReadPGConversationPage(ctx context.Context, scope hubauth.Input, id string, before int64, limit int) (ConversationPageRead, error) {
	repository, ok := s.config().TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return ConversationPageRead{}, conversationSnapshotUnavailable()
	}
	var page ConversationPageRead
	var compaction conversationstore.CompactionState
	var compactionReference attachment.ArchiveReference
	contentReferences := map[string]attachment.ArchiveReference{}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		stored, err := reader.Conversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		page.Record = ConversationRecordFromStore(stored)
		if page.Record.ReadAudience != "" {
			audience, err := ConversationReadAudienceRecord(ctx, reader, scope.Organization, scope.Credential)
			if err != nil || audience != page.Record.ReadAudience {
				return hubapi.NotFound()
			}
		}
		if page.Record.Visibility == conversation.VisibilityPrivate && page.Record.OwnerPrincipalID != scope.Credential.ID {
			return hubapi.NotFound()
		}
		if err := AuthorizeConversationLinkedItemPG(ctx, reader, scope, page.Record); err != nil {
			return err
		}
		if page.Record.WorkItemID != "" {
			item, err := reader.WorkItem(ctx, page.Record.ProjectID, tracker.NativeWorkItemID(page.Record.WorkItemID))
			if err != nil {
				return err
			}
			project, err := reader.Project(ctx, page.Record.ProjectID)
			if err != nil {
				return err
			}
			page.Record.WorkItem = &ConversationWorkItem{ID: string(item.NativeID), Identifier: fmt.Sprintf("%s#%d", project.Name, item.Number), Title: item.Title}
			if item.WorkflowStateName != nil {
				page.Record.WorkItem.Lane = *item.WorkflowStateName
			}
		}

		compaction = stored.Compaction
		if compaction.ContentReference != "" {
			object, err := reader.ConversationContent(ctx, compaction.ContentReference)
			if err != nil {
				return err
			}
			compactionReference = object.Reference
		}
		var messages conversationstore.MessagePage
		for remaining, cursor := limit, before; remaining > 0; {
			batch, err := reader.Messages(ctx, id, cursor, min(remaining, corestore.MaxPageSize))
			if err != nil {
				return err
			}
			messages.Items = append(batch.Items, messages.Items...)
			messages.Next = batch.Next
			remaining -= len(batch.Items)
			if batch.Next == 0 {
				break
			}
			cursor = batch.Next
		}
		owner, ok := reader.(conversationstore.AttachmentReader)
		if !ok {
			return ConversationUnsupported("Conversation attachment owner is unavailable")
		}
		ids := make([]string, 0, len(messages.Items))
		for _, item := range messages.Items {
			ids = append(ids, item.ID)
		}
		attachments := make(map[string][]conversationstore.Attachment, len(ids))
		for batchIDs := range slices.Chunk(ids, corestore.MaxPageSize) {
			batch, err := owner.MessageAttachments(ctx, scope.Project, id, batchIDs)
			if err != nil {
				return err
			}
			for messageID, rows := range batch {
				attachments[messageID] = rows
			}
		}
		for _, item := range messages.Items {
			if item.ContentReference != "" {
				object, err := reader.ConversationContent(ctx, item.ContentReference)
				if err != nil {
					return err
				}
				contentReferences[item.ContentReference] = object.Reference
			}
			message := ConversationMessageFromStore(item)
			for _, a := range attachments[item.ID] {
				message.Attachments = append(message.Attachments, ConversationAttachmentFromStore(a))
			}

			references, err := reader.MessageReferences(ctx, id, item.ID)
			if err != nil {
				return err
			}
			for _, reference := range references {
				urlPath, err := MessageReferenceURL(ctx, reader, reference.Kind, reference.TargetID)
				if err != nil {
					return err
				}
				message.References = append(message.References, ConversationMessageReference{Kind: reference.Kind, ID: reference.TargetID, Label: reference.Label, URL: urlPath})
			}
			page.Messages = append(page.Messages, message)
		}
		return nil
	})
	if err != nil {
		return ConversationPageRead{}, TranslateConversationError(err)
	}
	if compaction.ContentReference != "" {
		var payload ConversationContent
		if err := s.ReadConversationObject(ctx, string(scope.Organization), compactionReference, &payload); err != nil {
			return ConversationPageRead{}, err
		}
		if err := json.Unmarshal(payload.Data, &page.compaction); err != nil {
			return ConversationPageRead{}, fmt.Errorf("decode conversation compaction summary: %w", err)
		}
		if page.compaction.ThroughSequence != compaction.ThroughSequence {
			return ConversationPageRead{}, ConversationContentUnavailable()
		}
	}
	for index := range page.Messages {
		message := &page.Messages[index]
		if message.ContentID == "" {
			continue
		}
		ref, ok := contentReferences[message.ContentID]
		if !ok {
			return ConversationPageRead{}, ConversationContentUnavailable()
		}
		var payload ConversationContent
		if err := s.ReadConversationObject(ctx, string(scope.Organization), ref, &payload); err != nil {
			return ConversationPageRead{}, err
		}
		if message.StoredTextBytes.Valid && message.StoredTextBytes.Int64 != int64(len(payload.Text)) {
			return ConversationPageRead{}, errors.New("message stored text length does not match content")
		}
		if message.TextBytes != int64(len(payload.Text)) {
			return ConversationPageRead{}, errors.New("message text length does not match content")
		}
		message.Text, message.Data = payload.Text, payload.Data
	}
	return page, nil
}

func (s *Server) ReplayPGConversationEvents(ctx context.Context, scope hubauth.Input, stream *ConversationStream, id string, cursor, through int64) (int64, error) {
	repository, ok := s.config().TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return cursor, conversationSnapshotUnavailable()
	}
	for cursor < through {
		var page conversationstore.EventPage
		refs := map[string]attachment.ArchiveReference{}
		err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
				return err
			}
			record, err := reader.Conversation(ctx, scope.Project, id)
			if err != nil {
				return err
			}
			current := ConversationRecordFromStore(record)
			if current.ReadAudience != "" {
				audience, err := ConversationReadAudienceRecord(ctx, reader, scope.Organization, scope.Credential)
				if err != nil || audience != current.ReadAudience {
					return hubapi.NotFound()
				}
			}
			if current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.Credential.ID {
				return hubapi.NotFound()
			}
			if err := AuthorizeConversationLinkedItemPG(ctx, reader, scope, current); err != nil {
				return err
			}
			if cursor < record.Compaction.ThroughSequence {
				return ErrConversationCursorExpired
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
			return cursor, TranslateConversationError(err)
		}
		if len(page.Items) == 0 {
			return cursor, nil
		}
		accepted := map[int]ConversationMessageResource{}
		ids := make([]string, 0, len(page.Items))
		for index := range page.Items {
			stored := &page.Items[index]
			if stored.Seq > through {
				break
			}
			if stored.ContentReference == "" {
				continue
			}
			var payload ConversationContent
			if err := s.ReadConversationObject(ctx, string(scope.Organization), refs[stored.ContentReference], &payload); err != nil {
				return cursor, err
			}
			stored.Data = payload.Data
			if stored.Type == conversation.EventMessageAccepted || stored.Type == conversation.EventMessageUpdated {
				var message ConversationMessageResource
				if err := json.Unmarshal(payload.Data, &message); err != nil {
					return cursor, err
				}
				accepted[index] = message
				ids = append(ids, message.ID)
			}
		}
		if len(ids) > 0 {
			if err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
					return err
				}
				record, err := reader.Conversation(ctx, scope.Project, id)
				if err != nil {
					return err
				}
				current := ConversationRecordFromStore(record)
				if current.ReadAudience != "" {
					audience, err := ConversationReadAudienceRecord(ctx, reader, scope.Organization, scope.Credential)
					if err != nil || audience != current.ReadAudience {
						return hubapi.NotFound()
					}
				}
				if current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.Credential.ID {
					return hubapi.NotFound()
				}
				if err := AuthorizeConversationLinkedItemPG(ctx, reader, scope, current); err != nil {
					return err
				}
				owner, ok := reader.(conversationstore.AttachmentReader)
				if !ok {
					return ConversationUnsupported("Conversation attachment owner is unavailable")
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
				return cursor, TranslateConversationError(err)
			}
		}
		for _, stored := range page.Items {
			if stored.Seq > through {
				return cursor, nil
			}
			if err := stream.Event(stored.Event); err != nil {
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

func conversationCursorURL(record ConversationRecord) string {
	return "/chat/c/" + url.PathEscape(record.ID) + "?project_id=" + url.QueryEscape(string(record.ProjectID))
}

func (s *Server) commandPostConversationPG(ctx context.Context, scope hubauth.Input, id string, command conversation.Command) (json.RawMessage, error) {
	service := s.conversations()
	current, err := s.ReadPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	var prior conversationstore.Command
	var priorFound bool
	err = s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		prior, priorFound, err = reader.Command(ctx, id, command.Key)
		return err
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	if priorFound {
		if prior.RequestHash != conversation.RequestHash(command) {
			return nil, &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: 409, Public: true}
		}
		if command.Kind == conversation.CommandCancel && prior.Receipt.Status == conversation.DeliverySaved {
			if err := service.wakeCoordinator(ctx, id); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				service.Logger.Error("deliver coordinator commands", "conversation_id", id, "error", err)
			}
			return s.readPGConversationCommandReceipt(ctx, scope, id, command.Key)
		}
		return json.Marshal(prior.Receipt)
	}
	if !s.deps.Ready() {
		return nil, &hubapi.Error{Code: "unavailable", Message: "The Hub is shutting down; try again", Status: 503}
	}
	if err := s.requireConversationFeaturePG(ctx, scope.Organization, "collaboration", s.config().Now()); err != nil {
		return nil, err
	}
	plan := &ConversationPublicationPlan{Service: service, Record: current, Original: current, Now: s.config().Now().UTC().Truncate(time.Microsecond)}
	ctx = plan.Context(ctx)
	dispatchFence := leasestore.Fence{LeaseID: current.Execution.Owner.LeaseID, FencingToken: current.Execution.Owner.FencingToken}
	var question conversationstore.Question
	if command.Kind == conversation.CommandAnswer {
		err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			question, err = reader.Question(ctx, id, command.QuestionID)
			if err == nil {
				prior, priorFound, err = reader.Command(ctx, id, command.Key)
			}
			return err
		})
		if err != nil {
			return nil, TranslateConversationError(err)
		}
		if priorFound {
			if prior.RequestHash != conversation.RequestHash(command) {
				return nil, &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: 409, Public: true}
			}
			return json.Marshal(prior.Receipt)
		}
		question, err = s.HydratePGConversationQuestion(ctx, scope.Organization, question)
		if err != nil {
			return nil, err
		}
		if question.Status == conversation.QuestionSending || question.Status == conversation.QuestionSent || question.Status == conversation.QuestionAnswered {
			return nil, conversationQuestionAnswered()
		}
		if question.Status != conversation.QuestionPending || (question.ExpiresAt != nil && !s.config().Now().Before(*question.ExpiresAt)) {
			return nil, ConversationStale("The question is no longer open")
		}
		if err := conversationRequireOwner(current.Execution.Owner, conversation.Expected{AttemptID: question.Owner.AttemptID}); err != nil {
			return nil, err
		}
		if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
			return nil, err
		}
		if err := conversation.ValidateAnswers(question.Question, command.Answers); err != nil {
			return nil, hubapi.Invalid(err.Error())
		}
	}
	message := ConversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: id, Role: conversation.RoleUser, Actor: ConversationActorFor(scope), CommandKey: command.Key, CreatedAt: plan.Now, UpdatedAt: plan.Now}
	var retryRevision int64
	switch command.Kind {
	case conversation.CommandMessage:
		message.Kind, message.Text, message.Data = conversation.MessageText, command.Text, CoordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
		if current.WorkItemID != "" {
			if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
				return nil, err
			}
			if ConversationBetweenTurns(current.Execution) {
				return nil, ConversationStale("No provider turn is active; send the message after the next turn starts or this attempt finishes")
			}
			message.Delivery, message.AttemptID, message.TurnID = conversation.DeliveryQueued, current.Execution.Owner.AttemptID, current.Execution.Owner.TurnID
		}
	case conversation.CommandAnswer:
		answers, err := hubapi.MarshalString(command.Answers)
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
			return nil, ConversationStale("No attempt is running")
		}
		message.Kind, message.Delivery = conversation.MessageInterrupt, conversation.DeliveryQueued
		message.AttemptID, message.ThreadID, message.TurnID = current.Execution.Owner.AttemptID, current.Execution.Owner.ThreadID, current.Execution.Owner.TurnID
	case conversation.CommandContinue:
		if current.WorkItemID == "" {
			return nil, ConversationUnsupported("Continue requires a linked issue")
		}
		if conversationExecutionLive(current.Execution.Status) {
			return nil, ConversationStale("An attempt is still running")
		}
		if strings.TrimSpace(command.Expected.AttemptID) == "" {
			var hadAttempt bool
			err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				hadAttempt, err = reader.HasConversationStart(ctx, id)
				return err
			})
			if err != nil {
				return nil, TranslateConversationError(err)
			}
			if current.Execution.Owner.AttemptID != "" || hadAttempt {
				return nil, hubapi.PublicInvalid("expected.attempt_id is required to continue a conversation that had an attempt")
			}
		}
		if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
			return nil, err
		}
		message.Kind, message.Text, message.Data = conversation.MessageContinue, command.Text, CoordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
	case conversation.CommandRetry:
		var original conversationstore.Message
		err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			original, err = reader.Message(ctx, id, command.MessageID)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return nil, hubapi.PublicInvalid("The message to retry is not part of this conversation")
		}
		if err != nil {
			return nil, TranslateConversationError(err)
		}
		if original.Role != conversation.RoleUser {
			return nil, hubapi.PublicInvalid("Only a message you sent can be retried")
		}
		switch original.Delivery {
		case conversation.DeliveryUnknown, conversation.DeliveryFailed, conversation.DeliveryRejected:
		default:
			return nil, hubapi.PublicInvalid(fmt.Sprintf("A message with delivery %s cannot be retried", original.Delivery))
		}
		message = ConversationMessageFromStore(original)
		if original.ContentReference != "" {
			var ref attachment.ArchiveReference
			err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				object, err := reader.ConversationContent(ctx, original.ContentReference)
				ref = object.Reference
				return err
			})
			if err != nil {
				return nil, TranslateConversationError(err)
			}
			var content ConversationContent
			if err := s.ReadConversationObject(ctx, string(scope.Organization), ref, &content); err != nil {
				return nil, err
			}
			message.Text, message.Data = content.Text, content.Data
		}
		retryRevision = original.Revision
		message.Data = CoordinatorConnectionData(ctx)
		message.Delivery = conversation.DeliverySaved
		if current.WorkItemID != "" {
			message.Delivery = conversation.DeliveryQueued
		}
		message.AttemptID, message.ThreadID, message.TurnID = "", "", ""
		message.UpdatedAt = plan.Now

	case conversation.CommandCancel:
		if current.WorkItemID != "" {
			return nil, ConversationUnsupported("Cancel applies to coordinator turns; interrupt the linked attempt instead")
		}
		if err := conversationRequireOwner(current.Execution.Owner, command.Expected); err != nil {
			return nil, err
		}

	default:
		return nil, ConversationUnsupported("This conversation command requires the worker control migration")
	}

	if len(command.Attachments) > 0 {
		err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			message.Attachments, err = loadCommandAttachmentsPG(ctx, reader, scope, id, command.Attachments, plan.Now)
			return err
		})
		if err != nil {
			return nil, TranslateConversationError(err)
		}
	}
	if command.Kind == conversation.CommandMessage || command.Kind == conversation.CommandAnswer || command.Kind == conversation.CommandInterrupt || command.Kind == conversation.CommandContinue || command.Kind == conversation.CommandRetry {
		if err := PreparePGConversationMessage(ctx, plan, &message); err != nil {
			return nil, err
		}
	}
	prepared := plan.Messages[message.ID]
	var attachmentEventContent string
	if len(command.Attachments) > 0 {
		raw, err := json.Marshal(ProjectMessage(prepared))
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(ConversationContent{Data: raw})
		if err != nil {
			return nil, err
		}
		ref, err := plan.Prepare(ctx, payload)
		if err != nil {
			return nil, err
		}
		attachmentEventContent = ContentArchiveID(string(scope.Organization), ref)
	}
	questionReference, questionUpdatedAt := question.ContentReference, question.UpdatedAt
	result := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: prepared.Delivery, MessageID: message.ID, UpdatedAt: plan.Now}
	if command.Kind == conversation.CommandAnswer {
		result.QuestionID = question.ID
		question.Status, question.Answers, question.AnsweredBy, question.UpdatedAt = conversation.QuestionSending, command.Answers, scope.Credential.ID, plan.Now
	}
	if command.Kind == conversation.CommandInterrupt {
		current.Execution.Status = conversation.ExecutionInterrupting
		current.Execution.UpdatedAt = plan.Now
	}
	if command.Kind == conversation.CommandContinue {
		current.Execution = conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: current.Execution.Owner.ThreadID}, UpdatedAt: plan.Now}
	}
	targetTurnID := ""
	if command.Kind == conversation.CommandCancel && conversationExecutionLive(current.Execution.Status) {
		targetTurnID = current.Execution.Owner.TurnID
	}
	if command.Kind == conversation.CommandAnswer {
		if err := plan.PrepareQuestion(ctx, ConversationQuestionRecord{Question: question.Question}); err != nil {
			return nil, err
		}
	}
	var stored ConversationRecord
	var receipt conversation.Receipt
	var replay bool
	queued := 0
	err = s.config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		queued = 0
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if current.WorkItemID != "" && (command.Kind == conversation.CommandContinue || command.Kind == conversation.CommandRetry) {
			if _, err := writer.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(current.WorkItemID)); err != nil {
				return err
			}
		}
		if err := RegisterPreparedConversationObjects(ctx, writer, scope.Organization, plan.Prepared, plan.Now); err != nil {
			return err
		}
		storedConversation, err := writer.LockConversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if storedConversation.ReadAudience != "" {
			audience, err := ConversationReadAudienceRecord(ctx, writer, scope.Organization, scope.Credential)
			if err != nil || audience != storedConversation.ReadAudience {
				return hubapi.NotFound()
			}
		}
		if storedConversation.Visibility == conversation.VisibilityPrivate && storedConversation.OwnerPrincipalID != scope.Credential.ID {
			return hubapi.NotFound()
		}
		if err := AuthorizeConversationLinkedItemPG(ctx, writer, scope, ConversationRecordFromStore(storedConversation)); err != nil {
			return err
		}
		if command.Kind == conversation.CommandAnswer {
			prior, found, err := writer.Command(ctx, id, command.Key)
			if err != nil {
				return err
			}
			if found {
				if prior.RequestHash != conversation.RequestHash(command) {
					return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: 409, Public: true}
				}
				receipt, replay = prior.Receipt, true
				return nil
			}
			lockedQuestion, err := writer.Question(ctx, id, command.QuestionID)
			if err != nil {
				return TranslateConversationError(err)
			}
			if lockedQuestion.Status == conversation.QuestionSending || lockedQuestion.Status == conversation.QuestionSent || lockedQuestion.Status == conversation.QuestionAnswered {
				return conversationQuestionAnswered()
			}
			if lockedQuestion.Status != conversation.QuestionPending || (lockedQuestion.ExpiresAt != nil && !plan.Now.Before(*lockedQuestion.ExpiresAt)) {
				return ConversationStale("The question is no longer open")
			}
			if lockedQuestion.Owner.AttemptID != question.Owner.AttemptID || lockedQuestion.Owner.ThreadID != question.Owner.ThreadID || lockedQuestion.Owner.TurnID != question.Owner.TurnID {
				return ConversationStale("The question changed; retry the command")
			}
			if lockedQuestion.ContentReference != questionReference || !lockedQuestion.UpdatedAt.Equal(questionUpdatedAt) {
				return ConversationStale("The question changed; retry the command")
			}
			prompts := question.Prompts
			question = lockedQuestion
			question.Prompts = prompts
			question.Status, question.Answers, question.AnsweredBy, question.UpdatedAt = conversation.QuestionSending, command.Answers, scope.Credential.ID, plan.Now
		}
		if storedConversation.Revision != current.Revision || storedConversation.WorkItemID != current.WorkItemID {
			return ConversationStale("The conversation changed; retry the command")
		}
		reservation := conversationstore.Command{TargetTurnID: targetTurnID, OrganizationID: scope.Organization, ConversationID: id, Key: command.Key, Kind: command.Kind, RequestHash: conversation.RequestHash(command), Receipt: conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliverySaved, UpdatedAt: plan.Now}, CreatedAt: plan.Now, UpdatedAt: plan.Now}
		existing, created, err := writer.ReserveCommand(ctx, reservation)
		if err != nil {
			if errors.Is(err, corestore.ErrUniqueViolation) {
				return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: 409, Public: true}
			}
			return err
		}
		if !created {
			receipt, replay = existing.Receipt, true
			return nil
		}

		if command.Kind == conversation.CommandCancel {
			result.Status = conversation.DeliverySaved
			result.MessageID = ""
			if targetTurnID == "" {
				result.Status = conversation.DeliveryRejected
				result.Error = &conversation.ReceiptError{Code: "no_active_turn", Message: "No coordinator turn is running"}
			} else if storedConversation.Execution.Owner.TurnID != targetTurnID || !conversationExecutionLive(storedConversation.Execution.Status) {
				return ConversationStale("The coordinator turn changed; retry the command")
			}
			if err := writer.UpdateCommand(ctx, conversationstore.Command{OrganizationID: scope.Organization, ConversationID: id, Key: command.Key, Kind: command.Kind, RequestHash: reservation.RequestHash, Receipt: result, UpdatedAt: plan.Now}); err != nil {
				return err
			}
			return AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventCommandReceipt, result, plan.Now)
		}

		var owner conversationstore.AttachmentWriter
		var attachments []ConversationAttachmentRecord
		if len(command.Attachments) > 0 {
			var ok bool
			owner, ok = writer.(conversationstore.AttachmentWriter)
			if !ok {
				return ConversationUnsupported("Conversation attachment owner is unavailable")
			}
			if err := s.authorizeConversationAttachmentPG(ctx, writer, scope, storedConversation, true); err != nil {
				return err
			}
			attachments, err = loadCommandAttachmentsPG(ctx, writer, scope, id, command.Attachments, s.config().Now())
			if err != nil {
				return err
			}
		}
		if command.Kind != conversation.CommandContinue {
			pending, err := writer.PendingUserMessages(ctx, id)
			if err != nil {
				return err
			}
			if pending >= service.Config.ControlQueueSize {
				queued = pending
				return ConversationQueueFull()
			}
		}
		var createdMessage conversationstore.Message
		if command.Kind == conversation.CommandRetry {
			createdMessage, err = writer.Message(ctx, id, prepared.ID)
		} else {
			createdMessage, err = writer.CreateMessage(ctx, ConversationMessageToStore(scope.Organization, prepared))
		}
		if command.Kind == conversation.CommandRetry {
			if err != nil {
				return err
			}
			if createdMessage.Revision != retryRevision || createdMessage.Role != conversation.RoleUser || (createdMessage.Delivery != conversation.DeliveryUnknown && createdMessage.Delivery != conversation.DeliveryFailed && createdMessage.Delivery != conversation.DeliveryRejected) {
				return ConversationStale("The message changed; retry the command")
			}
			createdMessage, err = writer.UpdateMessage(ctx, ConversationMessageToStore(scope.Organization, prepared), retryRevision)
			if err != nil {
				return err
			}
			prepared.Seq, prepared.Revision = createdMessage.Sequence, createdMessage.Revision
			if err := recordCommandReferences(ctx, writer, ConversationRecordFromStore(storedConversation), &prepared, plan.Now); err != nil {
				return err
			}
			if err := AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventMessageUpdated, ProjectMessage(prepared), plan.Now); err != nil {
				return err
			}
		} else {
			if err != nil {
				return err
			}
			prepared.Seq, prepared.Revision = createdMessage.Sequence, createdMessage.Revision
			if err := recordCommandReferences(ctx, writer, ConversationRecordFromStore(storedConversation), &prepared, plan.Now); err != nil {
				return err
			}
			message = prepared
		}

		for i := range attachments {
			if attachments[i].ID != prepared.Attachments[i].ID || attachments[i].ContentID != prepared.Attachments[i].ContentID || attachments[i].resource() != prepared.Attachments[i].resource() {
				return ConversationStale("Attachment changed; retry the command")
			}
			if err := owner.BindConversationAttachment(ctx, scope.Project, id, attachments[i].ID, scope.Credential.ID, createdMessage.ID, s.config().Now()); err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return hubapi.Invalid("Attachment is no longer available")
				}
				return err
			}
			attachments[i].MessageID, attachments[i].ExpiresAt = createdMessage.ID, nil
		}
		message.Attachments = attachments
		if command.Kind == conversation.CommandAnswer {
			question, err = PreparedPGConversationQuestion(ctx, question)
			if err != nil {
				return err
			}
			if err := writer.UpsertQuestion(ctx, question); err != nil {
				return err
			}
			if err := AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventQuestionUpdated, question.Question, plan.Now); err != nil {
				return err
			}
		}
		if command.Kind == conversation.CommandInterrupt {
			current.Execution = storedConversation.Execution
			current.Execution.Status = conversation.ExecutionInterrupting
			current.Execution.UpdatedAt = plan.Now
			storedConversation.Execution = current.Execution
			storedConversation.UpdatedAt = plan.Now
		}
		messageBody, err := json.Marshal(ProjectMessage(message))
		if err != nil {
			return err
		}
		if attachmentEventContent != "" {
			messageBody = json.RawMessage(`{}`)
		}
		acceptedEvent, err := writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.Organization, ConversationID: id, ContentReference: attachmentEventContent, Event: conversation.Event{Type: conversation.EventMessageAccepted, Data: messageBody, CreatedAt: plan.Now}})
		if err != nil {
			return err
		}
		storedConversation.EventSequence = acceptedEvent.Seq
		if command.Kind == conversation.CommandInterrupt {
			if err := AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventExecutionUpdated, ProjectExecution(current.Execution), plan.Now); err != nil {
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
			storedConversation.UpdatedAt = plan.Now
			storedConversation, err = writer.UpdateConversation(ctx, storedConversation, previousRevision)
			if err != nil {
				return err
			}
			if wasSettled {
				if err := AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventConversationUpdated, ProjectConversation(ConversationRecordFromStore(storedConversation)), plan.Now); err != nil {
					return err
				}
			}
		}
		if command.Kind == conversation.CommandContinue {
			if s.config().ConversationDispatch == nil {
				return ConversationDispatchUnavailable()
			}
			if err := s.config().ConversationDispatch.RequestDispatch(ctx, writer, scope.Project, current.WorkItemID, dispatchFence, plan.Now); err != nil {
				return err
			}
			if err := AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventExecutionUpdated, ProjectExecution(current.Execution), plan.Now); err != nil {
				return err
			}
		}
		if command.Kind == conversation.CommandRetry && current.WorkItemID != "" && !conversationExecutionLive(current.Execution.Status) {
			if s.config().ConversationDispatch == nil {
				return ConversationDispatchUnavailable()
			}
			if err := s.config().ConversationDispatch.QueueDispatch(ctx, writer, scope.Project, current.WorkItemID, dispatchFence, plan.Now); err != nil {
				return err
			}
		}
		result.Status, result.MessageID = createdMessage.Delivery, createdMessage.ID
		result.UpdatedAt = plan.Now
		if command.Kind == conversation.CommandAnswer {
			result.QuestionID = question.ID
		}
		if err := writer.UpdateCommand(ctx, conversationstore.Command{OrganizationID: scope.Organization, ConversationID: id, Key: command.Key, Kind: command.Kind, RequestHash: reservation.RequestHash, Receipt: result, UpdatedAt: plan.Now}); err != nil {
			return err
		}
		if err := AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventCommandReceipt, result, plan.Now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if queued > 0 {
			service.Logger.Warn("conversation.queue_full", "conversation_id", id, "kind", command.Kind, "queued", queued, "limit", service.Config.ControlQueueSize)
		}
		return nil, TranslateConversationError(err)
	}
	if replay {
		return json.Marshal(receipt)
	}
	stored = current
	if err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		stored, err = ReadConversationRecordFromTenant(ctx, reader, scope.Project, id)
		return err
	}); err != nil {
		return nil, err
	}
	service.logReceipt(id, stored.Execution.Owner.AttemptID, result)
	if command.Kind == conversation.CommandCancel {
		service.Broker.Notify(stored.ID)
		if err := service.wakeCoordinator(ctx, id); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			service.Logger.Error("deliver coordinator commands", "conversation_id", id, "error", err)
		}
		return s.readPGConversationCommandReceipt(ctx, scope, id, command.Key)
	}
	service.Committed(stored)

	return json.Marshal(result)
}

func (s *Server) commandCreateConversationPG(ctx context.Context, scope hubauth.Input, request ConversationCreateRequest, command conversation.Command, record ConversationRecord, plan *ConversationPublicationPlan) (json.RawMessage, error) {
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	payload, err := RecordPayload(record)
	if err != nil {
		return nil, err
	}
	archive, err := MessageContent(ConversationMessageRecord{Data: payload})
	if err != nil {
		return nil, err
	}
	ref, ok := plan.Prepared[string(scope.Organization)+"/"+artifact.Digest(archive)]
	if !ok {
		return nil, errors.New("conversation record was not prepared before the transaction")
	}
	record.CreationKey, record.CreationHash = request.Key, hash
	record.ContentID = ContentArchiveID(string(scope.Organization), ref)
	var stored ConversationRecord
	var receipt *conversation.Receipt
	created := false
	err = s.config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if err := RegisterPreparedConversationObjects(ctx, writer, scope.Organization, plan.Prepared, plan.Now); err != nil {
			return err
		}
		existing, err := writer.ConversationByCreationKey(ctx, scope.Credential.ID, request.Key)
		if err == nil {
			if existing.CreationHash != hash {
				return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: 409, Public: true}
			}
			stored = ConversationRecordFromStore(existing)
			if err := AuthorizeConversationLinkedItemPG(ctx, writer, scope, stored); err != nil {
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
		if err := AuthorizeConversationLinkedItemPG(ctx, writer, scope, record); err != nil {
			return err
		}
		createdRecord, err := writer.CreateConversation(ctx, ConversationRecordToStore(record))
		if err != nil {
			return err
		}
		stored = ConversationRecordFromStore(createdRecord)
		created = true
		if request.FirstMessage != nil {
			if len(plan.messageIDs) == 0 {
				return errors.New("first message was not prepared before the transaction")
			}
			prepared, found := plan.Messages[plan.messageIDs[0]]
			if !found {
				return errors.New("first message content was not prepared before the transaction")
			}
			initial := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliverySaved, UpdatedAt: plan.Now}
			commandRecord := conversationstore.Command{OrganizationID: scope.Organization, ConversationID: record.ID, Key: command.Key, Kind: command.Kind, RequestHash: conversation.RequestHash(command), Receipt: initial, CreatedAt: plan.Now, UpdatedAt: plan.Now}
			if _, created, err := writer.ReserveCommand(ctx, commandRecord); err != nil {
				return err
			} else if !created {
				return corestore.ErrUniqueViolation
			}
			message, err := writer.CreateMessage(ctx, ConversationMessageToStore(scope.Organization, prepared))
			if err != nil {
				return err
			}
			prepared.Seq, prepared.Revision = message.Sequence, message.Revision
			body, err := json.Marshal(ProjectMessage(prepared))
			if err != nil {
				return err
			}
			if _, err := writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.Organization, ConversationID: record.ID, Event: conversation.Event{Type: conversation.EventMessageAccepted, Data: body, CreatedAt: plan.Now}}); err != nil {
				return err
			}
			result := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: prepared.Delivery, MessageID: prepared.ID, UpdatedAt: plan.Now}
			if err := writer.UpdateCommand(ctx, conversationstore.Command{OrganizationID: scope.Organization, ConversationID: record.ID, Key: command.Key, Kind: command.Kind, RequestHash: commandRecord.RequestHash, Receipt: result, UpdatedAt: plan.Now}); err != nil {
				return err
			}
			if err := AppendPGConversationEvent(ctx, writer, scope.Organization, record.ID, conversation.EventCommandReceipt, result, plan.Now); err != nil {
				return err
			}
			receipt = &result
		}
		return nil
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	resource := ProjectConversation(stored)
	resource.WebURL, err = s.canonicalBrowserLink(ctx, conversationCursorURL(stored))
	if err != nil {
		return nil, err
	}
	if created && receipt != nil {
		s.conversations().logReceipt(stored.ID, stored.Execution.Owner.AttemptID, *receipt)
		s.conversations().Committed(stored)
	}
	return json.Marshal(ConversationCreatedResponse{Conversation: resource, Receipt: receipt})
}

func (s *Server) CommandPatchConversationPG(ctx context.Context, scope hubauth.Input, id string, request ConversationPatchRequest, title string) (json.RawMessage, error) {
	previous, err := s.ReadPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if previous.Visibility == conversation.VisibilityPrivate && previous.OwnerPrincipalID != scope.Credential.ID {
		return nil, hubapi.NotFound()
	}
	var issue PreferenceIssue = noPreferenceIssue{}
	if request.Preferences != nil {
		err = s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, reader, scope, previous.OwnerPrincipalID != scope.Credential.ID); err != nil {
				return err
			}
			return s.validateConversationPreferences(ctx, reader, ConversationRecordToStore(previous), request.Preferences.apply(previous.Preferences), s.config().Now())
		})
		if err != nil {
			return nil, err
		}
		issue, err = s.deps.PreferenceIssue(ctx, scope, previous, request.Preferences.apply(previous.Preferences))
		if err != nil {
			return nil, err
		}
	}
	var updated conversationstore.Conversation
	err = s.config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if err := issue.Lock(ctx, writer); err != nil {
			return err
		}
		current, err := writer.LockConversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if current.Revision != previous.Revision || current.WorkItemID != previous.WorkItemID {
			return ConversationStale("The conversation changed; reload and retry")
		}
		if current.ReadAudience != "" {
			audience, err := ConversationReadAudienceRecord(ctx, writer, scope.Organization, scope.Credential)
			if err != nil || audience != current.ReadAudience {
				return hubapi.NotFound()
			}
		}
		if current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.Credential.ID {
			return hubapi.NotFound()
		}
		if err := AuthorizeConversationLinkedItemPG(ctx, writer, scope, ConversationRecordFromStore(current)); err != nil {
			return err
		}
		if current.OwnerPrincipalID == scope.Credential.ID {
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
			if err := s.validateConversationPreferences(ctx, writer, current, preferences, s.config().Now()); err != nil {
				return err
			}
			current.Preferences = preferences
		}
		if issue.Changed() {
			if err := s.authorizeWorkItemRecordScope(ctx, writer, scope); err != nil {
				return err
			}
		}
		if err := issue.Write(ctx, writer); err != nil {
			return err
		}
		previousRevision := current.Revision
		if request.Title != nil || request.Preferences == nil {
			current.Title = title
		}
		current.UpdatedAt = s.config().Now()
		updated, err = writer.UpdateConversation(ctx, current, previousRevision)
		if err != nil {
			return err
		}
		return AppendPGConversationEvent(ctx, writer, scope.Organization, id, conversation.EventConversationUpdated, ProjectConversation(ConversationRecordFromStore(updated)), current.UpdatedAt)
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	record, err := s.ReadPGConversation(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	s.conversations().Committed(record)
	return json.Marshal(ProjectConversation(record))
}

func ConversationRecordToStore(record ConversationRecord) conversationstore.Conversation {
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

func ConversationMessageToStore(organization tracker.OrganizationID, message ConversationMessageRecord) conversationstore.Message {
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

func RegisterPreparedConversationObjects(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, prepared PreparedConversationContents, at time.Time) error {
	registered := make(map[string]struct{}, len(prepared))
	for _, ref := range prepared {
		id := ContentArchiveID(string(organization), ref)
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

func PreparePGConversationMessage(ctx context.Context, plan *ConversationPublicationPlan, message *ConversationMessageRecord) error {
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	message.ConversationID = plan.Record.ID
	if message.CreatedAt.IsZero() {
		message.CreatedAt = plan.Now
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	prepared, err := plan.Message(ctx, *message)
	if err != nil {
		return err
	}
	prepared.Revision = 1
	plan.messageIDs = append(plan.messageIDs, prepared.ID)
	if plan.revisions == nil {
		plan.revisions = map[string]int64{}
	}
	plan.revisions[prepared.ID] = 1
	plan.RememberMessage(prepared)
	if plan.Record.LastMessageAt == nil || plan.Record.LastMessageAt.Before(prepared.CreatedAt) {
		at := prepared.CreatedAt
		plan.Record.LastMessageAt = &at
	}
	*message = prepared
	return nil
}

func AppendPGConversationEvent(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, conversationID string, eventType conversation.EventType, body any, at time.Time) error {
	if prepared, err := appendPreparedPGWorkerEvent(ctx, writer, organization, conversationID, eventType, body, at); prepared {
		return err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: conversationID, Event: conversation.Event{Type: eventType, Data: data, CreatedAt: at}})
	return err
}

func ReadConversationRecordFromTenant(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, id string) (ConversationRecord, error) {
	stored, err := reader.Conversation(ctx, project, id)
	if err != nil {
		return ConversationRecord{}, err
	}
	return ConversationRecordFromStore(stored), nil
}

func (s *Server) readPGConversationCommandReceipt(ctx context.Context, scope hubauth.Input, id, key string) (json.RawMessage, error) {
	var command conversationstore.Command
	err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, found, err := r.Command(ctx, id, key)
		if err != nil {
			return err
		}
		if !found {
			return corestore.ErrNotFound
		}
		command = stored
		return nil
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	return json.Marshal(command.Receipt)
}

func PreparedPGConversationQuestion(ctx context.Context, question conversationstore.Question) (conversationstore.Question, error) {
	raw, err := QuestionPayload(ConversationQuestionRecord{Question: question.Question})
	if err != nil {
		return question, err
	}
	content, err := MessageContent(ConversationMessageRecord{Data: raw})
	if err != nil {
		return question, err
	}
	var prepared PreparedConversationContents
	if plan, ok := ctx.Value(PgWorkerPublicationKey{}).(*ConversationPublicationPlan); ok {
		prepared = plan.Prepared
	} else if contents, ok := ctx.Value(ConversationContentsKey{}).(PreparedConversationContents); ok {
		prepared = contents
	}
	ref, found := prepared[string(question.OrganizationID)+"/"+artifact.Digest(content)]
	if !found {
		return question, errors.New("conversation question was not published before the transaction")
	}
	question.ContentReference = ContentArchiveID(string(question.OrganizationID), ref)
	return question, nil
}

func (s *Server) HydratePGConversationQuestion(ctx context.Context, organization tracker.OrganizationID, question conversationstore.Question) (conversationstore.Question, error) {
	if question.ContentReference == "" {
		return question, nil
	}
	var reference attachment.ArchiveReference
	if err := s.config().TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		object, err := r.ConversationContent(ctx, question.ContentReference)
		reference = object.Reference
		return err
	}); err != nil {
		return question, err
	}
	var value ConversationContent
	if err := s.ReadConversationObject(ctx, string(organization), reference, &value); err != nil {
		return question, err
	}
	var payload ConversationQuestionContent
	if err := json.Unmarshal(value.Data, &payload); err != nil {
		return question, fmt.Errorf("decode question content: %w", err)
	}
	question.Prompts, question.Answers = payload.Prompts, payload.Answers
	return question, nil
}

func recordCommandReferences(ctx context.Context, writer tenantstore.Writer, record ConversationRecord, message *ConversationMessageRecord, now time.Time) error {
	references, err := ResolveMessageReferences(ctx, writer, record, message.Text)
	if err != nil {
		return err
	}
	message.References = references
	return ReplaceMessageReferences(ctx, writer, record.OrganizationID, *message, now)
}
