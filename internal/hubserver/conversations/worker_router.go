package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationReconcileInterval is how often the control router looks for
// executions whose lease was released or expired without an unbind.
var conversationReconcileInterval = 30 * time.Second

// WorkerRouter delivers committed controls to bound workers over
// the long-poll endpoint and reconciles executions whose lease was lost.
// The durable store is the queue: Wake only signals broker subscribers.
type WorkerRouter struct {
	server  *Server
	service *ConversationService
	stop    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func NewWorkerRouter(service *ConversationService) *WorkerRouter {
	return &WorkerRouter{server: service.Server, service: service, stop: make(chan struct{})}
}

func NewControlRouter(service *ConversationService) ConversationControlRouter {
	router := NewWorkerRouter(service)
	router.wg.Add(1)
	go router.reconcileLoop()
	return router
}

// Wake notifies long-polling workers that queued controls may exist.
func (r *WorkerRouter) Wake(conversationID string) {
	r.service.Broker.Notify(conversationID)
}

// Stop ends the reconcile loop and every in-flight long poll.
func (r *WorkerRouter) Stop() {
	r.once.Do(func() { close(r.stop) })
	r.wg.Wait()
}

func (r *WorkerRouter) reconcileLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(conversationReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			if err := r.service.ReconcileExecutions(context.Background()); err != nil {
				r.service.Logger.Warn("conversation execution reconcile failed", "error", err)
			}
		}
	}
}

// RegisterWorkerRoutes mounts the lease-fenced worker endpoints.
func (c *ConversationService) RegisterWorkerRoutes(e *echo.Group, worker echo.MiddlewareFunc) {
	router, ok := c.Controls.(*WorkerRouter)
	if !ok {
		return
	}
	e.POST(hubapi.NativeBase+"/work-items/:item/conversation/bind", router.Bind, worker)
	e.POST(hubapi.NativeBase+"/work-items/:item/conversation/unbind", router.Unbind, worker)
	e.GET(hubapi.NativeBase+"/conversations/:conversation/controls", router.Controls, worker)
	e.POST(hubapi.NativeBase+"/conversations/:conversation/turn-events", router.TurnEvents, worker)
}

// Wire shapes.

type conversationBindRequest struct {
	WorkerIdentity
	RunID        string                    `json:"run_id"`
	Capabilities conversation.Capabilities `json:"capabilities"`
	ThreadID     string                    `json:"thread_id,omitempty"`
}

type WorkerBindResponse struct {
	ConversationID string `json:"conversation_id"`
	Continuation   bool   `json:"continuation"`
	// Preferences are the conversation's turn preferences. The runner
	// applies the explicit ones to its turn request and leaves "auto" to the
	// project's configured defaults.
	Preferences conversation.Preferences   `json:"preferences"`
	Resume      ConversationResumeResource `json:"resume"`
	Pending     []WorkerControl      `json:"pending"`
	Cursor      int64                      `json:"cursor"`
}

type conversationUnbindRequest struct {
	WorkerIdentity
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

type conversationUnbindResponse struct {
	ConversationID string                        `json:"conversation_id"`
	Execution      ConversationExecutionResource `json:"execution"`
}

type WorkerControlsResponse struct {
	Controls []WorkerControl `json:"controls"`
	Cursor   int64                 `json:"cursor"`
}

// WorkerControl is the envelope handed to the worker. Its kind is the
// runner control vocabulary: a continuation intent is a user message.
type WorkerControl struct {
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
	WorkerIdentity
	// BatchKey identifies one worker batch and is repeated when the worker
	// retries it. A repeat applies nothing and answers with the event
	// sequence the first attempt produced, so a retried batch never appends
	// its deltas and items twice. It is optional; a worker that sends none
	// gets the old at-least-once behaviour.
	BatchKey string                  `json:"batch_key"`
	Events   []conversationTurnEvent `json:"events"`
}

type WorkerTurnEventsResponse struct {
	EventSeq int64 `json:"event_seq"`
}

// Ownership checks.

func conversationIdentityFromHeaders(c echo.Context) (WorkerIdentity, error) {
	header := c.Request().Header
	token, err := strconv.ParseInt(strings.TrimSpace(header.Get(ConversationHeaderFencingToken)), 10, 64)
	if err != nil {
		return WorkerIdentity{}, hubapi.NativePublicInvalid("A numeric fencing token header is required")
	}
	identity := WorkerIdentity{
		LeaseID:      tracker.LeaseID(strings.TrimSpace(header.Get(ConversationHeaderLease))),
		FencingToken: tracker.FencingToken(token),
		AttemptID:    strings.TrimSpace(header.Get(ConversationHeaderAttempt)),
	}
	return identity, identity.Validate()
}

// Transactions.

// transact runs fn in one transaction and commits when it returns nil. Only
// one transaction may be open at a time on the hub pool, so fn must never
// start another.

// Handlers.

func (r *WorkerRouter) Bind(e echo.Context) error {
	s := r.server
	var request conversationBindRequest
	if err := hubapi.DecodeAPIJSON(e, &request); err != nil {
		return s.invalidRequest(e, err)
	}
	if err := request.Validate(); err != nil {
		return s.apiError(e, err)
	}
	if strings.TrimSpace(request.RunID) == "" {
		return s.apiError(e, hubapi.NativePublicInvalid("run_id is required"))
	}

	return r.bindPostgres(e, request)
}

func (r *WorkerRouter) Unbind(e echo.Context) error {
	s := r.server
	var request conversationUnbindRequest
	if err := hubapi.DecodeAPIJSON(e, &request); err != nil {
		return s.invalidRequest(e, err)
	}
	if err := request.Validate(); err != nil {
		return s.apiError(e, err)
	}
	status, ok := map[string]conversation.ExecutionStatus{
		"succeeded": conversation.ExecutionCompleted, "failed": conversation.ExecutionFailed,
		"cancelled": conversation.ExecutionInterrupted, "interrupted": conversation.ExecutionInterrupted,
	}[request.Outcome]
	if !ok {
		return s.apiError(e, hubapi.NativePublicInvalid("outcome must be succeeded, failed, cancelled or interrupted"))
	}

	return r.unbindPostgres(e, request, status)
}

func (r *WorkerRouter) Controls(e echo.Context) error {
	s := r.server
	identity, err := conversationIdentityFromHeaders(e)
	if err != nil {
		return s.apiError(e, err)
	}
	after, wait, err := conversationControlQuery(e)
	if err != nil {
		return s.apiError(e, err)
	}
	id := e.Param("conversation")

	return r.controlsPostgres(e, id, identity, after, wait)
}

func conversationControlQuery(e echo.Context) (after int64, wait time.Duration, err error) {
	if raw := strings.TrimSpace(e.QueryParam("after")); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			return 0, 0, hubapi.NativePublicInvalid("after must be a non-negative cursor")
		}
	}
	if raw := strings.TrimSpace(e.QueryParam("wait")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 {
			return 0, 0, hubapi.NativePublicInvalid("wait must be a number of seconds")
		}
		wait = min(time.Duration(seconds)*time.Second, ConversationControlMaxWait)
	}
	return after, wait, nil
}

// pollControls acknowledges controls up to the cursor and hands off the next
// queued controls in one transaction. Nothing is committed when the poll
// changed nothing, so an idle worker never produces events.

func (r *WorkerRouter) TurnEvents(e echo.Context) error {
	s := r.server
	var request conversationTurnEventsRequest
	if err := hubapi.DecodeAPIJSON(e, &request); err != nil {
		return s.invalidRequest(e, err)
	}
	if err := request.Validate(); err != nil {
		return s.apiError(e, err)
	}
	if len(request.Events) > ConversationMaxTurnEvents {
		return s.apiError(e, hubapi.NativePublicInvalid("Too many events in one batch"))
	}
	request.BatchKey = strings.TrimSpace(request.BatchKey)
	if len(request.BatchKey) > conversation.MaxCommandKeyBytes {
		return s.apiError(e, hubapi.NativePublicInvalid(fmt.Sprintf("batch_key must be at most %d bytes", conversation.MaxCommandKeyBytes)))
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
	if len(raw) > ConversationMaxItemDataBytes {
		return nil, hubapi.NativePublicInvalid("item data is too long")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, hubapi.NativePublicInvalid("item data must be a JSON object")
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
