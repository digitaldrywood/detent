package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationReconcileInterval is how often the control router looks for
// executions whose lease was released or expired without an unbind.
var conversationReconcileInterval = 30 * time.Second

const (
	conversationControlMaxWait     = 30 * time.Second
	conversationMaxDeltaBytes      = 64 * 1024
	conversationMaxSummaryBytes    = 4000
	conversationMaxItemDataBytes   = 16 * 1024
	conversationMaxTurnEvents      = 500
	conversationHeaderLease        = "X-Detent-Lease"
	conversationHeaderFencingToken = "X-Detent-Fencing-Token"
	conversationHeaderAttempt      = "X-Detent-Attempt"
	conversationQuestionPrompt     = "Waiting for your answer"
	conversationLeaseLostError     = "runner lease lost"
)

// conversationWorkerRouter delivers committed controls to bound workers over
// the long-poll endpoint and reconciles executions whose lease was lost.
// The durable store is the queue: Wake only signals broker subscribers.
type conversationWorkerRouter struct {
	service *conversationService
	stop    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func newConversationControlRouter(service *conversationService) conversationControlRouter {
	router := &conversationWorkerRouter{service: service, stop: make(chan struct{})}
	router.wg.Add(1)
	go router.reconcileLoop()
	return router
}

// Wake notifies long-polling workers that queued controls may exist.
func (r *conversationWorkerRouter) Wake(conversationID string) {
	r.service.broker.notify(conversationID)
}

// Stop ends the reconcile loop and every in-flight long poll.
func (r *conversationWorkerRouter) Stop() {
	r.once.Do(func() { close(r.stop) })
	r.wg.Wait()
}

func (r *conversationWorkerRouter) reconcileLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(conversationReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			if err := r.service.reconcileExecutions(context.Background()); err != nil {
				r.service.logger.Warn("conversation execution reconcile failed", "error", err)
			}
		}
	}
}

// registerConversationWorkerRoutes mounts the lease-fenced worker endpoints.
func (s *Service) registerConversationWorkerRoutes(e *echo.Echo) {
	router, ok := s.conversations.controls.(*conversationWorkerRouter)
	if !ok {
		return
	}
	worker := s.requireNativeScope(apiScopeWorker)
	e.POST(nativeBase+"/work-items/:item/conversation/bind", router.bind, worker)
	e.POST(nativeBase+"/work-items/:item/conversation/unbind", router.unbind, worker)
	e.GET(nativeBase+"/conversations/:conversation/controls", router.controls, worker)
	e.POST(nativeBase+"/conversations/:conversation/turn-events", router.turnEvents, worker)
}

// Wire shapes.

// conversationWorkerIdentity is the lease identity every worker request
// carries: in the JSON body, or in headers for GET.
type conversationWorkerIdentity struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token"`
	AttemptID    string               `json:"attempt_id"`
}

type conversationBindRequest struct {
	conversationWorkerIdentity
	RunID        string                    `json:"run_id"`
	Capabilities conversation.Capabilities `json:"capabilities"`
	ThreadID     string                    `json:"thread_id,omitempty"`
}

type conversationBindResponse struct {
	ConversationID string `json:"conversation_id"`
	// Preferences are the conversation's turn preferences. The runner
	// applies the explicit ones to its turn request and leaves "auto" to the
	// project's configured defaults (decisions section 14).
	Preferences conversation.Preferences   `json:"preferences"`
	Resume      conversationResumeResource `json:"resume"`
	Pending     []conversationControl      `json:"pending"`
	Cursor      int64                      `json:"cursor"`
}

type conversationResumeResource struct {
	ThreadID string `json:"thread_id"`
	// ThreadOrigin names the kind of turn the thread the runner is about to
	// use belongs to: conversation.ThreadOriginCoordinator or
	// conversation.ThreadOriginWorker. It is the origin this bind's turn
	// records, so a runner holding a thread of the other kind in its own
	// registry can see that the hub did not hand it back (decisions section
	// 9.3). Empty when the bind resumed nothing and started no turn.
	ThreadOrigin string `json:"thread_origin,omitempty"`
	// Transcript carries the recent history when the provider thread cannot
	// be resumed, so the runner can prepend it as clearly delimited data.
	Transcript []conversationTranscriptEntry `json:"transcript,omitempty"`
}

// conversationTranscriptEntry is one bounded history line for a runner that
// starts a fresh provider thread.
type conversationTranscriptEntry struct {
	Role conversation.Role        `json:"role"`
	Kind conversation.MessageKind `json:"kind"`
	Text string                   `json:"text"`
}

type conversationUnbindRequest struct {
	conversationWorkerIdentity
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

type conversationUnbindResponse struct {
	ConversationID string                        `json:"conversation_id"`
	Execution      conversationExecutionResource `json:"execution"`
}

type conversationControlsResponse struct {
	Controls []conversationControl `json:"controls"`
	Cursor   int64                 `json:"cursor"`
}

// conversationControl is the envelope handed to the worker. Its kind is the
// runner control vocabulary: a continuation intent is a user message.
type conversationControl struct {
	Cursor     int64               `json:"cursor"`
	Key        string              `json:"key"`
	Kind       string              `json:"kind"`
	MessageID  string              `json:"message_id"`
	Text       string              `json:"text,omitempty"`
	QuestionID string              `json:"question_id,omitempty"`
	RequestID  string              `json:"request_id,omitempty"`
	Answers    map[string][]string `json:"answers,omitempty"`
	// Attachments are the files the message carries, each with the hub path
	// that streams it. The worker downloads them with its own token through
	// the conversation's read rule (decisions section 17.1).
	Attachments []conversation.Attachment `json:"attachments,omitempty"`
	Expected    conversation.Expected     `json:"expected"`
}

// conversationAnswerData is the message data of an answer control. The
// operator API stores the bare answers map and names the question on the
// receipt; a data object carrying both is accepted as well.
type conversationAnswerData struct {
	QuestionID string
	Answers    map[string][]string
}

func decodeConversationAnswer(data json.RawMessage) conversationAnswerData {
	var wrapped struct {
		QuestionID string              `json:"question_id"`
		Answers    map[string][]string `json:"answers"`
	}
	if len(data) > 0 && json.Unmarshal(data, &wrapped) == nil && (wrapped.QuestionID != "" || wrapped.Answers != nil) {
		return conversationAnswerData(wrapped)
	}
	var answers map[string][]string
	if len(data) > 0 {
		if err := json.Unmarshal(data, &answers); err != nil {
			return conversationAnswerData{}
		}
	}
	return conversationAnswerData{Answers: answers}
}

type conversationTurnEvent struct {
	Type           string `json:"type"`
	ThreadID       string `json:"thread_id,omitempty"`
	TurnID         string `json:"turn_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	ProviderItemID string `json:"provider_item_id,omitempty"`
	Text           string `json:"text,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Summary        string `json:"summary,omitempty"`
	// Data carries structured status for an item event, such as a
	// coordinator proposal (decisions section 9.3).
	Data      json.RawMessage       `json:"data,omitempty"`
	RequestID string                `json:"request_id,omitempty"`
	Prompts   []conversation.Prompt `json:"prompts,omitempty"`
	Key       string                `json:"key,omitempty"`
	Status    string                `json:"status,omitempty"`
	Error     string                `json:"error,omitempty"`
}

type conversationTurnEventsRequest struct {
	conversationWorkerIdentity
	// BatchKey identifies one worker batch and is repeated when the worker
	// retries it. A repeat applies nothing and answers with the event
	// sequence the first attempt produced, so a retried batch never appends
	// its deltas and items twice. It is optional; a worker that sends none
	// gets the old at-least-once behaviour.
	BatchKey string                  `json:"batch_key"`
	Events   []conversationTurnEvent `json:"events"`
}

type conversationTurnEventsResponse struct {
	EventSeq int64 `json:"event_seq"`
}

// Ownership checks.

func (identity conversationWorkerIdentity) validate() error {
	if strings.TrimSpace(string(identity.LeaseID)) == "" || identity.FencingToken <= 0 || strings.TrimSpace(identity.AttemptID) == "" {
		return nativeInvalid("lease_id, fencing_token and attempt_id are required")
	}
	return nil
}

func conversationIdentityFromHeaders(c echo.Context) (conversationWorkerIdentity, error) {
	header := c.Request().Header
	token, err := strconv.ParseInt(strings.TrimSpace(header.Get(conversationHeaderFencingToken)), 10, 64)
	if err != nil {
		return conversationWorkerIdentity{}, nativeInvalid("A numeric fencing token header is required")
	}
	identity := conversationWorkerIdentity{
		LeaseID:      tracker.LeaseID(strings.TrimSpace(header.Get(conversationHeaderLease))),
		FencingToken: tracker.FencingToken(token),
		AttemptID:    strings.TrimSpace(header.Get(conversationHeaderAttempt)),
	}
	return identity, identity.validate()
}

// conversationWorkerError maps tracker fencing failures onto the
// conversation vocabulary: a lease that is not current is stale_execution.
func conversationWorkerError(err error) error {
	if errors.Is(err, tracker.ErrStaleFencingToken) {
		return conversationStale("The lease is no longer current")
	}
	return err
}

// requireWorkerOwner validates the owner tuple against the hub's ledger: the
// credential is live, the lease belongs to the item and to the caller's
// runner, the policy is approved and a running native attempt row binds the
// attempt to that lease and fencing token. With allowReleased the lease may
// already be released or expired, which lets a finishing worker unbind.
func (c *conversationService) requireWorkerOwner(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, identity conversationWorkerIdentity, runID string, allowReleased bool, now time.Time) (leaseRecord, error) {
	if err := identity.validate(); err != nil {
		return leaseRecord{}, err
	}
	if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
		return leaseRecord{}, err
	}
	if allowReleased {
		if err := requireLeaseRunner(ctx, tx, identity.LeaseID, scope); err != nil {
			return leaseRecord{}, err
		}
	} else if err := requireNativeMutationLease(ctx, tx, scope, item, tracker.Mutation{LeaseID: identity.LeaseID, FencingToken: identity.FencingToken}, now); err != nil {
		return leaseRecord{}, conversationWorkerError(err)
	}
	lease, found, err := readLeaseByID(ctx, tx, identity.LeaseID)
	if err != nil {
		return leaseRecord{}, err
	}
	if !found {
		return leaseRecord{}, nativeNotFound()
	}
	_, id, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return leaseRecord{}, err
	}
	if lease.issueID != id {
		return leaseRecord{}, nativeNotFound()
	}
	if lease.session.FencingToken != identity.FencingToken {
		return leaseRecord{}, conversationStale("The lease fencing token does not match")
	}
	query := "SELECT count(*) FROM native_attempts WHERE id = ? AND organization_id = ? AND project_id = ? AND work_item_id = ? AND lease_id = ? AND fencing_token = ?"
	args := []any{identity.AttemptID, scope.organization, scope.project, item, identity.LeaseID, identity.FencingToken}
	if runID != "" {
		query += " AND run_id = ?"
		args = append(args, runID)
	}
	if !allowReleased {
		query += " AND status = 'running'"
	}
	var count int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return leaseRecord{}, fmt.Errorf("read conversation attempt: %w", err)
	}
	if count != 1 {
		return leaseRecord{}, conversationStale("The attempt is not the running owner of the lease")
	}
	return lease, nil
}

// requireRecordOwner checks that the conversation's execution is owned by
// the caller's generation. Terminal executions are stale unless allowed.
func requireConversationRecordOwner(record conversationRecord, identity conversationWorkerIdentity, allowTerminal bool) error {
	owner := record.Execution.Owner
	if owner.AttemptID != identity.AttemptID || owner.LeaseID != string(identity.LeaseID) || owner.FencingToken != int64(identity.FencingToken) {
		return conversationStaleOwner("The conversation is not bound to this attempt", identity.AttemptID, owner.AttemptID)
	}
	if !allowTerminal && record.Execution.Status.Terminal() {
		return conversationStale("The execution has ended")
	}
	return nil
}

// Transactions.

// transact runs fn in one transaction and commits when it returns nil. Only
// one transaction may be open at a time on the hub pool, so fn must never
// start another.

// readLinkedConversation resolves the canonical conversation of a work item
// in the request scope.
func (c *conversationService) readLinkedConversation(ctx context.Context, tx *sql.Tx, scope nativeScope, item string) (conversationRecord, error) {
	record, err := scanConversation(tx.QueryRowContext(ctx, "SELECT "+conversationReadColumns+" FROM conversations WHERE organization_id = ? AND project_id = ? AND work_item_id = ?", scope.organization, scope.project, item))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read linked conversation: %w", err)
	}
	if err := c.authorizeRead(scope, record); err != nil {
		return record, err
	}
	return record, nil
}

// conversationMessageQuery prefixes a constant filter; callers append their
// constant condition so the full statement is a compile-time string.
const conversationMessageQuery = "SELECT " + conversationMessageColumns + " FROM conversation_messages WHERE "

func (c *conversationService) queryMessages(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]conversationMessageRecord, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var messages []conversationMessageRecord
	for rows.Next() {
		message, err := scanConversationMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("query messages: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	// The rows are drained before the follow-up query: the hub pool holds a
	// single connection.
	pointers := make([]*conversationMessageRecord, 0, len(messages))
	for i := range messages {
		pointers = append(pointers, &messages[i])
	}
	if err := resolveMessageReferences(ctx, tx, pointers); err != nil {
		return nil, err
	}
	if err := resolveMessageAttachments(ctx, tx, pointers); err != nil {
		return nil, err
	}
	return messages, nil
}

// readReceipt returns the stored receipt for a command key; found is false
// when the key was never reserved.
func (c *conversationService) readReceipt(ctx context.Context, tx *sql.Tx, conversationID, key string) (receipt conversation.Receipt, found bool, err error) {
	var encoded string
	err = tx.QueryRowContext(ctx, "SELECT receipt_json FROM conversation_commands WHERE conversation_id = ? AND key = ?", conversationID, key).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Receipt{}, false, nil
	}
	if err != nil {
		return conversation.Receipt{}, false, fmt.Errorf("read receipt: %w", err)
	}
	decoded, err := decodeConversationReceipt(encoded)
	if err != nil || decoded == nil {
		return conversation.Receipt{}, false, err
	}
	return *decoded, true, nil
}

// setControlDelivery moves a user control on the delivery ladder and keeps
// its receipt in step. Receipts keep their identifiers and only change
// status and error.
func (c *conversationService) setControlDelivery(ctx context.Context, tx *sql.Tx, message *conversationMessageRecord, delivery conversation.Delivery, failure *conversation.ReceiptError, now time.Time) error {
	message.Delivery = delivery
	if err := c.updateMessage(ctx, tx, *message, now); err != nil {
		return err
	}
	if message.CommandKey == "" {
		return nil
	}
	receipt, found, err := c.readReceipt(ctx, tx, message.ConversationID, message.CommandKey)
	if err != nil || !found {
		return err
	}
	receipt.Status = delivery
	receipt.Error = failure
	if receipt.MessageID == "" {
		receipt.MessageID = message.ID
	}
	return c.recordReceipt(ctx, tx, message.ConversationID, message.AttemptID, receipt, now)
}

// handOffControls marks queued controls after the cursor as sending for the
// current owner and returns their envelopes in acceptance order. Controls
// already sending to this owner are returned again without new events.
func (c *conversationService) handOffControls(ctx context.Context, tx *sql.Tx, record *conversationRecord, after int64, now time.Time) (controls []conversationControl, changed bool, err error) {
	owner := record.Execution.Owner
	// A queued control has not been handed to this owner whatever its seq
	// is, so a retry that re-queues an older message still reaches a worker
	// whose cursor is already past it (decisions section 10.3).
	messages, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND (seq > ? OR delivery = 'queued') AND ((delivery IN ('queued', 'sending') AND kind IN ('text', 'answer', 'interrupt', 'continue')) OR (delivery = 'saved' AND kind = 'continue')) ORDER BY seq", record.ID, after)
	if err != nil {
		return nil, false, err
	}
	controls = []conversationControl{}
	for i := range messages {
		if len(controls) >= c.config.ControlQueueSize {
			break
		}
		message := &messages[i]
		control := conversationControl{Cursor: message.Seq, Key: message.CommandKey, MessageID: message.ID, Expected: conversation.Expected{AttemptID: owner.AttemptID, TurnID: owner.TurnID}}
		switch message.Kind {
		case conversation.MessageText, conversation.MessageContinue:
			control.Kind = "message"
			control.Text = message.Text
			control.Attachments = projectAttachments(message.Attachments)
		case conversation.MessageInterrupt:
			control.Kind = "interrupt"
		case conversation.MessageAnswer:
			control.Kind = "answer"
			question, answers, err := c.resolveAnswer(ctx, tx, record.ID, *message)
			if err != nil {
				// The answer cannot reach the provider without its request.
				if err := c.setControlDelivery(ctx, tx, message, conversation.DeliveryUnknown, &conversation.ReceiptError{Code: "unknown", Message: "The question is no longer available"}, now); err != nil {
					return nil, false, err
				}
				changed = true
				continue
			}
			control.QuestionID = question.ID
			control.RequestID = question.RequestID
			control.Answers = answers
		}
		if message.Delivery != conversation.DeliverySending || message.AttemptID != owner.AttemptID {
			message.AttemptID = owner.AttemptID
			message.ThreadID = owner.ThreadID
			message.TurnID = owner.TurnID
			if err := c.setControlDelivery(ctx, tx, message, conversation.DeliverySending, nil, now); err != nil {
				return nil, false, err
			}
			changed = true
		}
		controls = append(controls, control)
	}
	return controls, changed, nil
}

// resolveAnswer finds the question an answer control targets: named on the
// receipt or in the message data. The answers come from the question row
// when the operator API recorded them there, otherwise from the message.
func (c *conversationService) resolveAnswer(ctx context.Context, tx *sql.Tx, conversationID string, message conversationMessageRecord) (conversationQuestionRecord, map[string][]string, error) {
	data := decodeConversationAnswer(message.Data)
	questionID := data.QuestionID
	if questionID == "" && message.CommandKey != "" {
		receipt, found, err := c.readReceipt(ctx, tx, conversationID, message.CommandKey)
		if err != nil {
			return conversationQuestionRecord{}, nil, err
		}
		if found {
			questionID = receipt.QuestionID
		}
	}
	if questionID == "" {
		return conversationQuestionRecord{}, nil, nativeNotFound()
	}
	question, err := c.store.readQuestion(ctx, tx, conversationID, questionID)
	if err != nil {
		return conversationQuestionRecord{}, nil, err
	}
	answers := question.Answers
	if len(answers) == 0 {
		answers = data.Answers
	}
	return question, answers, nil
}

// finishExecution applies the terminal transition shared by unbind and lease
// loss: streaming assistant output settles, every control that was handed to
// the ended attempt becomes unknown rather than being replayed, a control
// that was never handed out stays queued, and pending questions expire.
// Nothing is retried automatically; the user retries explicitly with a retry
// command (decisions section 10.3).
func (c *conversationService) finishExecution(ctx context.Context, tx *sql.Tx, record *conversationRecord, status conversation.ExecutionStatus, failure string, settled bool, now time.Time) error {
	execution := record.Execution
	responding, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND attempt_id = ? AND delivery = 'responding' ORDER BY seq", record.ID, execution.Owner.AttemptID)
	if err != nil {
		return err
	}
	for i := range responding {
		responding[i].Delivery = conversationDeliveryForExecution(status)
		if err := c.updateMessage(ctx, tx, responding[i], now); err != nil {
			return err
		}
	}
	controls, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND delivery IN ('queued', 'sending', 'sent') ORDER BY seq", record.ID)
	if err != nil {
		return err
	}
	for i := range controls {
		message := &controls[i]
		switch {
		case message.Delivery == conversation.DeliverySent:
			// The worker confirmed receipt, so the control was established;
			// it settles with the execution rather than lingering at sent.
			if err := c.setControlDelivery(ctx, tx, message, conversationDeliveryForExecution(status), nil, now); err != nil {
				return err
			}
		case message.Kind == conversation.MessageAnswer || message.Kind == conversation.MessageInterrupt:
			// Only the ended attempt could ever serve these.
			if err := c.setControlDelivery(ctx, tx, message, conversation.DeliveryUnknown, &conversation.ReceiptError{Code: "unknown", Message: "The execution ended before the control was delivered"}, now); err != nil {
				return err
			}
		case message.Delivery == conversation.DeliverySending:
			// Handed to the worker and never confirmed: the hub cannot tell
			// whether the provider saw it, so it is never replayed.
			if err := c.setControlDelivery(ctx, tx, message, conversation.DeliveryUnknown, &conversation.ReceiptError{Code: "unknown", Message: "The execution ended before the control could be established"}, now); err != nil {
				return err
			}
		}
	}
	if err := c.expireQuestions(ctx, tx, record.ID, "", now); err != nil {
		return err
	}
	execution.Status = status
	execution.Error = failure
	execution.Capabilities = conversation.Capabilities{}
	execution.Settled = settled
	return c.updateExecution(ctx, tx, record, execution, now)
}

func conversationDeliveryForExecution(status conversation.ExecutionStatus) conversation.Delivery {
	switch status {
	case conversation.ExecutionCompleted:
		return conversation.DeliveryCompleted
	case conversation.ExecutionFailed:
		return conversation.DeliveryFailed
	case conversation.ExecutionInterrupted:
		return conversation.DeliveryInterrupted
	default:
		return conversation.DeliveryUnknown
	}
}

// expireQuestions expires every question still awaiting an answer, limited
// to one turn when turnID is set.
func (c *conversationService) expireQuestions(ctx context.Context, tx *sql.Tx, conversationID, turnID string, now time.Time) error {
	pending, err := c.store.listPendingQuestions(ctx, tx, conversationID)
	if err != nil {
		return err
	}
	for _, question := range pending {
		if turnID != "" && question.Owner.TurnID != turnID {
			continue
		}
		if err := c.updateQuestion(ctx, tx, question, conversation.QuestionExpired, now); err != nil {
			return err
		}
	}
	return nil
}

func (c *conversationService) updateQuestion(ctx context.Context, tx *sql.Tx, question conversationQuestionRecord, status conversation.QuestionStatus, now time.Time) error {
	question.Status = status
	question.UpdatedAt = now
	if err := c.store.upsertQuestion(ctx, tx, question); err != nil {
		return err
	}
	if _, err := c.store.appendEvent(ctx, tx, question.ConversationID, conversation.EventQuestionUpdated, question.Question, now); err != nil {
		return err
	}
	return nil
}

// Handlers.

func (r *conversationWorkerRouter) bind(e echo.Context) error {
	c, s := r.service, r.service.server
	var request conversationBindRequest
	if err := decodeAPIJSON(e, &request); err != nil {
		return invalidAPIRequest(e, err)
	}
	if err := request.validate(); err != nil {
		return s.nativeAPIError(e, err)
	}
	if strings.TrimSpace(request.RunID) == "" {
		return s.nativeAPIError(e, nativeInvalid("run_id is required"))
	}
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	item := e.Param("item")
	var record conversationRecord
	var response conversationBindResponse
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
		lease, err := c.requireWorkerOwner(ctx, tx, scope, item, request.conversationWorkerIdentity, request.RunID, false, now)
		if err != nil {
			return err
		}
		record, err = c.readLinkedConversation(ctx, tx, scope, item)
		if err != nil {
			return err
		}
		if err := c.store.recordStart(ctx, tx, request.AttemptID, record.ID, now); err != nil {
			var started *errConversationStarted
			if errors.As(err, &started) {
				return conversationStale("The attempt already started a conversation turn")
			}
			return err
		}
		previous := record.Execution
		if !previous.Status.Terminal() && previous.Owner.AttemptID != "" && previous.Owner.AttemptID != request.AttemptID {
			// The earlier owner lost its lease without unbinding; the new
			// claim proves that. Settle it before the new attempt takes over.
			if err := c.finishExecution(ctx, tx, &record, conversation.ExecutionInterrupted, conversationLeaseLostError, false, now); err != nil {
				return err
			}
		}
		thread := strings.TrimSpace(request.ThreadID)
		named := thread != ""
		if thread == "" {
			thread = record.ProviderThreadID
		} else {
			// The runner named the thread, so the thread is its own and this
			// bind's kind of turn is its origin (decisions section 9.3).
			record.ProviderThreadID = thread
			record.ProviderThreadRunnerID = scope.credential.Runner.RunnerID
			record.ProviderThreadOrigin = conversation.ThreadOriginWorker
		}
		execution := conversation.Execution{
			Status: conversation.ExecutionStarting,
			Owner: conversation.Owner{
				AttemptID: request.AttemptID, RunID: request.RunID, LeaseID: string(request.LeaseID), FencingToken: int64(request.FencingToken),
				RunnerID: scope.credential.Runner.RunnerID, MachineID: string(lease.session.Machine.ID), ThreadID: thread,
			},
			Capabilities: request.Capabilities,
		}
		// The owner tuple is taken in memory first so the hand-off and the
		// transcript see the new generation; the execution is persisted once,
		// with the resume it ends up reporting (decisions section 10.4).
		record.Execution = execution
		pending, _, err := c.handOffControls(ctx, tx, &record, 0, now)
		if err != nil {
			return err
		}
		resume, err := c.resumeFor(ctx, tx, record, scope, thread, named, false, pending)
		if err != nil {
			return err
		}
		execution.Resume = conversationResumeKind(resume)
		if err := c.updateExecution(ctx, tx, &record, execution, now); err != nil {
			return err
		}
		if execution.Resume == conversation.ResumeTranscript {
			// Transcript recovery is visible in the history rather than only
			// in the runner's prompt (decisions section 10.4).
			notice := conversationMessageRecord{
				Role: conversation.RoleSystem, Kind: conversation.MessageStatus,
				Text:      conversationTranscriptNotice(len(resume.Transcript)),
				Delivery:  conversation.DeliverySaved,
				AttemptID: execution.Owner.AttemptID, ThreadID: execution.Owner.ThreadID,
				Actor: conversationActorFor(scope),
			}
			if err := c.appendMessage(ctx, tx, &record, &notice, now); err != nil {
				return err
			}
		}
		response.ConversationID, response.Resume, response.Pending = record.ID, resume, pending
		response.Preferences = record.Preferences.Normalized()
		if len(pending) > 0 {
			response.Cursor = pending[len(pending)-1].Cursor
		}
		return nil
	})
	if err != nil {
		c.logStaleExecution("bind", record.ID, err)
		return s.nativeAPIError(e, err)
	}
	c.committed(record)
	return e.JSON(http.StatusOK, response)
}

func (r *conversationWorkerRouter) unbind(e echo.Context) error {
	c, s := r.service, r.service.server
	var request conversationUnbindRequest
	if err := decodeAPIJSON(e, &request); err != nil {
		return invalidAPIRequest(e, err)
	}
	if err := request.validate(); err != nil {
		return s.nativeAPIError(e, err)
	}
	status, ok := map[string]conversation.ExecutionStatus{
		"succeeded": conversation.ExecutionCompleted, "failed": conversation.ExecutionFailed,
		"cancelled": conversation.ExecutionInterrupted, "interrupted": conversation.ExecutionInterrupted,
	}[request.Outcome]
	if !ok {
		return s.nativeAPIError(e, nativeInvalid("outcome must be succeeded, failed, cancelled or interrupted"))
	}
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	item := e.Param("item")
	var record conversationRecord
	changed := false
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
		if _, err := c.requireWorkerOwner(ctx, tx, scope, item, request.conversationWorkerIdentity, "", true, now); err != nil {
			return err
		}
		var err error
		record, err = c.readLinkedConversation(ctx, tx, scope, item)
		if err != nil {
			return err
		}
		if err := requireConversationRecordOwner(record, request.conversationWorkerIdentity, true); err != nil {
			return err
		}
		changed = true
		if record.Execution.Settled {
			// This attempt already unbound; a repeated call changes nothing.
			changed = false
			return nil
		}
		if record.Execution.Status == conversation.ExecutionInterrupted && record.Execution.Error == conversationLeaseLostError {
			// The lease was released before the unbind arrived and lease
			// loss already settled the controls; the worker's own outcome
			// is the accurate verdict for the execution.
			execution := record.Execution
			execution.Status = status
			execution.Error = strings.TrimSpace(request.Error)
			execution.Settled = true
			return c.updateExecution(ctx, tx, &record, execution, now)
		}
		// A turn that reported interrupted or failed already made the
		// execution terminal; the unbind outcome still wins and the
		// controls still settle (decisions section 10.7).
		return c.finishExecution(ctx, tx, &record, status, strings.TrimSpace(request.Error), true, now)
	})
	if err != nil {
		c.logStaleExecution("unbind", record.ID, err)
		return s.nativeAPIError(e, err)
	}
	if changed {
		c.committed(record)
	}
	return e.JSON(http.StatusOK, conversationUnbindResponse{ConversationID: record.ID, Execution: projectExecution(record.Execution)})
}

func (r *conversationWorkerRouter) controls(e echo.Context) error {
	c, s := r.service, r.service.server
	identity, err := conversationIdentityFromHeaders(e)
	if err != nil {
		return s.nativeAPIError(e, err)
	}
	after, wait, err := conversationControlQuery(e)
	if err != nil {
		return s.nativeAPIError(e, err)
	}
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	id := e.Param("conversation")
	subscription, cancel := c.broker.subscribe(id)
	defer cancel()
	var deadline <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		page, err := c.pollControls(ctx, scope, id, identity, after)
		if err != nil {
			c.logStaleExecution("controls", id, err)
			return s.nativeAPIError(e, err)
		}
		if len(page.Controls) > 0 || wait == 0 {
			return e.JSON(http.StatusOK, page)
		}
		select {
		case <-subscription.wake:
			continue
		case <-subscription.closed:
		case <-r.stop:
		case <-deadline:
		case <-ctx.Done():
		}
		return e.JSON(http.StatusOK, page)
	}
}

func conversationControlQuery(e echo.Context) (after int64, wait time.Duration, err error) {
	if raw := strings.TrimSpace(e.QueryParam("after")); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			return 0, 0, nativeInvalid("after must be a non-negative cursor")
		}
	}
	if raw := strings.TrimSpace(e.QueryParam("wait")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 {
			return 0, 0, nativeInvalid("wait must be a number of seconds")
		}
		wait = min(time.Duration(seconds)*time.Second, conversationControlMaxWait)
	}
	return after, wait, nil
}

// pollControls acknowledges controls up to the cursor and hands off the next
// queued controls in one transaction. Nothing is committed when the poll
// changed nothing, so an idle worker never produces events.
func (c *conversationService) pollControls(ctx context.Context, scope nativeScope, id string, identity conversationWorkerIdentity, after int64) (conversationControlsResponse, error) {
	page := conversationControlsResponse{Controls: []conversationControl{}, Cursor: after}
	var record conversationRecord
	changed := false
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		var item string
		var err error
		record, item, err = c.loadWorkerConversation(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if err := requireConversationRecordOwner(record, identity, false); err != nil {
			return err
		}
		if _, err := c.requireWorkerOwner(ctx, tx, scope, item, identity, "", false, now); err != nil {
			return err
		}
		acknowledged, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND delivery = 'sending' AND attempt_id = ? AND seq <= ? ORDER BY seq", record.ID, identity.AttemptID, after)
		if err != nil {
			return err
		}
		for i := range acknowledged {
			if err := c.setControlDelivery(ctx, tx, &acknowledged[i], conversation.DeliverySent, nil, now); err != nil {
				return err
			}
			changed = true
		}
		controls, handed, err := c.handOffControls(ctx, tx, &record, after, now)
		if err != nil {
			return err
		}
		changed = changed || handed
		page.Controls = controls
		if len(controls) > 0 {
			page.Cursor = controls[len(controls)-1].Cursor
		}
		return nil
	})
	if err != nil {
		return page, err
	}
	if changed {
		c.committed(record)
	}
	return page, nil
}

// conversationTurn carries the state of one turn-event batch while it is
// applied inside a transaction.
type conversationTurn struct {
	scope     nativeScope
	record    *conversationRecord
	execution conversation.Execution
	changed   bool
	now       time.Time
}

func (r *conversationWorkerRouter) turnEvents(e echo.Context) error {
	c, s := r.service, r.service.server
	var request conversationTurnEventsRequest
	if err := decodeAPIJSON(e, &request); err != nil {
		return invalidAPIRequest(e, err)
	}
	if err := request.validate(); err != nil {
		return s.nativeAPIError(e, err)
	}
	if len(request.Events) > conversationMaxTurnEvents {
		return s.nativeAPIError(e, nativeInvalid("Too many events in one batch"))
	}
	request.BatchKey = strings.TrimSpace(request.BatchKey)
	if len(request.BatchKey) > conversation.MaxCommandKeyBytes {
		return s.nativeAPIError(e, nativeInvalid(fmt.Sprintf("batch_key must be at most %d bytes", conversation.MaxCommandKeyBytes)))
	}
	ctx := e.Request().Context()
	scope := nativeRequestScope(e)
	id := e.Param("conversation")
	var record conversationRecord
	var response conversationTurnEventsResponse
	replayed := false
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
		var item string
		var err error
		record, item, err = c.loadWorkerConversation(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if err := requireConversationRecordOwner(record, request.conversationWorkerIdentity, false); err != nil {
			return err
		}
		if _, err := c.requireWorkerOwner(ctx, tx, scope, item, request.conversationWorkerIdentity, "", false, now); err != nil {
			return err
		}
		if request.BatchKey != "" {
			var stored int64
			err := tx.QueryRowContext(ctx, "SELECT event_seq FROM conversation_turn_batches WHERE attempt_id = ? AND batch_key = ?", request.AttemptID, request.BatchKey).Scan(&stored)
			switch {
			case err == nil:
				// The batch already committed; nothing is applied again.
				response.EventSeq, replayed = stored, true
				return nil
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("read turn batch: %w", err)
			}
		}
		turn := &conversationTurn{scope: scope, record: &record, execution: record.Execution, now: now}
		for _, event := range request.Events {
			if err := c.applyTurnEvent(ctx, tx, turn, event); err != nil {
				return err
			}
		}
		if turn.changed {
			if err := c.updateExecution(ctx, tx, &record, turn.execution, now); err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, "SELECT event_seq FROM conversations WHERE id = ?", record.ID).Scan(&response.EventSeq); err != nil {
			return fmt.Errorf("read event sequence: %w", err)
		}
		record.EventSeq = response.EventSeq
		if request.BatchKey != "" {
			if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_turn_batches (attempt_id, batch_key, event_seq, created_at) VALUES (?, ?, ?, ?)",
				request.AttemptID, request.BatchKey, response.EventSeq, conversationTime(now)); err != nil {
				return fmt.Errorf("record turn batch: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		c.logStaleExecution("turn_events", id, err)
		return s.nativeAPIError(e, err)
	}
	if !replayed {
		c.committed(record)
	}
	return e.JSON(http.StatusAccepted, response)
}

func (c *conversationService) applyTurnEvent(ctx context.Context, tx *sql.Tx, turn *conversationTurn, event conversationTurnEvent) error {
	switch event.Type {
	case "turn_started":
		return c.applyTurnStarted(ctx, tx, turn, event)
	case "delta":
		return c.applyDelta(ctx, tx, turn, event)
	case "item":
		return c.applyItem(ctx, tx, turn, event)
	case "question_opened":
		return c.applyQuestionOpened(ctx, tx, turn, event)
	case "control_result":
		return c.applyControlResult(ctx, tx, turn, event)
	case "turn_completed":
		return c.applyTurnCompleted(ctx, tx, turn, event)
	case "execution_status":
		status := conversation.ExecutionStatus(event.Status)
		if !status.Valid() {
			return nativeInvalid("execution_status carries an unknown status")
		}
		turn.execution.Status = status
		turn.execution.Error = strings.TrimSpace(event.Error)
		turn.changed = true
		return nil
	default:
		return nativeInvalid("Unknown turn event type " + strconv.Quote(event.Type))
	}
}

func (c *conversationService) applyTurnStarted(ctx context.Context, tx *sql.Tx, turn *conversationTurn, event conversationTurnEvent) error {
	turnID := strings.TrimSpace(event.TurnID)
	if turnID == "" {
		return nativeInvalid("turn_started requires a turn_id")
	}
	if current := turn.execution.Owner.TurnID; current != "" && current != turnID {
		return conversationStale("Another turn is still in progress")
	}
	if thread := strings.TrimSpace(event.ThreadID); thread != "" {
		turn.execution.Owner.ThreadID = thread
		turn.record.ProviderThreadID = thread
		// The thread belongs to the provider login of the runner reporting
		// it; only that runner may resume it (decisions section 9.3).
		turn.record.ProviderThreadRunnerID = turn.scope.credential.Runner.RunnerID
		// It also carries the instructions and permission set of the kind of
		// turn that opened it, so a later bind of the other kind is handed a
		// transcript instead of this thread.
		turn.record.ProviderThreadOrigin = conversation.ThreadOriginWorker
	}
	turn.execution.Owner.TurnID = turnID
	turn.execution.Status = conversation.ExecutionRunning
	turn.execution.Error = ""
	turn.changed = true
	prompted, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND attempt_id = ? AND delivery IN ('sending', 'sent') AND kind IN ('text', 'continue') ORDER BY seq", turn.record.ID, turn.execution.Owner.AttemptID)
	if err != nil {
		return err
	}
	for i := range prompted {
		prompted[i].ThreadID = turn.execution.Owner.ThreadID
		prompted[i].TurnID = turnID
		if err := c.setControlDelivery(ctx, tx, &prompted[i], conversation.DeliveryDelivered, nil, turn.now); err != nil {
			return err
		}
	}
	return nil
}

func (c *conversationService) applyDelta(ctx context.Context, tx *sql.Tx, turn *conversationTurn, event conversationTurnEvent) error {
	item := strings.TrimSpace(event.ProviderItemID)
	if item == "" {
		return nativeInvalid("delta requires a provider_item_id")
	}
	if len(event.Text) > conversationMaxDeltaBytes {
		return nativeInvalid("delta text is too long")
	}
	owner := turn.execution.Owner
	query, args := conversationMessageQuery+"conversation_id = ? AND role = 'assistant' AND attempt_id = ? AND provider_item_id = ? ORDER BY seq", []any{turn.record.ID, owner.AttemptID, item}
	if event.MessageID != "" {
		query, args = conversationMessageQuery+"conversation_id = ? AND role = 'assistant' AND attempt_id = ? AND id = ? ORDER BY seq", []any{turn.record.ID, owner.AttemptID, event.MessageID}
	}
	existing, err := c.queryMessages(ctx, tx, query, args...)
	if err != nil {
		return err
	}
	var message conversationMessageRecord
	if len(existing) > 0 {
		message = existing[0]
	} else {
		message = conversationMessageRecord{
			Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding,
			AttemptID: owner.AttemptID, ThreadID: owner.ThreadID, TurnID: owner.TurnID, ProviderItemID: item,
			Actor: conversationActorFor(turn.scope),
		}
		if err := c.appendMessage(ctx, tx, turn.record, &message, turn.now); err != nil {
			return err
		}
	}
	if event.Text == "" {
		return nil
	}
	return c.appendDelta(ctx, tx, &message, event.Text, turn.now)
}

func (c *conversationService) applyItem(ctx context.Context, tx *sql.Tx, turn *conversationTurn, event conversationTurnEvent) error {
	kind := conversation.MessageKind(event.Kind)
	if kind != conversation.MessageTool && kind != conversation.MessageStatus {
		return nativeInvalid("item kind must be tool or status")
	}
	data, err := conversationItemData(event.Data)
	if err != nil {
		return err
	}
	owner := turn.execution.Owner
	message := conversationMessageRecord{
		Role: conversation.RoleSystem, Kind: kind, Text: conversationTruncate(event.Summary, conversationMaxSummaryBytes), Data: data, Delivery: conversation.DeliveryCompleted,
		AttemptID: owner.AttemptID, ThreadID: owner.ThreadID, TurnID: owner.TurnID, ProviderItemID: strings.TrimSpace(event.ProviderItemID),
		Actor: conversationActorFor(turn.scope),
	}
	return c.appendMessage(ctx, tx, turn.record, &message, turn.now)
}

// conversationItemData validates the optional structured data of an item
// event: a bounded JSON object, never an array or a bare value.
func conversationItemData(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > conversationMaxItemDataBytes {
		return nil, nativeInvalid("item data is too long")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, nativeInvalid("item data must be a JSON object")
	}
	return raw, nil
}

func conversationTruncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && cut < len(value) && (value[cut]&0xC0) == 0x80 {
		cut--
	}
	return value[:cut]
}

func (c *conversationService) applyQuestionOpened(ctx context.Context, tx *sql.Tx, turn *conversationTurn, event conversationTurnEvent) error {
	requestID := strings.TrimSpace(event.RequestID)
	if requestID == "" {
		return nativeInvalid("question_opened requires a request_id and prompts")
	}
	if err := conversation.ValidatePrompts(event.Prompts); err != nil {
		return nativeInvalid(err.Error())
	}
	owner := turn.execution.Owner
	turnID := strings.TrimSpace(event.TurnID)
	if turnID != "" && owner.TurnID != "" && turnID != owner.TurnID {
		return conversationStale("The question belongs to another turn")
	}
	if turnID == "" {
		turnID = owner.TurnID
	}
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT id FROM conversation_questions WHERE conversation_id = ? AND attempt_id = ? AND request_id = ?", turn.record.ID, owner.AttemptID, requestID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read question: %w", err)
	}
	message := conversationMessageRecord{
		Role: conversation.RoleAssistant, Kind: conversation.MessageStatus, Text: conversationQuestionPrompt, Delivery: conversation.DeliveryCompleted,
		AttemptID: owner.AttemptID, ThreadID: owner.ThreadID, TurnID: turnID, Actor: conversationActorFor(turn.scope),
	}
	if err := c.appendMessage(ctx, tx, turn.record, &message, turn.now); err != nil {
		return err
	}
	expires := turn.now.Add(c.config.QuestionTimeout)
	questionOwner := owner
	questionOwner.TurnID = turnID
	if thread := strings.TrimSpace(event.ThreadID); thread != "" {
		questionOwner.ThreadID = thread
	}
	question := conversationQuestionRecord{Question: conversation.Question{
		ID: conversation.NewQuestionID(), ConversationID: turn.record.ID, MessageID: message.ID, Status: conversation.QuestionPending,
		Owner: questionOwner, Prompts: event.Prompts, ExpiresAt: &expires, CreatedAt: turn.now, UpdatedAt: turn.now,
	}, RequestID: requestID}
	if err := c.store.upsertQuestion(ctx, tx, question); err != nil {
		return err
	}
	if _, err := c.store.appendEvent(ctx, tx, turn.record.ID, conversation.EventQuestionOpened, question.Question, turn.now); err != nil {
		return err
	}
	turn.execution.Status = conversation.ExecutionWaitingInput
	turn.changed = true
	return nil
}

func (c *conversationService) applyControlResult(ctx context.Context, tx *sql.Tx, turn *conversationTurn, event conversationTurnEvent) error {
	key := strings.TrimSpace(event.Key)
	if key == "" {
		return nativeInvalid("control_result requires a key")
	}
	messages, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND role = 'user' AND command_key = ? ORDER BY seq", turn.record.ID, key)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return nativeInvalid("Unknown control key " + strconv.Quote(key))
	}
	message := &messages[0]
	if message.AttemptID != turn.execution.Owner.AttemptID {
		return conversationStale("The control was handed to another attempt")
	}
	var delivery conversation.Delivery
	var failure *conversation.ReceiptError
	var questionStatus conversation.QuestionStatus
	switch event.Status {
	case "delivered":
		delivery, questionStatus = conversation.DeliveryDelivered, conversation.QuestionAnswered
		if message.Kind == conversation.MessageAnswer {
			// Answers have no provider acknowledgement: sent is the ceiling.
			delivery = conversation.DeliverySent
		}
	case "rejected":
		delivery, questionStatus = conversation.DeliveryRejected, conversation.QuestionExpired
		failure = &conversation.ReceiptError{Code: "rejected", Message: strings.TrimSpace(event.Error)}
	case "unknown":
		delivery, questionStatus = conversation.DeliveryUnknown, conversation.QuestionUnknown
		failure = &conversation.ReceiptError{Code: "unknown", Message: strings.TrimSpace(event.Error)}
	default:
		return nativeInvalid("control_result status must be delivered, rejected or unknown")
	}
	if err := c.setControlDelivery(ctx, tx, message, delivery, failure, turn.now); err != nil {
		return err
	}
	if message.Kind != conversation.MessageAnswer {
		return nil
	}
	question, _, err := c.resolveAnswer(ctx, tx, turn.record.ID, *message)
	if err != nil {
		var failure *nativeError
		if errors.As(err, &failure) && failure.status == http.StatusNotFound {
			// The question row is gone; the receipt already records the outcome.
			return nil
		}
		return err
	}
	switch question.Status {
	case conversation.QuestionPending, conversation.QuestionSending, conversation.QuestionSent:
		if questionStatus == conversation.QuestionAnswered && question.AnsweredBy == "" {
			question.AnsweredBy = message.Actor.PrincipalID
		}
		if err := c.updateQuestion(ctx, tx, question, questionStatus, turn.now); err != nil {
			return err
		}
	}
	if questionStatus != conversation.QuestionAnswered || turn.execution.Status != conversation.ExecutionWaitingInput {
		return nil
	}
	pending, err := c.store.listPendingQuestions(ctx, tx, turn.record.ID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		turn.execution.Status = conversation.ExecutionRunning
		turn.changed = true
	}
	return nil
}

func (c *conversationService) applyTurnCompleted(ctx context.Context, tx *sql.Tx, turn *conversationTurn, event conversationTurnEvent) error {
	turnID := strings.TrimSpace(event.TurnID)
	if turnID == "" {
		return nativeInvalid("turn_completed requires a turn_id")
	}
	if current := turn.execution.Owner.TurnID; current != "" && current != turnID {
		return conversationStale("The completed turn is not the current turn")
	}
	var delivery conversation.Delivery
	switch event.Status {
	case "completed":
		delivery = conversation.DeliveryCompleted
	case "interrupted":
		delivery = conversation.DeliveryInterrupted
	case "failed":
		delivery = conversation.DeliveryFailed
	default:
		return nativeInvalid("turn_completed status must be completed, interrupted or failed")
	}
	responding, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND role = 'assistant' AND attempt_id = ? AND turn_id = ? AND delivery = 'responding' ORDER BY seq", turn.record.ID, turn.execution.Owner.AttemptID, turnID)
	if err != nil {
		return err
	}
	for i := range responding {
		responding[i].Delivery = delivery
		if err := c.updateMessage(ctx, tx, responding[i], turn.now); err != nil {
			return err
		}
	}
	if err := c.expireQuestions(ctx, tx, turn.record.ID, turnID, turn.now); err != nil {
		return err
	}
	turn.execution.Owner.TurnID = ""
	// An interrupted or failed turn stays interrupted or failed: the
	// execution must not report running until the runner unbinds
	// (decisions section 10.7).
	switch delivery {
	case conversation.DeliveryInterrupted:
		turn.execution.Status = conversation.ExecutionInterrupted
	case conversation.DeliveryFailed:
		turn.execution.Status = conversation.ExecutionFailed
	default:
		turn.execution.Status = conversation.ExecutionRunning
	}
	turn.execution.Error = strings.TrimSpace(event.Error)
	turn.changed = true
	return nil
}

// Lease loss.

// leaseReleased settles the execution bound to a lease right after the
// lease's release committed. Failures are logged: the release itself has
// already succeeded and the reconcile loop retries.
func (c *conversationService) leaseReleased(ctx context.Context, leaseID tracker.LeaseID) {
	candidates, err := c.inFlightExecutions(ctx, conversationInFlightQuery+"AND json_extract(execution_json, '$.lease_id') = ?", leaseID)
	if err != nil {
		c.logger.Warn("conversation lease release lookup failed", "lease", leaseID, "error", err)
		return
	}
	for _, candidate := range candidates {
		if err := c.settleLostExecution(ctx, candidate); err != nil {
			c.logger.Warn("conversation lease release settlement failed", "conversation", candidate.id, "error", err)
		}
	}
}

// reconcileExecutions marks executions whose lease was released or expired
// without an unbind as interrupted, applying the unbind transition.
func (c *conversationService) reconcileExecutions(ctx context.Context) error {
	candidates, err := c.inFlightExecutions(ctx, conversationInFlightQuery)
	if err != nil {
		return err
	}
	var errs []error
	for _, candidate := range candidates {
		if err := c.settleLostExecution(ctx, candidate); err != nil {
			errs = append(errs, fmt.Errorf("conversation %s: %w", candidate.id, err))
		}
	}
	return errors.Join(errs...)
}

type conversationExecutionRef struct {
	id           string
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

// inFlightExecutions lists conversations whose execution is live, or ended by
// a turn report but never settled by an unbind. It runs outside any
// transaction; settlement re-reads the state under one.
const conversationInFlightQuery = `SELECT id, organization_id, project_id FROM conversations WHERE (json_extract(execution_json, '$.status') IN ('starting', 'running', 'waiting_input', 'interrupting')
 OR (json_extract(execution_json, '$.status') IN ('completed', 'interrupted', 'failed', 'unknown') AND COALESCE(json_extract(execution_json, '$.settled'), 0) = 0 AND COALESCE(json_extract(execution_json, '$.lease_id'), '') <> '')) `

func (c *conversationService) inFlightExecutions(ctx context.Context, query string, args ...any) ([]conversationExecutionRef, error) {
	rows, err := c.server.database.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list in-flight conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []conversationExecutionRef
	for rows.Next() {
		var ref conversationExecutionRef
		if err := rows.Scan(&ref.id, &ref.organization, &ref.project); err != nil {
			return nil, fmt.Errorf("scan in-flight conversation: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list in-flight conversations: %w", err)
	}
	return refs, nil
}

// settleLostExecution re-checks the owner lease inside a transaction and
// applies the interrupted transition when it is released or expired.
func (c *conversationService) settleLostExecution(ctx context.Context, ref conversationExecutionRef) error {
	var record conversationRecord
	changed := false
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		record, err = c.store.readConversation(ctx, tx, ref.organization, ref.project, ref.id)
		if err != nil {
			return err
		}
		execution := record.Execution
		if (execution.Status.Terminal() && execution.Settled) || execution.Status == conversation.ExecutionIdle || execution.Status == conversation.ExecutionWaitingForRunner {
			return nil
		}
		if execution.Owner.LeaseID == "" && execution.Owner.AttemptID == "" {
			// A hub-side coordinator turn runs in this process and holds no
			// lease and no attempt. Reconciliation is about workers that
			// stopped reporting; it must not interrupt a live in-process turn.
			return nil
		}
		if execution.Owner.LeaseID != "" {
			lease, found, err := readLeaseByID(ctx, tx, tracker.LeaseID(execution.Owner.LeaseID))
			if err != nil {
				return err
			}
			if found && lease.session.FencingToken == tracker.FencingToken(execution.Owner.FencingToken) && lease.session.ReleasedAt == nil && lease.session.ExpiresAt.After(now) {
				return nil
			}
		}
		changed = true
		if execution.Status.Terminal() {
			// The turn already reported how it ended; only the unbind that
			// would have settled its controls is missing, and the lost lease
			// means it will never arrive.
			return c.finishExecution(ctx, tx, &record, execution.Status, execution.Error, true, now)
		}
		return c.finishExecution(ctx, tx, &record, conversation.ExecutionInterrupted, conversationLeaseLostError, false, now)
	})
	if err != nil {
		return err
	}
	if changed {
		c.committed(record)
	}
	return nil
}
