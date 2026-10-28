package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/conversation"
)

func (p *conversationPublicationPlan) updateQuestion(ctx context.Context, question conversationQuestionRecord, status conversation.QuestionStatus) error {
	ctx = p.context(ctx)
	question.Status = status
	question.UpdatedAt = p.now
	p.rememberQuestion(question)
	return p.event(ctx, question.Question)
}

func (p *conversationPublicationPlan) rememberQuestion(question conversationQuestionRecord) {
	if p.questions == nil {
		p.questions = map[string]conversationQuestionRecord{}
	}
	p.questions[question.ID] = question
}

func (p *conversationPublicationPlan) pendingQuestions(ctx context.Context) ([]conversationQuestionRecord, error) {
	ctx = p.context(ctx)
	stored, err := p.service.store.listPendingQuestions(ctx, p.service.store.reader, p.record.ID)
	if err != nil {
		return nil, err
	}
	questions := map[string]conversationQuestionRecord{}
	for _, question := range stored {
		questions[question.ID] = question
	}
	for id, question := range p.questions {
		questions[id] = question
	}
	pending := make([]conversationQuestionRecord, 0, len(questions))
	for _, question := range questions {
		switch question.Status {
		case conversation.QuestionPending, conversation.QuestionSending, conversation.QuestionSent:
			pending = append(pending, question)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})
	return pending, nil
}

func (p *conversationPublicationPlan) pendingControls(ctx context.Context, after int64) ([]conversationMessageRecord, error) {
	ctx = p.context(ctx)
	messages, err := p.service.pendingControlMessages(ctx, p.service.store.reader, p.record.ID, after)
	if err != nil {
		return nil, err
	}
	pending := messages[:0]
	for _, message := range messages {
		message = p.currentMessage(message)
		if message.Seq <= after && message.Delivery != conversation.DeliveryQueued {
			continue
		}
		if message.Delivery != conversation.DeliveryQueued && message.Delivery != conversation.DeliverySending && (message.Delivery != conversation.DeliverySaved || message.Kind != conversation.MessageContinue) {
			continue
		}
		pending = append(pending, message)
	}
	return pending, nil
}

func (p *conversationPublicationPlan) expireQuestions(ctx context.Context, turnID string) error {
	ctx = p.context(ctx)
	pending, err := p.pendingQuestions(ctx)
	if err != nil {
		return err
	}
	for _, question := range pending {
		if turnID == "" || question.Owner.TurnID == turnID {
			if err := p.updateQuestion(ctx, question, conversation.QuestionExpired); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *conversationPublicationPlan) finishExecution(ctx context.Context, status conversation.ExecutionStatus, failure string, settled bool) error {
	ctx = p.context(ctx)
	c := p.service
	execution := p.record.Execution
	responding, err := c.queryMessages(ctx, c.store.reader, conversationMessageQuery+"conversation_id = ? AND attempt_id = ? AND delivery = 'responding' ORDER BY seq", p.record.ID, execution.Owner.AttemptID)
	if err != nil {
		return err
	}
	for _, message := range responding {
		message = p.currentMessage(message)
		if message.Delivery != conversation.DeliveryResponding {
			continue
		}
		message.Delivery = conversationDeliveryForExecution(status)
		if err := p.updateMessage(ctx, message); err != nil {
			return err
		}
	}
	controls, err := c.queryMessages(ctx, c.store.reader, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND delivery IN ('queued', 'sending', 'sent') ORDER BY seq", p.record.ID)
	if err != nil {
		return err
	}
	for _, message := range controls {
		message = p.currentMessage(message)
		if message.Delivery != conversation.DeliveryQueued && message.Delivery != conversation.DeliverySending && message.Delivery != conversation.DeliverySent {
			continue
		}
		delivery := conversation.DeliveryUnknown
		var receiptError *conversation.ReceiptError
		switch {
		case message.Delivery == conversation.DeliverySent:
			delivery = conversationDeliveryForExecution(status)
		case message.Kind == conversation.MessageAnswer || message.Kind == conversation.MessageInterrupt:
			receiptError = &conversation.ReceiptError{Code: "unknown", Message: "The execution ended before the control was delivered"}
		case message.Delivery == conversation.DeliverySending:
			receiptError = &conversation.ReceiptError{Code: "unknown", Message: "The execution ended before the control could be established"}
		default:
			continue
		}
		if err := p.setControlDelivery(ctx, message, delivery, receiptError); err != nil {
			return err
		}
	}
	if err := p.expireQuestions(ctx, ""); err != nil {
		return err
	}
	execution.Status, execution.Error, execution.Settled = status, failure, settled
	execution.Capabilities = conversation.Capabilities{}
	return p.updateExecution(ctx, execution)
}

func (p *conversationPublicationPlan) handOffControls(ctx context.Context, after int64) error {
	ctx = p.context(ctx)
	c := p.service
	if conversationBetweenTurns(p.record.Execution) {
		return nil
	}
	messages, err := p.pendingControls(ctx, after)
	if err != nil {
		return err
	}
	owner := p.record.Execution.Owner
	accepted := 0
	for _, message := range messages {
		if accepted >= c.config.ControlQueueSize {
			break
		}
		if message.Kind == conversation.MessageAnswer {
			if _, _, err := c.resolveAnswer(ctx, c.store.reader, p.record.ID, message); err != nil {
				if err := p.setControlDelivery(ctx, message, conversation.DeliveryUnknown, &conversation.ReceiptError{Code: "unknown", Message: "The question is no longer available"}); err != nil {
					return err
				}
				continue
			}
		}
		if message.Delivery != conversation.DeliverySending || message.AttemptID != owner.AttemptID {
			message.AttemptID, message.ThreadID, message.TurnID = owner.AttemptID, owner.ThreadID, owner.TurnID
			if err := p.setControlDelivery(ctx, message, conversation.DeliverySending, nil); err != nil {
				return err
			}
		}
		accepted++
	}
	return nil
}

func (p *conversationPublicationPlan) turnEvents(ctx context.Context, scope nativeScope, events []conversationTurnEvent) error {
	ctx = p.context(ctx)
	c := p.service
	execution := p.record.Execution
	changed := false
	messages := map[string]*conversationMessageRecord{}
	byItem := map[string]*conversationMessageRecord{}
	baseBytes := map[string]int64{}
	load := func(event conversationTurnEvent) (*conversationMessageRecord, error) {
		if event.MessageID != "" {
			if message := messages[event.MessageID]; message != nil {
				return message, nil
			}
		}
		condition, value := "provider_item_id", strings.TrimSpace(event.ProviderItemID)
		if event.MessageID != "" {
			condition, value = "id", event.MessageID
		}
		message := &conversationMessageRecord{ConversationID: p.record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, AttemptID: execution.Owner.AttemptID}
		err := c.store.reader.QueryRowContext(ctx, "SELECT id,COALESCE(text_bytes,length(CAST(text AS BLOB))),revision,turn_id,provider_item_id,delivery FROM conversation_messages WHERE conversation_id=? AND role='assistant' AND attempt_id=? AND "+condition+"=? ORDER BY seq LIMIT 1", p.record.ID, execution.Owner.AttemptID, value).Scan(&message.ID, &message.TextBytes, &message.Revision, &message.TurnID, &message.ProviderItemID, &message.Delivery)
		if errors.Is(err, sql.ErrNoRows) {
			if event.MessageID == "" {
				if current := byItem[strings.TrimSpace(event.ProviderItemID)]; current != nil {
					return current, nil
				}
			}
			return nil, sql.ErrNoRows
		}
		if err != nil {
			return nil, err
		}
		if current := messages[message.ID]; current != nil {
			return current, nil
		}
		if p.revisions == nil {
			p.revisions = map[string]int64{}
		}
		p.revisions[message.ID] = message.Revision
		baseBytes[message.ID] = message.TextBytes
		messages[message.ID] = message
		return message, nil
	}
	for _, event := range events {
		switch event.Type {
		case "execution_status":
			execution.Status = conversation.ExecutionStatus(event.Status)
			if !execution.Status.Valid() {
				return nativePublicInvalid("execution_status carries an unknown status")
			}
			execution.Error, changed = strings.TrimSpace(event.Error), true
		case "turn_started":
			if thread := strings.TrimSpace(event.ThreadID); thread != "" {
				execution.Owner.ThreadID = thread
				p.record.ProviderThreadID, p.record.ProviderThreadRunnerID, p.record.ProviderThreadOrigin = thread, scope.credential.Runner.RunnerID, conversation.ThreadOriginWorker
			}
			execution.Owner.TurnID, execution.Status, execution.Error = strings.TrimSpace(event.TurnID), conversation.ExecutionRunning, ""
			changed = true
			prompted, err := c.queryMessages(ctx, c.store.reader, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND attempt_id = ? AND delivery IN ('sending', 'sent') AND kind IN ('text', 'continue') ORDER BY seq", p.record.ID, execution.Owner.AttemptID)
			if err != nil {
				return err
			}
			for _, message := range prompted {
				message = p.currentMessage(message)
				if message.Delivery != conversation.DeliverySending && message.Delivery != conversation.DeliverySent {
					continue
				}
				message.ThreadID, message.TurnID = execution.Owner.ThreadID, execution.Owner.TurnID
				if err := p.setControlDelivery(ctx, message, conversation.DeliveryDelivered, nil); err != nil {
					return err
				}
			}
		case "delta":
			message, err := load(event)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if message == nil {
				message = &conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, TurnID: execution.Owner.TurnID, ProviderItemID: strings.TrimSpace(event.ProviderItemID), Actor: conversationActorFor(scope)}
				if err := p.appendMessage(ctx, message); err != nil {
					return err
				}
				messages[message.ID] = message
				if byItem[message.ProviderItemID] == nil {
					byItem[message.ProviderItemID] = message
				}
			}
			if event.Text != "" {
				if err := p.appendDeltaAt(ctx, *message, message.TextBytes, event.Text); err != nil {
					return err
				}
				message.Text += event.Text
				message.TextBytes += int64(len(event.Text))
			}
		case "item":
			data, err := conversationItemData(event.Data)
			if err != nil {
				return err
			}
			message := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageKind(event.Kind), Text: conversationTruncate(event.Summary, conversationMaxSummaryBytes), Data: data, Delivery: conversation.DeliveryCompleted, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, TurnID: execution.Owner.TurnID, ProviderItemID: strings.TrimSpace(event.ProviderItemID), Actor: conversationActorFor(scope)}
			if err := p.appendMessage(ctx, &message); err != nil {
				return err
			}
		case "question_opened":
			duplicate := false
			for _, question := range p.questions {
				if question.Owner.AttemptID == execution.Owner.AttemptID && question.RequestID == strings.TrimSpace(event.RequestID) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			var existing string
			err := c.store.reader.QueryRowContext(ctx, "SELECT id FROM conversation_questions WHERE conversation_id=? AND attempt_id=? AND request_id=?", p.record.ID, execution.Owner.AttemptID, strings.TrimSpace(event.RequestID)).Scan(&existing)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			turnID := strings.TrimSpace(event.TurnID)
			if turnID == "" {
				turnID = execution.Owner.TurnID
			}
			message := conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageStatus, Text: conversationQuestionPrompt, Delivery: conversation.DeliveryCompleted, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, TurnID: turnID, Actor: conversationActorFor(scope)}
			if err := p.appendMessage(ctx, &message); err != nil {
				return err
			}
			id := conversation.NewQuestionID()
			p.questionIDs = append(p.questionIDs, id)
			expires := p.now.Add(c.config.QuestionTimeout)
			owner := execution.Owner
			owner.TurnID = turnID
			if thread := strings.TrimSpace(event.ThreadID); thread != "" {
				owner.ThreadID = thread
			}
			question := conversation.Question{ID: id, ConversationID: p.record.ID, MessageID: message.ID, Status: conversation.QuestionPending, Owner: owner, Prompts: event.Prompts, ExpiresAt: &expires, CreatedAt: p.now, UpdatedAt: p.now}
			p.rememberQuestion(conversationQuestionRecord{Question: question, RequestID: strings.TrimSpace(event.RequestID)})
			if err := p.event(ctx, question); err != nil {
				return err
			}
			execution.Status, changed = conversation.ExecutionWaitingInput, true
		case "control_result":
			rows, err := c.queryMessages(ctx, c.store.reader, conversationMessageQuery+"conversation_id=? AND role='user' AND command_key=? ORDER BY seq", p.record.ID, strings.TrimSpace(event.Key))
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nativeInvalid("Unknown control key")
			}
			message := p.currentMessage(rows[0])
			delivery, status := conversation.DeliveryDelivered, conversation.QuestionAnswered
			var failure *conversation.ReceiptError
			switch event.Status {
			case "delivered":
				if message.Kind == conversation.MessageAnswer {
					delivery = conversation.DeliverySent
				}
			case "rejected":
				delivery, status = conversation.DeliveryRejected, conversation.QuestionExpired
				failure = &conversation.ReceiptError{Code: "rejected", Message: strings.TrimSpace(event.Error)}
			case "unknown":
				delivery, status = conversation.DeliveryUnknown, conversation.QuestionUnknown
				failure = &conversation.ReceiptError{Code: "unknown", Message: strings.TrimSpace(event.Error)}
			default:
				return nativePublicInvalid("control_result status must be delivered, rejected or unknown")
			}
			if err := p.setControlDelivery(ctx, message, delivery, failure); err != nil {
				return err
			}
			if message.Kind != conversation.MessageAnswer {
				continue
			}
			question, _, err := c.resolveAnswer(ctx, c.store.reader, p.record.ID, message)
			if err != nil {
				var missing *nativeError
				if errors.As(err, &missing) && missing.status == http.StatusNotFound {
					continue
				}
				return err
			}
			if current, ok := p.questions[question.ID]; ok {
				question = current
			}
			switch question.Status {
			case conversation.QuestionPending, conversation.QuestionSending, conversation.QuestionSent:
				if status == conversation.QuestionAnswered && question.AnsweredBy == "" {
					question.AnsweredBy = message.Actor.PrincipalID
				}
				if err := p.updateQuestion(ctx, question, status); err != nil {
					return err
				}
			}
			if status == conversation.QuestionAnswered && execution.Status == conversation.ExecutionWaitingInput {
				pending, err := p.pendingQuestions(ctx)
				if err != nil {
					return err
				}
				if len(pending) == 0 {
					execution.Status, changed = conversation.ExecutionRunning, true
				}
			}
		case "turn_completed":
			responding, err := c.queryMessages(ctx, c.store.reader, conversationMessageQuery+"conversation_id=? AND role='assistant' AND attempt_id=? AND turn_id=? AND delivery='responding' ORDER BY seq", p.record.ID, execution.Owner.AttemptID, strings.TrimSpace(event.TurnID))
			if err != nil {
				return err
			}
			for index := range responding {
				message := &responding[index]
				if current := messages[message.ID]; current != nil {
					if current.Delivery != conversation.DeliveryResponding {
						continue
					}
					if _, exists := baseBytes[message.ID]; exists {
						message.Text += current.Text
						message.TextBytes = current.TextBytes
						delete(baseBytes, message.ID)
					}
				}
				messages[message.ID], byItem[message.ProviderItemID] = message, message
			}
			delivery := conversation.Delivery(event.Status)
			for _, message := range messages {
				if message.TurnID == strings.TrimSpace(event.TurnID) && message.Delivery == conversation.DeliveryResponding {
					message.Delivery = delivery
					if err := p.updateMessage(ctx, *message); err != nil {
						return err
					}
				}
			}
			if err := p.expireQuestions(ctx, strings.TrimSpace(event.TurnID)); err != nil {
				return err
			}
			execution.Owner.TurnID = ""
			execution.Status = conversation.ExecutionRunning
			switch event.Status {
			case "interrupted":
				execution.Status = conversation.ExecutionInterrupted
			case "failed":
				execution.Status = conversation.ExecutionFailed
			}
			execution.Error, changed = strings.TrimSpace(event.Error), true
		}
	}
	if changed {
		return p.updateExecution(ctx, execution)
	}
	return nil
}

func (c *conversationService) prepareWorkerBind(ctx context.Context, scope nativeScope, item string, request conversationBindRequest) (*conversationPublicationPlan, error) {
	ctx = newConversationArchiveReads(ctx)
	now, err := c.server.database.currentTime()
	if err != nil {
		return nil, err
	}
	authTx, err := c.store.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	lease, ownerErr := c.requireWorkerOwner(ctx, authTx, scope, item, request.conversationWorkerIdentity, request.RunID, false, now)
	if err := errors.Join(ownerErr, rollbackConversationPreparation(authTx)); err != nil {
		return nil, err
	}
	record, err := readLinkedConversationRecord(ctx, c.store.reader, scope, item)
	if err == nil {
		readable := record
		if scope.credential.Scope == apiScopeWorker {
			readable.ReadAudience = ""
		}
		err = c.authorizeRead(ctx, c.store.reader, scope, readable)
	}
	original := record
	var creation *conversationRecord
	if err != nil {
		var missing *nativeError
		if !errors.As(err, &missing) || missing.status != http.StatusNotFound || record.ID != "" {
			return nil, err
		}
		issue, _, err := readNativeIssue(ctx, c.store.reader, scope, item)
		if err != nil {
			return nil, err
		}
		record = conversationRecord{ID: conversation.NewConversationID(), OrganizationID: scope.organization, ProjectID: scope.project, OwnerPrincipalID: scope.credential.ID, Title: deriveConversationTitle(issue.Title), Origin: conversationOriginWorker, Visibility: conversation.VisibilityShared, Status: conversation.StatusActive, WorkItemID: item, LinkedAt: &now, Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now, Revision: 1}
		if issue.Actor.PrincipalID != "" {
			var owner string
			if err := c.store.reader.QueryRowContext(ctx, "SELECT id FROM api_tokens WHERE id=?", issue.Actor.PrincipalID).Scan(&owner); err == nil {
				record.OwnerPrincipalID = owner
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		copy := record
		creation = &copy
	}
	p := &conversationPublicationPlan{service: c, record: record, original: original, creation: creation, now: now}
	previous := record.Execution
	if !previous.Status.Terminal() && previous.Owner.AttemptID != "" && previous.Owner.AttemptID != request.AttemptID {
		if err := p.finishExecution(ctx, conversation.ExecutionInterrupted, conversationLeaseLostError, false); err != nil {
			return nil, err
		}
	}
	thread := strings.TrimSpace(request.ThreadID)
	named := thread != ""
	if thread == "" {
		thread = p.record.ProviderThreadID
	} else {
		p.record.ProviderThreadID, p.record.ProviderThreadRunnerID, p.record.ProviderThreadOrigin = thread, scope.credential.Runner.RunnerID, conversation.ThreadOriginWorker
	}
	execution := conversation.Execution{Status: conversation.ExecutionStarting, Owner: conversation.Owner{AttemptID: request.AttemptID, RunID: request.RunID, LeaseID: string(request.LeaseID), FencingToken: int64(request.FencingToken), RunnerID: scope.credential.Runner.RunnerID, MachineID: string(lease.session.Machine.ID), ThreadID: thread}, Capabilities: request.Capabilities}
	p.record.Execution = execution
	if err := p.handOffControls(ctx, 0); err != nil {
		return nil, err
	}
	messages, err := p.pendingControls(ctx, 0)
	if err != nil {
		return nil, err
	}
	var pending []conversationControl
	for _, message := range messages {
		if len(pending) >= c.config.ControlQueueSize {
			break
		}
		if message.Kind == conversation.MessageAnswer {
			if _, _, err := c.resolveAnswer(ctx, c.store.reader, p.record.ID, message); err != nil {
				continue
			}
		}
		pending = append(pending, conversationControl{MessageID: message.ID})
	}
	resume, err := c.resumeFor(ctx, c.store.reader, p.record, scope, thread, named, false, pending)
	if err != nil {
		return nil, err
	}
	execution.Resume = conversationResumeKind(resume)
	if err := p.updateExecution(ctx, execution); err != nil {
		return nil, err
	}
	if execution.Resume == conversation.ResumeTranscript {
		message := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: conversationTranscriptNotice(len(resume.Transcript)), Delivery: conversation.DeliverySaved, AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID, Actor: conversationActorFor(scope)}
		if err := p.appendMessage(ctx, &message); err != nil {
			return nil, err
		}
	}
	return p, nil
}
