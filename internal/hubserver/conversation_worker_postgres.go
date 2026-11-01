package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const conversationPGWorkerPageSize = corestore.MaxPageSize

func (r *conversationWorkerRouter) bindPostgres(e echo.Context, request conversationBindRequest) error {
	c, s := r.service, r.service.server
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	itemID := e.Param("item")
	now := s.config.now()
	var record conversationRecord
	var item corestore.WorkItem
	var found bool
	var pendingCandidates []conversationstore.Message
	pendingContentRefs := make(map[string]attachment.ArchiveReference)
	pendingContent := make(map[string]conversationContent)
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		var err error
		item, err = reader.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(itemID))
		if err != nil {
			return err
		}
		if item.Archived {
			return nativeNotFound()
		}
		if _, _, err := s.authorizeConversationWorkerLeasePG(ctx, reader, scope, itemID, request.conversationWorkerIdentity, request.RunID, false); err != nil {
			return err
		}
		stored, err := reader.ConversationByWorkItem(ctx, scope.project, itemID)
		if err == nil {
			record, found = conversationRecordFromStore(stored), true
			messages, err := reader.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliveryQueued, conversation.DeliverySending, conversation.DeliverySaved}, Limit: 128})
			if err != nil {
				return err
			}
			pendingCandidates = messages.Items
			for _, message := range pendingCandidates {
				if message.ContentReference == "" {
					continue
				}
				object, err := reader.ConversationContent(ctx, message.ContentReference)
				if err != nil {
					return err
				}
				pendingContentRefs[message.ID] = object.Reference
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		record = conversationRecord{
			ID: conversation.NewConversationID(), OrganizationID: scope.organization, ProjectID: scope.project,
			OwnerPrincipalID: scope.credential.ID, Origin: conversationOriginWorker,
			Title: deriveConversationTitle(item.Title), Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
			WorkItemID: itemID, LinkedAt: &now, Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now},
			CreatedAt: now, UpdatedAt: now, Revision: 1, Preferences: conversation.DefaultPreferences(),
		}
		if item.ActorPrincipalID != nil {
			record.OwnerPrincipalID = *item.ActorPrincipalID
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(e, conversationStaleExecutionError("bind", record.ID, translateConversationError(err)))
	}
	for messageID, reference := range pendingContentRefs {
		var payload conversationContent
		if err := s.readConversationObject(ctx, string(scope.organization), reference, &payload); err != nil {
			return s.nativeAPIError(e, conversationStaleExecutionError("bind", record.ID, err))
		}
		pendingContent[messageID] = payload
	}
	resume, err := s.conversationBindResumePG(ctx, scope, record, request, pendingCandidates)
	if err != nil {
		return s.nativeAPIError(e, conversationStaleExecutionError("bind", record.ID, err))
	}
	initialRecord := record
	execution := conversation.Execution{
		Status: conversation.ExecutionStarting,
		Owner: conversation.Owner{
			AttemptID: request.AttemptID, RunID: request.RunID, LeaseID: string(request.LeaseID), FencingToken: int64(request.FencingToken),
			RunnerID: scope.credential.Runner.RunnerID, MachineID: string(scope.credential.Runner.MachineID), ThreadID: resume.ThreadID,
		},
		Capabilities: request.Capabilities, Resume: conversationResumeKind(resume), UpdatedAt: now,
	}
	if resume.ThreadID != "" && strings.TrimSpace(request.ThreadID) != "" {
		record.ProviderThreadID = resume.ThreadID
		record.ProviderThreadRunnerID = scope.credential.Runner.RunnerID
		record.ProviderThreadOrigin = conversation.ThreadOriginWorker
	}
	record.Execution = execution
	record.UpdatedAt = now
	var response conversationBindResponse
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, false); err != nil {
			return err
		}
		if _, _, err := s.authorizeConversationWorkerLeasePG(ctx, writer, scope, itemID, request.conversationWorkerIdentity, request.RunID, false); err != nil {
			return err
		}
		var stored conversationstore.Conversation
		if found {
			stored, err = writer.LockConversation(ctx, scope.project, record.ID)
			if err != nil {
				return err
			}
			if stored.Revision != record.Revision {
				return conversationStale("The conversation changed before the turn was bound")
			}
			previous := conversationRecordFromStore(stored)
			if !previous.Execution.Status.Terminal() && previous.Execution.Owner.AttemptID != "" && previous.Execution.Owner.AttemptID != request.AttemptID {
				if previous.Execution.Owner.LeaseID == string(request.LeaseID) {
					return conversationStale("The lease is already bound to another attempt")
				}
				if err := settlePGWorkerMessages(ctx, writer, &previous, conversation.ExecutionInterrupted, now, conversationPGWorkerPageSize); err != nil {
					return fmt.Errorf("settle previous conversation execution: %w", err)
				}
				if err := appendPGConversationEvent(ctx, writer, scope.organization, record.ID, conversation.EventExecutionUpdated, projectExecution(previous.Execution), now); err != nil {
					return fmt.Errorf("append previous conversation execution event: %w", err)
				}
				_, err = writer.Conversation(ctx, scope.project, record.ID)
				if err != nil {
					return fmt.Errorf("refresh conversation after settling previous execution: %w", err)
				}
			}
		} else {
			_, err = writer.CreateConversation(ctx, conversationRecordToStore(initialRecord))
			if err != nil {
				return err
			}
		}
		if err := writer.RecordStart(ctx, request.AttemptID, record.ID, now); err != nil {
			if errors.Is(err, corestore.ErrUniqueViolation) {
				return conversationStale("The attempt already started a conversation turn")
			}
			return err
		}
		pending, err := handoffPGBindControls(ctx, writer, scope, &record, pendingCandidates, pendingContent, now, c.config.ControlQueueSize)
		if err != nil {
			return err
		}
		stored, err = writer.Conversation(ctx, scope.project, record.ID)
		if err != nil {
			return fmt.Errorf("refresh conversation after binding controls: %w", err)
		}
		previousRevision := stored.Revision
		stored.Execution, stored.UpdatedAt = execution, now
		stored.ProviderThreadID = record.ProviderThreadID
		stored.ProviderThreadRunnerID = record.ProviderThreadRunnerID
		stored.ProviderThreadOrigin = record.ProviderThreadOrigin
		_, err = writer.UpdateConversation(ctx, stored, previousRevision)
		if err != nil {
			return err
		}
		eventBody, err := json.Marshal(projectExecution(execution))
		if err != nil {
			return err
		}
		if _, err := writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.organization, ConversationID: record.ID, Event: conversation.Event{Type: conversation.EventExecutionUpdated, Data: eventBody, CreatedAt: now}}); err != nil {
			return err
		}
		if execution.Resume == conversation.ResumeTranscript {
			notice := conversationstore.Message{OrganizationID: scope.organization, ConversationID: record.ID, ID: conversation.NewMessageID(), Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: conversationTranscriptNotice(len(resume.Transcript)), Delivery: conversation.DeliverySaved, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, Actor: conversationActorFor(scope), CreatedAt: now, UpdatedAt: now}
			created, err := writer.CreateMessage(ctx, notice)
			if err != nil {
				return err
			}
			if err := appendPGConversationEvent(ctx, writer, scope.organization, record.ID, conversation.EventMessageAccepted, projectMessage(conversationMessageFromStore(created)), now); err != nil {
				return err
			}
		}
		stored, err = writer.Conversation(ctx, scope.project, record.ID)
		if err != nil {
			return err
		}
		record = conversationRecordFromStore(stored)
		response = conversationBindResponse{ConversationID: record.ID, Resume: resume, Pending: pending, Preferences: record.Preferences.Normalized()}
		if len(pending) > 0 {
			response.Cursor = pending[len(pending)-1].Cursor
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(e, conversationStaleExecutionError("bind", record.ID, translateConversationError(err)))
	}
	c.committed(record)
	return e.JSON(http.StatusOK, response)
}

func handoffPGBindControls(ctx context.Context, writer tenantstore.Writer, scope nativeScope, record *conversationRecord, candidates []conversationstore.Message, content map[string]conversationContent, now time.Time, limit int) ([]conversationControl, error) {
	controls := make([]conversationControl, 0, min(limit, len(candidates)))
	if conversationBetweenTurns(record.Execution) {
		return controls, nil
	}
	for _, candidate := range candidates {
		if len(controls) >= limit {
			break
		}
		message, err := writer.Message(ctx, record.ID, candidate.ID)
		if err != nil {
			return nil, err
		}
		if message.Revision != candidate.Revision && message.ContentReference != candidate.ContentReference {
			return nil, conversationStale("A pending control changed before the turn was bound")
		}
		if message.Kind != conversation.MessageText && message.Kind != conversation.MessageContinue && message.Kind != conversation.MessageAnswer && message.Kind != conversation.MessageInterrupt {
			continue
		}
		savedContinue := message.Delivery == conversation.DeliverySaved && message.Kind == conversation.MessageContinue
		if message.Delivery != conversation.DeliveryQueued && message.Delivery != conversation.DeliverySending && !savedContinue {
			continue
		}
		control := conversationControl{Cursor: message.Sequence, Key: message.CommandKey, MessageID: message.ID, Expected: conversation.Expected{AttemptID: record.Execution.Owner.AttemptID, TurnID: record.Execution.Owner.TurnID}}
		payload := content[message.ID]
		switch message.Kind {
		case conversation.MessageText, conversation.MessageContinue:
			control.Kind, control.Text = "message", payload.Text
			if message.ContentReference == "" {
				control.Text = message.Text
			}
		case conversation.MessageInterrupt:
			control.Kind = "interrupt"
		case conversation.MessageAnswer:
			data := message.Data
			if len(data) == 0 || string(data) == "{}" {
				data = payload.Data
			}
			answer := decodeConversationAnswer(data)
			questionID := answer.QuestionID
			if questionID == "" && message.CommandKey != "" {
				command, found, err := writer.Command(ctx, record.ID, message.CommandKey)
				if err != nil {
					return nil, err
				}
				if found {
					questionID = command.Receipt.QuestionID
				}
			}
			question, err := writer.Question(ctx, record.ID, questionID)
			if err != nil || (question.Status != conversation.QuestionPending && question.Status != conversation.QuestionSending && question.Status != conversation.QuestionSent) {
				message.Delivery, message.UpdatedAt = conversation.DeliveryUnknown, now
				failure := &conversation.ReceiptError{Code: "unknown", Message: "The question is no longer available"}
				if err := updatePGWorkerMessage(ctx, writer, message, now, failure); err != nil {
					return nil, err
				}
				continue
			}
			control.Kind, control.QuestionID, control.RequestID = "answer", question.ID, question.RequestID
			control.Answers = question.Answers
			if len(control.Answers) == 0 {
				control.Answers = answer.Answers
			}
		}
		if message.Delivery != conversation.DeliverySending || message.AttemptID != record.Execution.Owner.AttemptID {
			message.AttemptID, message.ThreadID, message.TurnID = record.Execution.Owner.AttemptID, record.Execution.Owner.ThreadID, record.Execution.Owner.TurnID
			message.Delivery, message.UpdatedAt = conversation.DeliverySending, now
			if err := updatePGWorkerMessage(ctx, writer, message, now, nil); err != nil {
				return nil, err
			}
		}
		controls = append(controls, control)
	}
	return controls, nil
}

func (s *Service) conversationBindResumePG(ctx context.Context, scope nativeScope, record conversationRecord, request conversationBindRequest, pendingCandidates []conversationstore.Message) (conversationResumeResource, error) {
	thread := strings.TrimSpace(request.ThreadID)
	if thread == "" {
		thread = record.ProviderThreadID
	}
	decision := conversationResumeDecision{RunnerThreadID: request.ThreadID, RecordedRunnerID: record.ProviderThreadRunnerID, RecordedOrigin: record.ProviderThreadOrigin, BindingRunnerID: scope.credential.Runner.RunnerID}
	if decision.resumeThread() {
		return conversationResumeResource{ThreadID: thread, ThreadOrigin: conversation.ThreadOriginWorker}, nil
	}
	if record.ReadAudience != "" {
		return conversationResumeResource{}, nil
	}
	page, err := s.readPGConversationPage(ctx, scope, record.ID, 0, conversationTranscriptMessages+len(pendingCandidates))
	if err != nil {
		return conversationResumeResource{}, err
	}
	handed := make(map[string]struct{}, len(pendingCandidates))
	for _, message := range pendingCandidates {
		if message.Kind != conversation.MessageText && message.Kind != conversation.MessageContinue && message.Kind != conversation.MessageAnswer && message.Kind != conversation.MessageInterrupt {
			continue
		}
		if message.Delivery == conversation.DeliveryQueued || (message.Delivery == conversation.DeliverySaved && message.Kind == conversation.MessageContinue) || (message.Delivery == conversation.DeliverySending && record.Execution.Owner.AttemptID == request.AttemptID) {
			handed[message.ID] = struct{}{}
		}
	}
	transcript := make([]conversationTranscriptEntry, 0, len(page.messages))
	for _, message := range page.messages {
		if _, pending := handed[message.ID]; pending {
			continue
		}
		transcript = append(transcript, conversationTranscriptEntry{Role: message.Role, Kind: message.Kind, Text: boundRunes(message.Text, conversationTranscriptRunes)})
	}
	slices.Reverse(transcript)
	return conversationResumeResource{ThreadOrigin: conversation.ThreadOriginWorker, Transcript: transcript}, nil
}

func (r *conversationWorkerRouter) unbindPostgres(e echo.Context, request conversationUnbindRequest, status conversation.ExecutionStatus) error {
	c, s := r.service, r.service.server
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	itemID := e.Param("item")
	now := s.config.now()
	var record conversationRecord
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, false); err != nil {
			return fmt.Errorf("authorize conversation unbind access: %w", err)
		}
		stored, err := writer.ConversationByWorkItem(ctx, scope.project, itemID)
		if err != nil {
			return fmt.Errorf("read linked conversation for unbind: %w", err)
		}
		stored, err = writer.LockConversation(ctx, scope.project, stored.ID)
		if err != nil {
			return fmt.Errorf("lock conversation for unbind: %w", err)
		}
		record = conversationRecordFromStore(stored)
		if _, _, err := s.authorizeConversationWorkerLeasePG(ctx, writer, scope, itemID, request.conversationWorkerIdentity, "", true); err != nil {
			return fmt.Errorf("verify worker lease for unbind: %w", err)
		}
		if err := requireConversationRecordOwner(record, request.conversationWorkerIdentity, true); err != nil {
			return fmt.Errorf("verify conversation owner for unbind: %w", err)
		}
		if record.Execution.Settled {
			return nil
		}
		if err := settlePGWorkerMessages(ctx, writer, &record, status, now, conversationPGWorkerPageSize); err != nil {
			return fmt.Errorf("settle conversation worker messages: %w", err)
		}
		if err := expirePGOutstandingQuestions(ctx, writer, record.ID, record.Execution.Owner.AttemptID, "", func(question conversationstore.Question) error {
			question.Status, question.UpdatedAt = conversation.QuestionExpired, now
			if err := writer.UpsertQuestion(ctx, question); err != nil {
				return err
			}
			return appendPGConversationEvent(ctx, writer, scope.organization, record.ID, conversation.EventQuestionUpdated, question.Question, now)
		}); err != nil {
			return fmt.Errorf("expire conversation worker questions: %w", err)
		}
		execution := record.Execution
		execution.Status, execution.Error, execution.Settled = status, strings.TrimSpace(request.Error), true
		execution.Capabilities = conversation.Capabilities{}
		execution.UpdatedAt = now
		stored, err = writer.Conversation(ctx, scope.project, record.ID)
		if err != nil {
			return fmt.Errorf("refresh conversation event head before unbind: %w", err)
		}
		stored.Execution, stored.UpdatedAt = execution, now
		stored, err = writer.UpdateConversation(ctx, stored, stored.Revision)
		if err != nil {
			return fmt.Errorf("update conversation worker execution: %w", err)
		}
		if err := appendPGConversationEvent(ctx, writer, scope.organization, record.ID, conversation.EventExecutionUpdated, projectExecution(execution), now); err != nil {
			return fmt.Errorf("append conversation worker execution event: %w", err)
		}
		record = conversationRecordFromStore(stored)
		return nil
	})
	if err != nil {
		return s.nativeAPIError(e, conversationStaleExecutionError("unbind", record.ID, fmt.Errorf("write conversation unbind transaction: %w", translateConversationError(err))))
	}
	c.committed(record)
	return e.JSON(http.StatusOK, conversationUnbindResponse{ConversationID: record.ID, Execution: projectExecution(record.Execution)})
}

func expirePGOutstandingQuestions(ctx context.Context, writer conversationstore.Writer, conversationID, attemptID, turnID string, expire func(conversationstore.Question) error) error {
	after := ""
	for {
		page, err := writer.OutstandingQuestions(ctx, conversationID, attemptID, turnID, after, conversationPGWorkerPageSize)
		if err != nil {
			return err
		}
		for _, question := range page.Items {
			if err := expire(question); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		after = page.Next
	}
}

func settlePGWorkerMessages(ctx context.Context, writer tenantstore.Writer, record *conversationRecord, status conversation.ExecutionStatus, now time.Time, limit int) error {
	before := int64(0)
	for {
		responding, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, AttemptID: record.Execution.Owner.AttemptID, Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryResponding}, BeforeSequence: before, Limit: limit})
		if err != nil {
			return err
		}
		for _, message := range responding.Items {
			message.Delivery, message.UpdatedAt = conversationDeliveryForExecution(status), now
			if err := updatePGWorkerMessage(ctx, writer, message, now, nil); err != nil {
				return fmt.Errorf("settle responding conversation message: %w", err)
			}
		}
		if responding.Next == 0 {
			break
		}
		before = responding.Items[0].Sequence
	}
	before = 0
	for {
		controls, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliveryQueued, conversation.DeliverySending, conversation.DeliverySent}, BeforeSequence: before, Limit: limit})
		if err != nil {
			return err
		}
		for _, message := range controls.Items {
			switch {
			case message.Delivery == conversation.DeliverySent:
				message.Delivery = conversationDeliveryForExecution(status)
			case message.Kind == conversation.MessageAnswer || message.Kind == conversation.MessageInterrupt:
				message.Delivery = conversation.DeliveryUnknown
			case message.Delivery == conversation.DeliverySending:
				message.Delivery = conversation.DeliveryUnknown
			default:
				continue
			}
			message.UpdatedAt = now
			failure := (*conversation.ReceiptError)(nil)
			if message.Delivery == conversation.DeliveryUnknown {
				failure = &conversation.ReceiptError{Code: "unknown", Message: "The execution ended before the control could be established"}
			}
			if err := updatePGWorkerMessage(ctx, writer, message, now, failure); err != nil {
				return fmt.Errorf("settle conversation control message: %w", err)
			}
		}
		if controls.Next == 0 {
			break
		}
		before = controls.Items[0].Sequence
	}
	record.Execution.Status, record.Execution.Error, record.Execution.Settled = status, "", true
	record.Execution.Capabilities = conversation.Capabilities{}
	record.Execution.UpdatedAt = now
	return nil
}

func (c *conversationService) leaseReleasedTenant(ctx context.Context, writer tenantstore.Writer, leaseID string, fencingToken int64, _ string) error {
	stored, err := writer.ConversationByExecutionLease(ctx, leaseID)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	locked, err := writer.LockConversation(ctx, stored.ProjectID, stored.ID)
	if err != nil {
		return err
	}
	record := conversationRecordFromStore(locked)
	if record.Execution.Owner.LeaseID != leaseID || record.Execution.Owner.FencingToken != fencingToken || record.Execution.Settled {
		return nil
	}
	lease, err := writer.Lease(ctx, leaseID)
	if err != nil {
		return err
	}
	now := c.server.config.now()
	if lease.FencingToken != fencingToken || lease.ReleasedAt == nil || lease.RenewedAt.After(now) {
		return nil
	}
	status := record.Execution.Status
	if !status.Terminal() {
		status = conversation.ExecutionInterrupted
	}
	if err := settlePGWorkerMessages(ctx, writer, &record, status, now, conversationPGWorkerPageSize); err != nil {
		return fmt.Errorf("settle released conversation worker messages: %w", err)
	}
	if err := expirePGOutstandingQuestions(ctx, writer, record.ID, record.Execution.Owner.AttemptID, "", func(question conversationstore.Question) error {
		question.Status, question.UpdatedAt = conversation.QuestionExpired, now
		if err := writer.UpsertQuestion(ctx, question); err != nil {
			return err
		}
		return appendPGConversationEvent(ctx, writer, record.OrganizationID, record.ID, conversation.EventQuestionUpdated, question.Question, now)
	}); err != nil {
		return err
	}
	current, err := writer.Conversation(ctx, record.ProjectID, record.ID)
	if err != nil {
		return err
	}
	current.Execution, current.UpdatedAt = record.Execution, now
	updated, err := writer.UpdateConversation(ctx, current, current.Revision)
	if err != nil {
		return err
	}
	return appendPGConversationEvent(ctx, writer, record.OrganizationID, record.ID, conversation.EventExecutionUpdated, projectExecution(updated.Execution), now)
}

func updatePGWorkerMessage(ctx context.Context, writer tenantstore.Writer, message conversationstore.Message, now time.Time, failure *conversation.ReceiptError) error {
	updated, err := writer.UpdateMessage(ctx, message, message.Revision)
	if err != nil {
		return err
	}
	if err := appendPGConversationEvent(ctx, writer, message.OrganizationID, message.ConversationID, conversation.EventMessageUpdated, projectMessage(conversationMessageFromStore(updated)), now); err != nil {
		return err
	}
	if message.CommandKey == "" {
		return nil
	}
	command, found, err := writer.Command(ctx, message.ConversationID, message.CommandKey)
	if err != nil || !found {
		return err
	}
	command.Receipt.Status, command.Receipt.Error, command.Receipt.UpdatedAt = message.Delivery, failure, now
	if command.Receipt.MessageID == "" {
		command.Receipt.MessageID = message.ID
	}
	if err := writer.UpdateCommand(ctx, command); err != nil {
		return err
	}
	return appendPGConversationEvent(ctx, writer, message.OrganizationID, message.ConversationID, conversation.EventCommandReceipt, command.Receipt, now)
}

func (r *conversationWorkerRouter) turnEventsPostgres(e echo.Context, id string, request conversationTurnEventsRequest) error {
	c, s := r.service, r.service.server
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	request.BatchKey = strings.TrimSpace(request.BatchKey)
	if len(request.Events) > conversationMaxTurnEvents || len(request.BatchKey) > conversation.MaxCommandKeyBytes {
		return s.nativeAPIError(e, nativePublicInvalid("The turn event batch exceeds its limit"))
	}
	now := s.config.now()
	var current conversationRecord
	var storedSequence int64
	var foundBatch bool
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		stored, err := reader.Conversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		current = conversationRecordFromStore(stored)
		if _, err := s.authorizeConversationWorkerPG(ctx, reader, scope, current, request.conversationWorkerIdentity, "", false); err != nil {
			return err
		}
		if request.BatchKey != "" {
			storedSequence, foundBatch, err = reader.TurnBatch(ctx, request.AttemptID, request.BatchKey)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(e, conversationStaleExecutionError("turn_events", id, translateConversationError(err)))
	}
	if foundBatch {
		return e.JSON(http.StatusAccepted, conversationTurnEventsResponse{EventSeq: storedSequence})
	}
	plan := &conversationPublicationPlan{service: c, record: current, original: current, now: now, prepared: make(preparedConversationContents), reads: &conversationArchiveReads{content: map[string][]byte{}}}
	preparedItems := make([]conversationMessageRecord, len(request.Events))
	for index, event := range request.Events {
		if event.Type != "item" {
			continue
		}
		data, err := conversationItemData(event.Data)
		if err != nil {
			return s.nativeAPIError(e, err)
		}
		message := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageKind(event.Kind), Text: conversationTruncate(event.Summary, conversationMaxSummaryBytes), Data: data, Delivery: conversation.DeliveryCompleted, ProviderItemID: strings.TrimSpace(event.ProviderItemID), Actor: conversationActorFor(scope), CreatedAt: now, UpdatedAt: now}
		if err := preparePGConversationMessage(ctx, plan, &message); err != nil {
			return s.nativeAPIError(e, err)
		}
		preparedItems[index] = message
	}
	var result conversationTurnEventsResponse
	changed := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, false); err != nil {
			return err
		}
		stored, err := writer.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		record := conversationRecordFromStore(stored)
		originalExecution := record.Execution
		originalProviderThreadID := record.ProviderThreadID
		originalProviderThreadRunnerID := record.ProviderThreadRunnerID
		originalProviderThreadOrigin := record.ProviderThreadOrigin
		if _, err := s.authorizeConversationWorkerPG(ctx, writer, scope, record, request.conversationWorkerIdentity, "", false); err != nil {
			return err
		}
		if request.BatchKey != "" {
			sequence, exists, err := writer.TurnBatch(ctx, request.AttemptID, request.BatchKey)
			if err != nil {
				return err
			}
			if exists {
				result.EventSeq = sequence
				return nil
			}
		}
		if err := registerPreparedConversationObjects(ctx, writer, scope.organization, plan.prepared, now); err != nil {
			return err
		}
		execution := record.Execution
		for index, event := range request.Events {
			if err := validatePGTurnEvent(ctx, writer, record, execution, event); err != nil {
				return err
			}
			if err := applyPGTurnEvent(ctx, writer, scope, &record, &execution, event, preparedItems[index], now, c.config.QuestionTimeout); err != nil {
				return err
			}
		}
		latest, err := writer.Conversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		stateChanged := !reflect.DeepEqual(execution, originalExecution) || record.ProviderThreadID != originalProviderThreadID || record.ProviderThreadRunnerID != originalProviderThreadRunnerID || record.ProviderThreadOrigin != originalProviderThreadOrigin
		changed = stateChanged || latest.EventSequence != stored.EventSequence
		if stateChanged {
			execution.UpdatedAt = now
			latest.Execution, latest.UpdatedAt = execution, now
			latest.ProviderThreadID = record.ProviderThreadID
			latest.ProviderThreadRunnerID = record.ProviderThreadRunnerID
			latest.ProviderThreadOrigin = record.ProviderThreadOrigin
			_, err = writer.UpdateConversation(ctx, latest, latest.Revision)
			if err != nil {
				return err
			}
			if err := appendPGConversationEvent(ctx, writer, scope.organization, id, conversation.EventExecutionUpdated, projectExecution(execution), now); err != nil {
				return err
			}
			changed = true
		}
		latest, err = writer.Conversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		result.EventSeq = latest.EventSequence
		if request.BatchKey != "" {
			if err := writer.RecordTurnBatch(ctx, request.AttemptID, request.BatchKey, result.EventSeq, now); err != nil {
				return err
			}
		}
		record = conversationRecordFromStore(latest)
		return nil
	})
	if err != nil {
		return s.nativeAPIError(e, conversationStaleExecutionError("turn_events", id, translateConversationError(err)))
	}
	if changed {
		var record conversationRecord
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			record, err = readConversationRecordFromTenant(ctx, reader, scope.project, id)
			return err
		}); err != nil {
			return s.nativeAPIError(e, err)
		}
		c.committed(record)
	}
	return e.JSON(http.StatusAccepted, result)
}

func validatePGTurnEvent(ctx context.Context, reader tenantstore.Reader, record conversationRecord, execution conversation.Execution, event conversationTurnEvent) error {
	switch event.Type {
	case "execution_status":
		if !conversation.ExecutionStatus(event.Status).Valid() {
			return nativePublicInvalid("execution_status carries an unknown status")
		}
	case "turn_started":
		if strings.TrimSpace(event.TurnID) == "" {
			return nativePublicInvalid("turn_started requires a turn_id")
		}
		if execution.Owner.TurnID != "" && execution.Owner.TurnID != event.TurnID {
			return conversationStale("Another turn is still in progress")
		}
	case "delta":
		if strings.TrimSpace(event.ProviderItemID) == "" || len(event.Text) > conversationMaxDeltaBytes {
			return nativePublicInvalid("delta requires a provider_item_id and bounded text")
		}
	case "item":
		if event.Kind != string(conversation.MessageTool) && event.Kind != string(conversation.MessageStatus) {
			return nativePublicInvalid("item kind must be tool or status")
		}
		_, err := conversationItemData(event.Data)
		return err
	case "question_opened":
		if strings.TrimSpace(event.RequestID) == "" {
			return nativePublicInvalid("question_opened requires a request_id and prompts")
		}
		if err := conversation.ValidatePrompts(event.Prompts); err != nil {
			return nativeInvalid(err.Error())
		}
		if event.TurnID != "" && execution.Owner.TurnID != "" && event.TurnID != execution.Owner.TurnID {
			return conversationStale("The question belongs to another turn")
		}
	case "control_result":
		if strings.TrimSpace(event.Key) == "" {
			return nativePublicInvalid("control_result requires a key")
		}
		message, found, err := reader.MessageByCommandKey(ctx, record.ID, event.Key)
		if err != nil {
			return err
		}
		if !found {
			return nativeInvalid("Unknown control key " + fmt.Sprintf("%q", event.Key))
		}
		if message.AttemptID != execution.Owner.AttemptID {
			return conversationStale("The control was handed to another attempt")
		}
		switch event.Status {
		case "delivered", "rejected", "unknown":
		default:
			return nativePublicInvalid("control_result status must be delivered, rejected or unknown")
		}
	case "turn_completed":
		if strings.TrimSpace(event.TurnID) == "" || (execution.Owner.TurnID != "" && execution.Owner.TurnID != event.TurnID) {
			return conversationStale("The completed turn is not the current turn")
		}
		switch event.Status {
		case "completed", "interrupted", "failed":
		default:
			return nativePublicInvalid("turn_completed status must be completed, interrupted or failed")
		}
	default:
		return nativeInvalid("Unknown turn event type " + fmt.Sprintf("%q", event.Type))
	}
	return nil
}

func applyPGTurnEvent(ctx context.Context, writer tenantstore.Writer, scope nativeScope, record *conversationRecord, execution *conversation.Execution, event conversationTurnEvent, preparedItem conversationMessageRecord, now time.Time, questionTimeout time.Duration) error {
	appendEvent := func(eventType conversation.EventType, body any) error {
		return appendPGConversationEvent(ctx, writer, scope.organization, record.ID, eventType, body, now)
	}
	updateMessage := func(message conversationstore.Message, failure *conversation.ReceiptError) error {
		message.UpdatedAt = now
		return updatePGWorkerMessage(ctx, writer, message, now, failure)
	}
	switch event.Type {
	case "execution_status":
		execution.Status, execution.Error = conversation.ExecutionStatus(event.Status), strings.TrimSpace(event.Error)
	case "turn_started":
		if thread := strings.TrimSpace(event.ThreadID); thread != "" {
			execution.Owner.ThreadID = thread
			record.ProviderThreadID, record.ProviderThreadRunnerID, record.ProviderThreadOrigin = thread, scope.credential.Runner.RunnerID, conversation.ThreadOriginWorker
		}
		execution.Owner.TurnID, execution.Status, execution.Error = strings.TrimSpace(event.TurnID), conversation.ExecutionRunning, ""
		prompted, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, AttemptID: execution.Owner.AttemptID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliverySending, conversation.DeliverySent}, Limit: conversationPGWorkerPageSize})
		if err != nil {
			return err
		}
		if prompted.Next != 0 {
			return conversationUnsupported("The turn control set exceeds the bounded event batch")
		}
		for _, message := range prompted.Items {
			if message.Kind != conversation.MessageText && message.Kind != conversation.MessageContinue {
				continue
			}
			message.ThreadID, message.TurnID, message.Delivery = execution.Owner.ThreadID, execution.Owner.TurnID, conversation.DeliveryDelivered
			if err := updateMessage(message, nil); err != nil {
				return err
			}
		}
	case "delta":
		messages, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, AttemptID: execution.Owner.AttemptID, Role: conversation.RoleAssistant, Limit: conversationPGWorkerPageSize})
		if err != nil {
			return err
		}
		if messages.Next != 0 {
			return conversationUnsupported("The turn message set exceeds the bounded event batch")
		}
		var message conversationstore.Message
		for _, candidate := range messages.Items {
			if (event.MessageID != "" && candidate.ID == event.MessageID) || (event.MessageID == "" && candidate.ProviderItemID == event.ProviderItemID) {
				message = candidate
				break
			}
		}
		if message.ID == "" {
			message, err = writer.CreateMessage(ctx, conversationMessageToStore(scope.organization, conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, TurnID: execution.Owner.TurnID, ProviderItemID: strings.TrimSpace(event.ProviderItemID), Actor: conversationActorFor(scope), CreatedAt: now, UpdatedAt: now}))
			if err != nil {
				return err
			}
			if err := appendEvent(conversation.EventMessageAccepted, projectMessage(conversationMessageFromStore(message))); err != nil {
				return err
			}
		}
		if event.Text != "" {
			offset := message.TextBytes
			message.TextBytes += int64(len(event.Text))
			message.Text += event.Text
			message.StoredTextBytes = int64(len(message.Text))
			message.UpdatedAt = now
			updated, err := writer.UpdateMessage(ctx, message, message.Revision)
			if err != nil {
				return err
			}
			fact := conversationDeltaFact{Text: event.Text, Revision: updated.Revision, MessageID: message.ID, Offset: offset, Bytes: len(event.Text)}
			if err := appendEvent(conversation.EventMessageDelta, fact); err != nil {
				return err
			}
		}
	case "item":
		preparedItem.AttemptID, preparedItem.ThreadID, preparedItem.TurnID = execution.Owner.AttemptID, execution.Owner.ThreadID, execution.Owner.TurnID
		created, err := writer.CreateMessage(ctx, conversationMessageToStore(scope.organization, preparedItem))
		if err != nil {
			return err
		}
		preparedItem.Seq, preparedItem.Revision = created.Sequence, created.Revision
		return appendEvent(conversation.EventMessageAccepted, projectMessage(preparedItem))
	case "question_opened":
		questions, err := writer.Questions(ctx, record.ID, execution.Owner.AttemptID, conversationPGWorkerPageSize)
		if err != nil {
			return err
		}
		for _, question := range questions {
			if question.RequestID == strings.TrimSpace(event.RequestID) {
				return nil
			}
		}
		turnID := strings.TrimSpace(event.TurnID)
		if turnID == "" {
			turnID = execution.Owner.TurnID
		}
		messageRecord := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageStatus, Text: conversationQuestionPrompt, Delivery: conversation.DeliveryCompleted, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, TurnID: turnID, Actor: conversationActorFor(scope), CreatedAt: now, UpdatedAt: now}
		message, err := writer.CreateMessage(ctx, conversationMessageToStore(scope.organization, messageRecord))
		if err != nil {
			return err
		}
		messageRecord.Seq, messageRecord.Revision = message.Sequence, message.Revision
		if err := appendEvent(conversation.EventMessageAccepted, projectMessage(messageRecord)); err != nil {
			return err
		}
		questionOwner := execution.Owner
		questionOwner.TurnID = turnID
		if event.ThreadID != "" {
			questionOwner.ThreadID = event.ThreadID
		}
		question := conversationstore.Question{OrganizationID: scope.organization, ConversationID: record.ID, Question: conversation.Question{ID: conversationQuestionID(ctx), ConversationID: record.ID, MessageID: message.ID, Status: conversation.QuestionPending, Owner: questionOwner, Prompts: event.Prompts, ExpiresAt: timePtr(now.Add(questionTimeout)), CreatedAt: now, UpdatedAt: now}, RequestID: strings.TrimSpace(event.RequestID)}
		if err := writer.UpsertQuestion(ctx, question); err != nil {
			return err
		}
		if err := appendEvent(conversation.EventQuestionOpened, question.Question); err != nil {
			return err
		}
		execution.Status = conversation.ExecutionWaitingInput
	case "control_result":
		message, found, err := writer.MessageByCommandKey(ctx, record.ID, strings.TrimSpace(event.Key))
		if err != nil {
			return err
		}
		if !found {
			return nativeInvalid("Unknown control key")
		}
		var delivery conversation.Delivery
		var failure *conversation.ReceiptError
		var questionStatus conversation.QuestionStatus
		switch event.Status {
		case "delivered":
			delivery, questionStatus = conversation.DeliveryDelivered, conversation.QuestionAnswered
			if message.Kind == conversation.MessageAnswer {
				delivery = conversation.DeliverySent
			}
		case "rejected":
			delivery, questionStatus = conversation.DeliveryRejected, conversation.QuestionExpired
			failure = &conversation.ReceiptError{Code: "rejected", Message: strings.TrimSpace(event.Error)}
		case "unknown":
			delivery, questionStatus = conversation.DeliveryUnknown, conversation.QuestionUnknown
			failure = &conversation.ReceiptError{Code: "unknown", Message: strings.TrimSpace(event.Error)}
		}
		message.Delivery = delivery
		if err := updateMessage(message, failure); err != nil {
			return err
		}
		if message.Kind == conversation.MessageAnswer {
			command, _, err := writer.Command(ctx, record.ID, message.CommandKey)
			if err != nil {
				return err
			}
			question, err := writer.Question(ctx, record.ID, command.Receipt.QuestionID)
			if err != nil {
				return err
			}
			if question.Status == conversation.QuestionPending || question.Status == conversation.QuestionSending || question.Status == conversation.QuestionSent {
				question.Status, question.UpdatedAt = questionStatus, now
				if questionStatus == conversation.QuestionAnswered && question.AnsweredBy == "" {
					question.AnsweredBy = message.Actor.PrincipalID
				}
				if err := writer.UpsertQuestion(ctx, question); err != nil {
					return err
				}
				if err := appendEvent(conversation.EventQuestionUpdated, question.Question); err != nil {
					return err
				}
			}
			if questionStatus == conversation.QuestionAnswered && execution.Status == conversation.ExecutionWaitingInput {
				questions, err := writer.OutstandingQuestions(ctx, record.ID, "", "", "", 1)
				if err != nil {
					return err
				}
				if len(questions.Items) == 0 {
					execution.Status = conversation.ExecutionRunning
				}
			}
		}
	case "turn_completed":
		delivery := map[string]conversation.Delivery{"completed": conversation.DeliveryCompleted, "interrupted": conversation.DeliveryInterrupted, "failed": conversation.DeliveryFailed}[event.Status]
		responding, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, AttemptID: execution.Owner.AttemptID, TurnID: strings.TrimSpace(event.TurnID), Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryResponding}, Limit: conversationPGWorkerPageSize})
		if err != nil {
			return err
		}
		if responding.Next != 0 {
			return conversationUnsupported("The turn message set exceeds the bounded event batch")
		}
		for _, message := range responding.Items {
			message.Delivery = delivery
			if err := updateMessage(message, nil); err != nil {
				return err
			}
		}
		if err := expirePGOutstandingQuestions(ctx, writer, record.ID, execution.Owner.AttemptID, event.TurnID, func(question conversationstore.Question) error {
			question.Status, question.UpdatedAt = conversation.QuestionExpired, now
			if err := writer.UpsertQuestion(ctx, question); err != nil {
				return err
			}
			return appendEvent(conversation.EventQuestionUpdated, question.Question)
		}); err != nil {
			return err
		}
		execution.Owner.TurnID = ""
		switch delivery {
		case conversation.DeliveryInterrupted:
			execution.Status = conversation.ExecutionInterrupted
		case conversation.DeliveryFailed:
			execution.Status = conversation.ExecutionFailed
		default:
			execution.Status = conversation.ExecutionRunning
		}
		execution.Error = strings.TrimSpace(event.Error)
	}
	return nil
}

func timePtr(value time.Time) *time.Time {
	return &value
}

func (r *conversationWorkerRouter) controlsPostgres(e echo.Context, id string, identity conversationWorkerIdentity, after int64, wait time.Duration) error {
	c, s := r.service, r.service.server
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	subscription, cancel := c.broker.subscribe(id)
	defer cancel()
	var deadline <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		page, err := r.pollControlsPostgres(ctx, scope, id, identity, after)
		if err != nil {
			return s.nativeAPIError(e, conversationStaleExecutionError("controls", id, translateConversationError(err)))
		}
		if len(page.Controls) > 0 || wait == 0 {
			return e.JSON(http.StatusOK, page)
		}
		select {
		case <-subscription.closed:
		case <-r.stop:
			return e.JSON(http.StatusOK, page)
		case <-deadline:
			return e.JSON(http.StatusOK, page)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *conversationWorkerRouter) pollControlsPostgres(ctx context.Context, scope nativeScope, id string, identity conversationWorkerIdentity, after int64) (conversationControlsResponse, error) {
	page := conversationControlsResponse{Controls: []conversationControl{}, Cursor: after}
	const limit = 128
	var snapshot conversationRecord
	var candidates []conversationstore.Message
	contentReferences := make(map[string]attachment.ArchiveReference)
	content := make(map[string]conversationContent)
	err := r.service.server.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := r.service.server.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		stored, err := reader.Conversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		snapshot = conversationRecordFromStore(stored)
		if _, err := r.service.server.authorizeConversationWorkerPG(ctx, reader, scope, snapshot, identity, "", false); err != nil {
			return err
		}
		messages, err := reader.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: id, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliveryQueued, conversation.DeliverySending, conversation.DeliverySaved}, Limit: limit})
		if err != nil {
			return err
		}
		candidates = messages.Items
		for _, message := range candidates {
			if message.ContentReference == "" {
				continue
			}
			object, err := reader.ConversationContent(ctx, message.ContentReference)
			if err != nil {
				return err
			}
			contentReferences[message.ID] = object.Reference
		}
		return nil
	})
	if err != nil {
		return page, err
	}
	for messageID, reference := range contentReferences {
		var payload conversationContent
		if err := r.service.server.readConversationObject(ctx, string(scope.organization), reference, &payload); err != nil {
			return page, err
		}
		content[messageID] = payload
	}
	changed := false
	err = r.service.server.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := r.service.server.authorizeAccessScope(ctx, writer, scope, false); err != nil {
			return err
		}
		stored, err := writer.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		record := conversationRecordFromStore(stored)
		if stored.Revision != snapshot.Revision || stored.Execution.Owner.AttemptID != snapshot.Execution.Owner.AttemptID {
			return conversationStale("The conversation changed; retry the control poll")
		}
		if _, err := r.service.server.authorizeConversationWorkerPG(ctx, writer, scope, record, identity, "", false); err != nil {
			return err
		}
		acknowledged, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: id, AttemptID: identity.AttemptID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliverySending}, BeforeSequence: after + 1, Limit: limit})
		if err != nil {
			return err
		}
		for _, message := range acknowledged.Items {
			message.Delivery, message.UpdatedAt = conversation.DeliverySent, r.service.server.config.now()
			if err := updatePGWorkerMessage(ctx, writer, message, message.UpdatedAt, nil); err != nil {
				return err
			}
			changed = true
		}
		if conversationBetweenTurns(record.Execution) {
			return nil
		}
		messages, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: id, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliveryQueued, conversation.DeliverySending, conversation.DeliverySaved}, Limit: limit})
		if err != nil {
			return err
		}
		for _, storedMessage := range messages.Items {
			message := storedMessage
			if message.Kind != conversation.MessageText && message.Kind != conversation.MessageContinue && message.Kind != conversation.MessageAnswer && message.Kind != conversation.MessageInterrupt {
				continue
			}
			savedContinue := message.Delivery == conversation.DeliverySaved && message.Kind == conversation.MessageContinue
			if message.Sequence <= after && message.Delivery != conversation.DeliveryQueued && !savedContinue {
				continue
			}
			if message.Delivery != conversation.DeliveryQueued && message.Delivery != conversation.DeliverySending && !savedContinue {
				continue
			}
			if len(page.Controls) >= r.service.config.ControlQueueSize {
				break
			}
			payload := content[message.ID]
			control := conversationControl{Cursor: message.Sequence, Key: message.CommandKey, MessageID: message.ID, Expected: conversation.Expected{AttemptID: record.Execution.Owner.AttemptID, TurnID: record.Execution.Owner.TurnID}}
			switch message.Kind {
			case conversation.MessageText, conversation.MessageContinue:
				control.Kind, control.Text = "message", payload.Text
			case conversation.MessageInterrupt:
				control.Kind = "interrupt"
			case conversation.MessageAnswer:
				data := message.Data
				if len(data) == 0 || string(data) == "{}" {
					data = payload.Data
				}
				answer := decodeConversationAnswer(data)
				questionID := answer.QuestionID
				if questionID == "" && message.CommandKey != "" {
					command, found, err := writer.Command(ctx, id, message.CommandKey)
					if err != nil {
						return err
					}
					if found {
						questionID = command.Receipt.QuestionID
					}
				}
				question, err := writer.Question(ctx, id, questionID)
				if err != nil || (question.Status != conversation.QuestionPending && question.Status != conversation.QuestionSending && question.Status != conversation.QuestionSent) {
					message.Delivery, message.UpdatedAt = conversation.DeliveryUnknown, r.service.server.config.now()
					failure := &conversation.ReceiptError{Code: "unknown", Message: "The question is no longer available"}
					if err := updatePGWorkerMessage(ctx, writer, message, message.UpdatedAt, failure); err != nil {
						return err
					}
					changed = true
					continue
				}
				control.Kind, control.QuestionID, control.RequestID = "answer", question.ID, question.RequestID
				control.Answers = question.Answers
				if len(control.Answers) == 0 {
					control.Answers = answer.Answers
				}
			}
			if message.Delivery != conversation.DeliverySending || message.AttemptID != record.Execution.Owner.AttemptID {
				message.AttemptID, message.ThreadID, message.TurnID = record.Execution.Owner.AttemptID, record.Execution.Owner.ThreadID, record.Execution.Owner.TurnID
				message.Delivery, message.UpdatedAt = conversation.DeliverySending, r.service.server.config.now()
				if err := updatePGWorkerMessage(ctx, writer, message, message.UpdatedAt, nil); err != nil {
					return err
				}
				changed = true
			}
			page.Controls = append(page.Controls, control)
		}
		if len(page.Controls) > 0 {
			page.Cursor = page.Controls[len(page.Controls)-1].Cursor
		}
		return nil
	})
	if err != nil {
		return page, err
	}
	if changed {
		var record conversationRecord
		if err := r.service.server.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			record, err = readConversationRecordFromTenant(ctx, reader, scope.project, id)
			return err
		}); err != nil {
			return page, err
		}
		r.service.committed(record)
	}
	return page, nil
}
