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
	Continuation   bool   `json:"continuation"`
	// Preferences are the conversation's turn preferences. The runner
	// applies the explicit ones to its turn request and leaves "auto" to the
	// project's configured defaults.
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
	// registry can see that the hub did not hand it back.
	// Empty when the bind resumed nothing and started no turn.
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
	// the conversation's read rule.
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
	// coordinator proposal.
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
		return nativePublicInvalid("lease_id, fencing_token and attempt_id are required")
	}
	return nil
}

func conversationIdentityFromHeaders(c echo.Context) (conversationWorkerIdentity, error) {
	header := c.Request().Header
	token, err := strconv.ParseInt(strings.TrimSpace(header.Get(conversationHeaderFencingToken)), 10, 64)
	if err != nil {
		return conversationWorkerIdentity{}, nativePublicInvalid("A numeric fencing token header is required")
	}
	identity := conversationWorkerIdentity{
		LeaseID:      tracker.LeaseID(strings.TrimSpace(header.Get(conversationHeaderLease))),
		FencingToken: tracker.FencingToken(token),
		AttemptID:    strings.TrimSpace(header.Get(conversationHeaderAttempt)),
	}
	return identity, identity.validate()
}

func conversationWorkerAuthorityUnavailable() error {
	return &nativeError{Code: "unavailable", Message: "Worker identity authority is unavailable", status: http.StatusServiceUnavailable}
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
func (c *conversationService) readLinkedConversation(ctx context.Context, tx nativeQueryer, scope nativeScope, item string) (conversationRecord, error) {
	record, err := c.store.readLinkedConversationRecord(ctx, tx, scope, item)
	if err != nil {
		return record, err
	}
	if err := c.authorizeRead(ctx, tx, scope, record); err != nil {
		return record, err
	}
	return record, nil
}

func (s *conversationStore) readLinkedConversationRecord(ctx context.Context, tx nativeQueryer, scope nativeScope, item string) (conversationRecord, error) {
	columns := conversationReadColumns
	if plan, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok && plan.record.WorkItemID == item {
		columns = conversationReadProjection(ctx, tx, plan.record.ID)
	}
	record, err := s.readConversationRecord(ctx, tx, tx.QueryRowContext(ctx, "SELECT "+columns+" FROM conversations WHERE organization_id = ? AND project_id = ? AND work_item_id = ?", scope.organization, scope.project, item))
	if errors.Is(err, sql.ErrNoRows) {
		return record, nativeNotFound()
	}
	if err != nil {
		return record, fmt.Errorf("read linked conversation: %w", err)
	}
	return record, nil
}

func (c *conversationService) ensureWorkerConversation(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, now time.Time) (conversationRecord, error) {
	var record conversationRecord
	var err error
	if scope.credential.Scope == apiScopeWorker {
		record, err = c.store.readLinkedConversationRecord(ctx, tx, scope, item)
		if err == nil {
			readable := record
			readable.ReadAudience = ""
			err = c.authorizeRead(ctx, tx, scope, readable)
		}
	} else {
		record, err = c.readLinkedConversation(ctx, tx, scope, item)
	}
	if err == nil {
		return record, nil
	}
	var failure *nativeError
	if !errors.As(err, &failure) || failure.Code != "not_found" || record.ID != "" {
		return conversationRecord{}, err
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return conversationRecord{}, err
	}
	record = conversationRecord{
		ID: conversation.NewConversationID(), OrganizationID: scope.organization, ProjectID: scope.project,
		OwnerPrincipalID: scope.credential.ID, Title: deriveConversationTitle(issue.Title),
		Origin:     conversationOriginWorker,
		Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
		WorkItemID: item, LinkedAt: &now,
		Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	if issue.Actor.PrincipalID != "" {
		var owner string
		switch err := tx.QueryRowContext(ctx, "SELECT id FROM api_tokens WHERE id = ?", issue.Actor.PrincipalID).Scan(&owner); {
		case err == nil:
			record.OwnerPrincipalID = owner
		case !errors.Is(err, sql.ErrNoRows):
			return conversationRecord{}, fmt.Errorf("read issue actor token: %w", err)
		}
	}
	if p, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok && p.creation != nil {
		record = *p.creation
	}
	if err := c.store.createConversation(ctx, tx, &record); err != nil {
		return conversationRecord{}, err
	}
	return record, nil
}

// readReceipt returns the stored receipt for a command key; found is false
// when the key was never reserved.
func (c *conversationService) readReceipt(ctx context.Context, tx nativeQueryer, conversationID, key string) (receipt conversation.Receipt, found bool, err error) {
	var encoded, contentID string
	err = tx.QueryRowContext(ctx, "SELECT receipt_json,content_attachment_id FROM conversation_commands WHERE conversation_id = ? AND key = ?", conversationID, key).Scan(&encoded, &contentID)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.Receipt{}, false, nil
	}
	if err != nil {
		return conversation.Receipt{}, false, fmt.Errorf("read receipt: %w", err)
	}
	decoded, err := c.store.decodeReceipt(ctx, tx, conversationID, encoded, contentID)
	if err != nil || decoded == nil {
		return conversation.Receipt{}, false, err
	}
	return *decoded, true, nil
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

// Handlers.

func (r *conversationWorkerRouter) bind(e echo.Context) error {
	s := r.service.server
	var request conversationBindRequest
	if err := decodeAPIJSON(e, &request); err != nil {
		return s.invalidAPIRequest(e, err)
	}
	if err := request.validate(); err != nil {
		return s.nativeAPIError(e, err)
	}
	if strings.TrimSpace(request.RunID) == "" {
		return s.nativeAPIError(e, nativePublicInvalid("run_id is required"))
	}

	return r.bindPostgres(e, request)
}

func (r *conversationWorkerRouter) unbind(e echo.Context) error {
	s := r.service.server
	var request conversationUnbindRequest
	if err := decodeAPIJSON(e, &request); err != nil {
		return s.invalidAPIRequest(e, err)
	}
	if err := request.validate(); err != nil {
		return s.nativeAPIError(e, err)
	}
	status, ok := map[string]conversation.ExecutionStatus{
		"succeeded": conversation.ExecutionCompleted, "failed": conversation.ExecutionFailed,
		"cancelled": conversation.ExecutionInterrupted, "interrupted": conversation.ExecutionInterrupted,
	}[request.Outcome]
	if !ok {
		return s.nativeAPIError(e, nativePublicInvalid("outcome must be succeeded, failed, cancelled or interrupted"))
	}

	return r.unbindPostgres(e, request, status)
}

func (r *conversationWorkerRouter) controls(e echo.Context) error {
	s := r.service.server
	identity, err := conversationIdentityFromHeaders(e)
	if err != nil {
		return s.nativeAPIError(e, err)
	}
	after, wait, err := conversationControlQuery(e)
	if err != nil {
		return s.nativeAPIError(e, err)
	}
	id := e.Param("conversation")

	return r.controlsPostgres(e, id, identity, after, wait)
}

func conversationControlQuery(e echo.Context) (after int64, wait time.Duration, err error) {
	if raw := strings.TrimSpace(e.QueryParam("after")); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			return 0, 0, nativePublicInvalid("after must be a non-negative cursor")
		}
	}
	if raw := strings.TrimSpace(e.QueryParam("wait")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 {
			return 0, 0, nativePublicInvalid("wait must be a number of seconds")
		}
		wait = min(time.Duration(seconds)*time.Second, conversationControlMaxWait)
	}
	return after, wait, nil
}

// pollControls acknowledges controls up to the cursor and hands off the next
// queued controls in one transaction. Nothing is committed when the poll
// changed nothing, so an idle worker never produces events.

func (r *conversationWorkerRouter) turnEvents(e echo.Context) error {
	s := r.service.server
	var request conversationTurnEventsRequest
	if err := decodeAPIJSON(e, &request); err != nil {
		return s.invalidAPIRequest(e, err)
	}
	if err := request.validate(); err != nil {
		return s.nativeAPIError(e, err)
	}
	if len(request.Events) > conversationMaxTurnEvents {
		return s.nativeAPIError(e, nativePublicInvalid("Too many events in one batch"))
	}
	request.BatchKey = strings.TrimSpace(request.BatchKey)
	if len(request.BatchKey) > conversation.MaxCommandKeyBytes {
		return s.nativeAPIError(e, nativePublicInvalid(fmt.Sprintf("batch_key must be at most %d bytes", conversation.MaxCommandKeyBytes)))
	}
	id := e.Param("conversation")

	return r.turnEventsPostgres(e, id, request)
}

// conversationItemData validates the optional structured data of an item
// event: a bounded JSON object, never an array or a bare value.
func conversationItemData(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > conversationMaxItemDataBytes {
		return nil, nativePublicInvalid("item data is too long")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, nativePublicInvalid("item data must be a JSON object")
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

// Lease loss.

// leaseReleased settles the execution bound to a lease right after the
// lease's release committed. Failures are logged: the release itself has
// already succeeded and the reconcile loop retries.
func (c *conversationService) leaseReleased(ctx context.Context, leaseID tracker.LeaseID) {
	organization, err := c.lifecycleOrganization()
	if err == nil {
		err = c.reconcileExecutionsPG(ctx, organization)
	}
	if err != nil {
		c.logger.Warn("conversation lease release reconcile failed", "lease", leaseID, "error", err)
	}
}

func (c *conversationService) reconcileExecutions(ctx context.Context) error {
	organization, err := c.lifecycleOrganization()
	if err != nil {
		return err
	}
	return c.reconcileExecutionsPG(ctx, organization)
}
