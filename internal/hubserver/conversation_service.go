package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// ConversationConfig enables the conversation product on the hub.
type ConversationConfig struct {
	// Enabled mounts the conversation API and streams.
	Enabled bool
	// Model is the model "auto" resolves to for this hub, offered beside the
	// models the runners report.
	Model string
	// QuestionTimeout bounds how long a pending runner question waits for
	// an answer before it expires. Zero selects the runner default.
	QuestionTimeout time.Duration
	// ControlQueueSize bounds pending controls per conversation. Zero
	// selects 64.
	ControlQueueSize int
	// SettleWindow is how long a conversation whose execution has ended or
	// never started may sit without activity before it settles. Zero selects
	// defaultConversationSettleWindow (decisions section 14).
	SettleWindow time.Duration
}

const (
	defaultConversationControlQueueSize = 64
	// defaultConversationQuestionTimeout bounds how long a turn waits for a
	// human answer when the configuration names no timeout.
	defaultConversationQuestionTimeout = 24 * time.Hour
	// defaultConversationSettleWindow is the project settle window when the
	// configuration names none (decisions section 14).
	defaultConversationSettleWindow = 24 * time.Hour
	// conversationSettleBatch bounds one settle sweep, and
	// conversationReferencePage bounds one "referenced from" listing.
	conversationSettleBatch       = 100
	conversationReferencePage     = 100
	conversationHeartbeatInterval = 15 * time.Second
	conversationAuthorizeInterval = 30 * time.Second
	conversationEventPage         = 200
	conversationMessagePage       = 50
	// conversationSnapshotQuestions bounds the questions a snapshot carries.
	conversationSnapshotQuestions = 20
)

func (c ConversationConfig) normalized() ConversationConfig {
	if c.ControlQueueSize <= 0 {
		c.ControlQueueSize = defaultConversationControlQueueSize
	}
	if c.QuestionTimeout <= 0 {
		c.QuestionTimeout = defaultConversationQuestionTimeout
	}
	if c.SettleWindow <= 0 {
		c.SettleWindow = defaultConversationSettleWindow
	}
	return c
}

// conversationService owns the conversation product inside the hub: durable
// storage, event fan-out to subscribers and the shared authorization rules.
// HTTP handlers and worker bindings live in sibling files. The hub has no
// coordinator yet, so a message in a conversation without a linked issue is
// saved and waits for one.
type conversationService struct {
	server *Service
	store  *conversationStore
	broker *conversationBroker
	config ConversationConfig
	logger *slog.Logger
	// controls delivers committed controls to the bound worker of a linked
	// conversation. It is woken after a control committed.
	controls conversationControlRouter
	// settle stops the settle sweep; settled waits for it to exit.
	settle  chan struct{}
	settled chan struct{}
	once    sync.Once
}

// conversationControlRouter hands committed controls to bound workers.
// Wake signals that queued controls may exist for the conversation.
type conversationControlRouter interface {
	Wake(conversationID string)
	Stop()
}

func newConversationService(server *Service, cfg ConversationConfig) *conversationService {
	service := &conversationService{
		server: server,
		store:  newConversationStore(server.database.db),
		broker: newConversationBroker(),
		config: cfg,
		logger: server.config.Logger.With("component", "conversation"),
	}
	service.controls = newConversationControlRouter(service)
	service.settle = make(chan struct{})
	service.settled = make(chan struct{})
	go service.settleLoop()
	return service
}

// committed must be called after every transaction that appended events:
// it wakes stream subscribers and the component that owns the next step.
func (c *conversationService) committed(record conversationRecord) {
	c.broker.notify(record.ID)
	if record.WorkItemID != "" {
		c.controls.Wake(record.ID)
	}
}

// start normalizes state left behind by a previous process so that no
// in-flight delivery or execution is reported as live after a restart.
func (c *conversationService) start(ctx context.Context) error {
	now, err := c.server.database.currentTime()
	if err != nil {
		return err
	}
	summary, err := c.store.normalizeAfterRestart(ctx, now)
	if err != nil {
		return err
	}
	c.logger.Info("conversation.restart_normalized",
		"conversations", summary.Conversations, "messages", summary.Messages,
		"questions", summary.Questions, "receipts", summary.Receipts)
	return c.wakePending(ctx)
}

// settleLoop settles conversations that have been finished and idle for the
// project's settle window. The sweep is periodic rather than scheduled: a
// conversation that wakes in the meantime is simply not a candidate when the
// sweep next runs (decisions section 14).
func (c *conversationService) settleLoop() {
	defer close(c.settled)
	ticker := time.NewTicker(conversationSettleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.settle:
			return
		case <-ticker.C:
			now, err := c.server.database.currentTime()
			if err != nil {
				c.logger.Warn("conversation.settle_sweep_failed", "error", err)
				continue
			}
			settled, err := c.settleIdleConversations(context.Background(), now)
			if err != nil {
				c.logger.Warn("conversation.settle_sweep_failed", "error", err)
				continue
			}
			if settled > 0 {
				c.logger.Info("conversation.settle_sweep", "settled", settled)
			}
			// Abandoned uploads go with the same tick: nobody sent them and
			// their bytes are the expensive part (section 17.1).
			swept, err := c.sweepAttachments(context.Background())
			if err != nil {
				c.logger.Warn("conversation.attachment_sweep_failed", "error", err)
				continue
			}
			if swept > 0 {
				c.logger.Info("conversation.attachment_sweep", "swept", swept)
			}
		}
	}
}

// wakePending resumes work that was accepted before a restart: linked
// conversations with queued controls are handed to the control router.
// Nothing is replayed; the router re-reads durable state.
func (c *conversationService) wakePending(ctx context.Context) error {
	rows, err := c.store.db.QueryContext(ctx, `SELECT DISTINCT m.conversation_id FROM conversation_messages m
JOIN conversations c ON c.id = m.conversation_id
WHERE m.role = 'user' AND m.delivery IN ('saved', 'queued') AND c.work_item_id IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("list pending conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	controls := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan pending conversation: %w", err)
		}
		controls++
		c.controls.Wake(id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.logger.Info("conversation.wake_pending", "controls", controls)
	return nil
}

func (c *conversationService) stop() {
	c.once.Do(func() {
		close(c.settle)
		<-c.settled
	})
	c.controls.Stop()
	c.broker.closeAll()
}

// conversationBroker wakes subscribers when a conversation gained events.
// It carries no payload: subscribers read the durable event log from their
// cursor, so a slow subscriber can never lose or reorder events.
type conversationBroker struct {
	mu          sync.Mutex
	closed      bool
	subscribers map[string]map[*conversationSubscription]struct{}
}

type conversationSubscription struct {
	conversationID string
	wake           chan struct{}
	closed         chan struct{}
	once           sync.Once
}

func newConversationBroker() *conversationBroker {
	return &conversationBroker{subscribers: map[string]map[*conversationSubscription]struct{}{}}
}

// subscribe registers interest in a conversation. The returned wake channel
// receives at most one pending signal; closed is closed when the broker
// shuts down. Call cancel when done.
func (b *conversationBroker) subscribe(conversationID string) (subscription *conversationSubscription, cancel func()) {
	subscription = &conversationSubscription{conversationID: conversationID, wake: make(chan struct{}, 1), closed: make(chan struct{})}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		subscription.close()
		return subscription, func() {}
	}
	set, ok := b.subscribers[conversationID]
	if !ok {
		set = map[*conversationSubscription]struct{}{}
		b.subscribers[conversationID] = set
	}
	set[subscription] = struct{}{}
	return subscription, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if set, ok := b.subscribers[conversationID]; ok {
			delete(set, subscription)
			if len(set) == 0 {
				delete(b.subscribers, conversationID)
			}
		}
		subscription.close()
	}
}

func (s *conversationSubscription) close() {
	s.once.Do(func() { close(s.closed) })
}

// notify wakes every subscriber of the conversation. Call it after the
// transaction that appended events has committed.
func (b *conversationBroker) notify(conversationID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for subscription := range b.subscribers[conversationID] {
		select {
		case subscription.wake <- struct{}{}:
		default:
		}
	}
}

func (b *conversationBroker) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, set := range b.subscribers {
		for subscription := range set {
			subscription.close()
		}
	}
	b.subscribers = map[string]map[*conversationSubscription]struct{}{}
}

// Error codes specific to conversations, in addition to the shared native
// vocabulary (not_found, invalid_request, idempotency_conflict).
func conversationForbidden(message string) error {
	return &nativeError{Code: "forbidden", Message: message, status: http.StatusForbidden}
}

func conversationStale(message string) error {
	return nativeStaleExecution(message)
}

// conversationStaleOwner reports an owner-generation mismatch with both
// attempts in details so the client can tell a lost race from a stale tab
// without a second request. Either identifier may be null.
func conversationStaleOwner(message, expected, current string) error {
	return &nativeError{
		Code: "stale_execution", Message: message, status: http.StatusConflict,
		Details: map[string]any{"expected_attempt_id": conversationOptional(expected), "current_attempt_id": conversationOptional(current)},
	}
}

// conversationRequireOwner checks the expected owner generation of a control
// and reports a mismatch as stale_execution carrying both attempts.
func conversationRequireOwner(owner conversation.Owner, expected conversation.Expected) error {
	if err := owner.Matches(expected); err != nil {
		return conversationStaleOwner(err.Error(), expected.AttemptID, owner.AttemptID)
	}
	return nil
}

// staleExecutionAttempts reports whether err is a stale_execution rejection
// and returns the expected and current attempt identifiers it carries. A
// rejection raised without a generation comparison carries neither.
func staleExecutionAttempts(err error) (expected, current string, ok bool) {
	var native *nativeError
	if !errors.As(err, &native) || native.Code != "stale_execution" {
		return "", "", false
	}
	read := func(key string) string {
		value, found := native.Details[key].(*string)
		if !found || value == nil {
			return ""
		}
		return *value
	}
	return read("expected_attempt_id"), read("current_attempt_id"), true
}

// logStaleExecution reports a rejected control whose owner generation is no
// longer current. It is called once per rejected request, at the boundary
// that answers it, so path names the API command kind or the worker
// endpoint that refused the control. Anything else is ignored.
func (c *conversationService) logStaleExecution(path, conversationID string, err error) {
	expected, current, ok := staleExecutionAttempts(err)
	if !ok {
		return
	}
	c.logger.Warn("conversation.stale_execution",
		"conversation_id", conversationID, "expected_attempt_id", expected,
		"current_attempt_id", current, "path", path)
}

func conversationAlreadyLinked(existing string) error {
	err := &nativeError{Code: "conversation_already_linked", Message: "The conversation or issue is already linked", status: http.StatusConflict}
	if existing != "" {
		err.Details = map[string]any{"existing_conversation_id": existing}
	}
	return err
}

func conversationQuestionAnswered() error {
	return &nativeError{Code: "question_already_answered", Message: "The question has already been answered", status: http.StatusConflict}
}

func conversationUnsupported(message string) error {
	return &nativeError{Code: "unsupported_control", Message: message, status: http.StatusUnprocessableEntity}
}

func conversationShareRequired() error {
	return &nativeError{Code: "share_history_required", Message: "Linking shares the conversation history; confirm with share_history", status: http.StatusUnprocessableEntity}
}

func conversationProjectFixed() error {
	return &nativeError{Code: "conversation_project_fixed", Message: "Conversations never move between projects", status: http.StatusUnprocessableEntity}
}

func conversationQueueFull() error {
	return &nativeError{Code: "queue_full", Message: "The control queue is full; retry later", status: http.StatusServiceUnavailable}
}

// translateConversationError maps store errors onto API errors.
func translateConversationError(err error) error {
	var linked *errConversationLinked
	if errors.As(err, &linked) {
		return conversationAlreadyLinked(linked.ExistingConversationID)
	}
	var revision *errConversationRevision
	if errors.As(err, &revision) {
		return &nativeError{Code: "revision_conflict", Message: "The conversation changed; reload and retry", status: http.StatusConflict}
	}
	if errors.Is(err, conversation.ErrStaleExecution) {
		return conversationStale(err.Error())
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nativeNotFound()
	}
	return err
}

// authorizeRead applies the audience rules from the decisions document:
// the record must belong to the request scope, private conversations are
// visible only to their owner, and every denial is an opaque not_found.
func (c *conversationService) authorizeRead(scope nativeScope, record conversationRecord) error {
	if record.OrganizationID != scope.organization || record.ProjectID != scope.project {
		return nativeNotFound()
	}
	if record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.credential.ID {
		return nativeNotFound()
	}
	return nil
}

// authorizeWrite requires read access, an active conversation and project
// write capability. Viewers and read-only grants cannot send commands.
func (c *conversationService) authorizeWrite(ctx context.Context, query nativeQueryer, scope nativeScope, record conversationRecord) error {
	if err := c.authorizeRead(scope, record); err != nil {
		return err
	}
	if scope.credential.Hosted != nil {
		if err := c.server.requireHostedProject(ctx, query, scope, true); err != nil {
			return conversationForbidden("Write access to the project is required")
		}
		return nil
	}
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin && scope.credential.Scope != apiScopeWorker {
		return conversationForbidden("Write access to the project is required")
	}
	return nil
}

// actorFor describes the request principal for message attribution.
func conversationActorFor(scope nativeScope) conversation.Actor {
	if scope.credential.Runner.RunnerID != "" || scope.credential.Scope == apiScopeWorker {
		return conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: scope.credential.ID}
	}
	return conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: scope.credential.ID}
}

// Public resource projections (contract section 5).

type conversationOwnerResource struct {
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
}

type conversationExecutionResource struct {
	Status conversation.ExecutionStatus `json:"status"`
	// The owner identifiers are null rather than empty when no attempt owns
	// the conversation; the client tells "absent" from "known" by null.
	AttemptID *string `json:"attempt_id"`
	RunID     *string `json:"run_id"`
	RunnerID  *string `json:"runner_id"`
	ThreadID  *string `json:"thread_id"`
	TurnID    *string `json:"turn_id"`
	// Resume reports what the bound runner continued from: its own provider
	// thread, a transcript, or nothing (decisions section 10.4).
	Resume       string                    `json:"resume"`
	Capabilities conversation.Capabilities `json:"capabilities"`
	Error        *string                   `json:"error"`
	UpdatedAt    time.Time                 `json:"updated_at"`
}

// conversationWorkItemResource is the linked issue as the client renders it:
// enough to show and open the issue card without a second request.
type conversationWorkItemResource struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Lane       string `json:"lane"`
	// RunnerBound reports that an attempt currently owns the issue, which
	// is what the client shows instead of "waiting for runner".
	RunnerBound bool `json:"runner_bound"`
}

type conversationResource struct {
	ID             string                  `json:"id"`
	OrganizationID tracker.OrganizationID  `json:"organization_id"`
	ProjectID      tracker.ProjectID       `json:"project_id"`
	Title          string                  `json:"title"`
	Visibility     conversation.Visibility `json:"visibility"`
	Status         conversation.Status     `json:"status"`
	// Preferences are the conversation's turn preferences; each field is
	// "auto" or an explicit value (decisions section 14).
	Preferences   conversation.Preferences      `json:"preferences"`
	WorkItemID    *string                       `json:"work_item_id"`
	LinkedAt      *time.Time                    `json:"linked_at"`
	WorkItem      *conversationWorkItemResource `json:"work_item"`
	Owner         conversationOwnerResource     `json:"owner"`
	Execution     conversationExecutionResource `json:"execution"`
	Revision      int64                         `json:"revision"`
	EventSeq      int64                         `json:"event_seq"`
	CreatedAt     time.Time                     `json:"created_at"`
	UpdatedAt     time.Time                     `json:"updated_at"`
	LastMessageAt *time.Time                    `json:"last_message_at"`
	// MessageCount is the whole history, so the share confirmation can say
	// how much becomes readable (decisions section 10.5).
	MessageCount int64 `json:"message_count"`
}

type conversationMessageResource struct {
	ID             string                   `json:"id"`
	ConversationID string                   `json:"conversation_id"`
	Seq            int64                    `json:"seq"`
	Role           conversation.Role        `json:"role"`
	Kind           conversation.MessageKind `json:"kind"`
	Text           string                   `json:"text"`
	Data           any                      `json:"data"`
	Delivery       conversation.Delivery    `json:"delivery"`
	AttemptID      *string                  `json:"attempt_id"`
	TurnID         *string                  `json:"turn_id"`
	ProviderItemID *string                  `json:"provider_item_id"`
	Actor          conversation.Actor       `json:"actor"`
	CommandKey     *string                  `json:"command_key"`
	// References are the issues and conversations the text names, resolved
	// against the project (decisions section 14).
	References []conversationReferenceResource `json:"references"`
	// Attachments are the uploads the message carries, each with the URL
	// that streams it (decisions section 17.1).
	Attachments []conversation.Attachment `json:"attachments"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
}

// conversationReferenceResource is one resolved reference as the client
// renders it: a link with a label.
type conversationReferenceResource struct {
	Kind  conversation.ReferenceKind `json:"kind"`
	ID    string                     `json:"id"`
	Label string                     `json:"label"`
	URL   string                     `json:"url"`
}

// conversationReferenceURL is the client route a reference opens.
func conversationReferenceURL(kind conversation.ReferenceKind, id string) string {
	if kind == conversation.ReferenceConversation {
		return "/chat/c/" + id
	}
	return "/work/i/" + id
}

func projectReferences(references []conversationMessageReference) []conversationReferenceResource {
	projected := make([]conversationReferenceResource, 0, len(references))
	for _, reference := range references {
		projected = append(projected, conversationReferenceResource{
			Kind: reference.Kind, ID: reference.ID, Label: reference.Label,
			URL: conversationReferenceURL(reference.Kind, reference.ID),
		})
	}
	return projected
}

func conversationOptional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func projectExecution(execution conversation.Execution) conversationExecutionResource {
	status := execution.Status
	if status == "" {
		status = conversation.ExecutionIdle
	}
	return conversationExecutionResource{
		Status:       status,
		AttemptID:    conversationOptional(execution.Owner.AttemptID),
		RunID:        conversationOptional(execution.Owner.RunID),
		RunnerID:     conversationOptional(execution.Owner.RunnerID),
		ThreadID:     conversationOptional(execution.Owner.ThreadID),
		TurnID:       conversationOptional(execution.Owner.TurnID),
		Resume:       execution.Resume,
		Capabilities: execution.Capabilities,
		Error:        conversationOptional(execution.Error),
		UpdatedAt:    execution.UpdatedAt,
	}
}

// conversationRunnerBound reports whether an attempt currently owns the
// conversation: an attempt identifier that has not reached a terminal
// execution status.
func conversationRunnerBound(execution conversation.Execution) bool {
	return execution.Owner.AttemptID != "" && !execution.Status.Terminal()
}

// projectWorkItem renders the linked issue summary. An unlinked conversation,
// or one whose issue could not be resolved, projects null.
func projectWorkItem(record conversationRecord) *conversationWorkItemResource {
	if record.WorkItemID == "" || record.WorkItem == nil {
		return nil
	}
	return &conversationWorkItemResource{
		ID:          record.WorkItem.ID,
		Identifier:  record.WorkItem.Identifier,
		Title:       record.WorkItem.Title,
		Lane:        record.WorkItem.Lane,
		RunnerBound: conversationRunnerBound(record.Execution),
	}
}

// conversationIssueResult is the data.issue payload of the status message a
// completed handoff appends. It survives a reload and reaches other tabs,
// so the result card does not depend on the linking response.
func conversationIssueResult(record conversationRecord) map[string]any {
	item := projectWorkItem(record)
	if item == nil {
		return nil
	}
	return map[string]any{
		"id": item.ID, "identifier": item.Identifier, "title": item.Title,
		"state": item.Lane, "lane": item.Lane, "runner_bound": item.RunnerBound,
	}
}

// conversationWorkItem is the linked issue summary cached on a conversation
// record so that every projection reports the same identity without a
// second lookup. runner_bound is not cached: it comes from the record's own
// execution state, which changes far more often than the issue.
type conversationWorkItem struct {
	ID         string
	Identifier string
	Title      string
	Lane       string
}

// resolveConversationWorkItems fills the linked issue summary of every linked
// record in one query. A record whose issue is no longer readable keeps a nil
// summary and projects work_item: null rather than failing the read.
func resolveConversationWorkItems(ctx context.Context, query nativeQueryer, records []*conversationRecord) error {
	linked := make([]string, 0, len(records))
	for _, record := range records {
		if record.WorkItemID != "" {
			linked = append(linked, record.WorkItemID)
		}
	}
	if len(linked) == 0 {
		return nil
	}
	ids, err := marshalNative(linked)
	if err != nil {
		return fmt.Errorf("encode linked work items: %w", err)
	}
	rows, err := query.QueryContext(ctx, `SELECT i.organization_id, i.project_id, i.native_id, p.name, COALESCE(i.number, 0), i.title, COALESCE(ws.detent_state, '')
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.native_id IN (SELECT value FROM json_each(?))`, ids)
	if err != nil {
		return fmt.Errorf("read linked work items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := map[string]conversationWorkItem{}
	for rows.Next() {
		var organization, project, id, name, title, lane string
		var number int64
		if err := rows.Scan(&organization, &project, &id, &name, &number, &title, &lane); err != nil {
			return fmt.Errorf("read linked work items: %w", err)
		}
		items[conversationWorkItemKey(organization, project, id)] = conversationWorkItem{
			ID: id, Identifier: fmt.Sprintf("%s#%d", name, number), Title: title, Lane: lane,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read linked work items: %w", err)
	}
	for _, record := range records {
		item, ok := items[conversationWorkItemKey(string(record.OrganizationID), string(record.ProjectID), record.WorkItemID)]
		if !ok {
			continue
		}
		record.WorkItem = &item
	}
	return nil
}

func conversationWorkItemKey(organization, project, workItem string) string {
	return organization + "/" + project + "/" + workItem
}

func projectConversation(record conversationRecord) conversationResource {
	return conversationResource{
		ID:             record.ID,
		OrganizationID: record.OrganizationID,
		ProjectID:      record.ProjectID,
		Title:          record.Title,
		Visibility:     record.Visibility,
		Status:         record.Status,
		Preferences:    record.Preferences.Normalized(),
		WorkItemID:     conversationOptional(record.WorkItemID),
		LinkedAt:       record.LinkedAt,
		WorkItem:       projectWorkItem(record),
		Owner:          conversationOwnerResource{PrincipalID: record.OwnerPrincipalID, Subject: record.OwnerSubject},
		Execution:      projectExecution(record.Execution),
		Revision:       record.Revision,
		EventSeq:       record.EventSeq,
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
		LastMessageAt:  record.LastMessageAt,
		MessageCount:   record.MessageCount,
	}
}

func projectMessage(record conversationMessageRecord) conversationMessageResource {
	var data any = map[string]any{}
	if len(record.Data) > 0 && string(record.Data) != "null" {
		data = record.Data
	}
	return conversationMessageResource{
		ID:             record.ID,
		ConversationID: record.ConversationID,
		Seq:            record.Seq,
		Role:           record.Role,
		Kind:           record.Kind,
		Text:           record.Text,
		Data:           data,
		Delivery:       record.Delivery,
		AttemptID:      conversationOptional(record.AttemptID),
		TurnID:         conversationOptional(record.TurnID),
		ProviderItemID: conversationOptional(record.ProviderItemID),
		Actor:          record.Actor,
		CommandKey:     conversationOptional(record.CommandKey),
		References:     projectReferences(record.References),
		Attachments:    projectAttachments(record.Attachments),
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
	}
}

// loadConversation reads a conversation for the request scope and applies
// the read rule. Any failure is an API error.
func (c *conversationService) loadConversation(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (conversationRecord, error) {
	if err := conversation.ValidateConversationID(id); err != nil {
		return conversationRecord{}, nativeNotFound()
	}
	record, err := c.store.readConversation(ctx, query, scope.organization, scope.project, id)
	if err != nil {
		return conversationRecord{}, translateConversationError(err)
	}
	if err := c.authorizeRead(scope, record); err != nil {
		return conversationRecord{}, err
	}
	return record, nil
}

// registerConversationRoutes mounts the API, the event stream and the
// client shell. The individual groups are implemented in sibling files.
func (s *Service) registerConversationRoutes(e *echo.Echo) {
	if s.conversations == nil {
		return
	}
	s.registerConversationAPIRoutes(e)
	s.registerConversationWorkerRoutes(e)
}

// Transactional helpers shared by the API, coordinator and worker paths.
// Each helper appends the durable event that describes the change inside
// the caller's transaction; the caller commits and then calls committed.

// appendMessage stores a new message and its message.accepted event. It
// fills identifiers and timestamps that are empty.
func (c *conversationService) appendMessage(ctx context.Context, tx *sql.Tx, record *conversationRecord, message *conversationMessageRecord, now time.Time) error {
	if message.ID == "" {
		message.ID = conversation.NewMessageID()
	}
	message.ConversationID = record.ID
	if message.CreatedAt.IsZero() {
		message.CreatedAt = now
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	if err := c.store.appendMessage(ctx, tx, message); err != nil {
		return err
	}
	if record.LastMessageAt == nil || record.LastMessageAt.Before(message.CreatedAt) {
		at := message.CreatedAt
		record.LastMessageAt = &at
	}
	if err := c.recordReferences(ctx, tx, *record, message, now); err != nil {
		return err
	}
	// A new message is activity: a settled conversation wakes before the
	// message is announced (decisions section 14).
	if err := c.unsettle(ctx, tx, record, now); err != nil {
		return err
	}
	if _, err := c.store.appendEvent(ctx, tx, record.ID, conversation.EventMessageAccepted, projectMessage(*message), now); err != nil {
		return err
	}
	return nil
}

// unsettle returns a settled conversation to active. Any accepted command or
// new message wakes it; there is no unarchive action (decisions section 14).
func (c *conversationService) unsettle(ctx context.Context, tx *sql.Tx, record *conversationRecord, now time.Time) error {
	if record.Status != conversation.StatusSettled {
		return nil
	}
	record.Status = conversation.StatusActive
	record.SettledAt = nil
	return c.saveConversation(ctx, tx, record, now)
}

// recordReferences extracts the references a message's text carries,
// resolves them in the conversation's scope and stores the ones that name
// something the hub can see. Extraction is bounded by
// conversation.MaxReferences, so one message can never write more rows than
// that.
func (c *conversationService) recordReferences(ctx context.Context, tx *sql.Tx, record conversationRecord, message *conversationMessageRecord, now time.Time) error {
	resolved, err := c.resolveReferences(ctx, tx, record, conversation.ExtractReferences(message.Text))
	if err != nil {
		return err
	}
	message.References = resolved
	return c.store.replaceMessageReferences(ctx, tx, *message, now)
}

// resolveReferences turns extracted tokens into stored references. A bare
// "#123" resolves inside the conversation's project; a qualified token
// resolves in the named project of the same organization. A token that names
// nothing the hub holds is dropped rather than stored as a dead link.
func (c *conversationService) resolveReferences(ctx context.Context, tx *sql.Tx, record conversationRecord, references []conversation.Reference) ([]conversationMessageReference, error) {
	if len(references) == 0 {
		return nil, nil
	}
	resolved := make([]conversationMessageReference, 0, len(references))
	for _, reference := range references {
		switch reference.Kind {
		case conversation.ReferenceConversation:
			found, ok, err := c.resolveConversationReference(ctx, tx, record, reference)
			if err != nil {
				return nil, err
			}
			if ok {
				resolved = append(resolved, found)
			}
		case conversation.ReferenceIssue:
			found, ok, err := c.resolveIssueReference(ctx, tx, record, reference)
			if err != nil {
				return nil, err
			}
			if ok {
				resolved = append(resolved, found)
			}
		}
	}
	return resolved, nil
}

// resolveConversationReference reports the referenced conversation; ok is
// false when the token names nothing in the organization, or names the
// conversation the message belongs to.
func (c *conversationService) resolveConversationReference(ctx context.Context, tx *sql.Tx, record conversationRecord, reference conversation.Reference) (found conversationMessageReference, ok bool, err error) {
	if reference.ConversationID == record.ID {
		// A conversation never references itself.
		return found, false, nil
	}
	var title string
	err = tx.QueryRowContext(ctx, "SELECT title FROM conversations WHERE id = ? AND organization_id = ?", reference.ConversationID, record.OrganizationID).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return found, false, nil
	}
	if err != nil {
		return found, false, fmt.Errorf("resolve conversation reference: %w", err)
	}
	return conversationMessageReference{Kind: conversation.ReferenceConversation, ID: reference.ConversationID, Label: reference.Text}, true, nil
}

// resolveIssueReference reports the referenced issue; ok is false when the
// number names no issue the hub holds in the named project.
func (c *conversationService) resolveIssueReference(ctx context.Context, tx *sql.Tx, record conversationRecord, reference conversation.Reference) (found conversationMessageReference, ok bool, err error) {
	query := "SELECT i.native_id FROM issues i WHERE i.organization_id = ? AND i.project_id = ? AND i.number = ?"
	args := []any{record.OrganizationID, record.ProjectID, reference.Number}
	if reference.Project != "" {
		// A qualified token names a project by name inside the same
		// organization; the organization segment must agree when present.
		query = `SELECT i.native_id FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
WHERE i.organization_id = ? AND lower(p.name) = ? AND i.number = ?`
		args = []any{record.OrganizationID, strings.ToLower(reference.Project), reference.Number}
		if reference.Organization != "" && !strings.EqualFold(reference.Organization, string(record.OrganizationID)) {
			return found, false, nil
		}
	}
	var workItemID string
	err = tx.QueryRowContext(ctx, query, args...).Scan(&workItemID)
	if errors.Is(err, sql.ErrNoRows) {
		return found, false, nil
	}
	if err != nil {
		return found, false, fmt.Errorf("resolve issue reference: %w", err)
	}
	return conversationMessageReference{Kind: conversation.ReferenceIssue, ID: workItemID, Label: reference.Text}, true, nil
}

// updateMessage persists a changed message and its message.updated event.
// A message that reached a terminal delivery has its final text, so its
// references are extracted then rather than on every streamed delta
// (decisions section 14). For any other delivery the caller's record must
// already carry its stored references, which every read path fills, because
// the event it emits replaces the client's copy of the message.
func (c *conversationService) updateMessage(ctx context.Context, tx *sql.Tx, message conversationMessageRecord, now time.Time) error {
	message.UpdatedAt = now
	if err := c.store.updateMessage(ctx, tx, message); err != nil {
		return err
	}
	if message.Delivery.Terminal() {
		record, err := c.store.readConversationByID(ctx, tx, message.ConversationID)
		if err != nil {
			return err
		}
		if err := c.recordReferences(ctx, tx, record, &message, now); err != nil {
			return err
		}
	}
	if _, err := c.store.appendEvent(ctx, tx, message.ConversationID, conversation.EventMessageUpdated, projectMessage(message), now); err != nil {
		return err
	}
	return nil
}

// appendDelta records streamed assistant text. The message row keeps the
// accumulated text so a snapshot is complete without replaying deltas.
func (c *conversationService) appendDelta(ctx context.Context, tx *sql.Tx, message *conversationMessageRecord, text string, now time.Time) error {
	offset := int64(len(message.Text))
	// The caller's record is only advanced once the write succeeded: a
	// failed write must not leave the accumulated text double-counted on
	// the next attempt.
	appended := *message
	appended.Text += text
	appended.UpdatedAt = now
	if err := c.store.updateMessage(ctx, tx, appended); err != nil {
		return err
	}
	*message = appended
	if _, err := c.store.appendEvent(ctx, tx, message.ConversationID, conversation.EventMessageDelta, conversationDeltaBody(message.ID, offset, text), now); err != nil {
		return err
	}
	return nil
}

// conversationDeltaBody is the message.delta event body. seq orders the
// deltas of one message and identifies a re-delivered one: it is the byte
// offset the text was appended at, so it is monotonic per message and stable
// across a retry.
func conversationDeltaBody(messageID string, seq int64, text string) map[string]any {
	return map[string]any{"message_id": messageID, "seq": seq, "text": text}
}

// saveConversation persists record changes with a revision check and emits
// conversation.updated.
func (c *conversationService) saveConversation(ctx context.Context, tx *sql.Tx, record *conversationRecord, now time.Time) error {
	record.UpdatedAt = now
	if err := c.store.updateConversation(ctx, tx, record, record.Revision); err != nil {
		return translateConversationError(err)
	}
	if _, err := c.store.appendEvent(ctx, tx, record.ID, conversation.EventConversationUpdated, projectConversation(*record), now); err != nil {
		return err
	}
	return nil
}

// updateExecution replaces the execution state and emits execution.updated
// in addition to conversation.updated.
func (c *conversationService) updateExecution(ctx context.Context, tx *sql.Tx, record *conversationRecord, execution conversation.Execution, now time.Time) error {
	execution.UpdatedAt = now
	record.Execution = execution
	if err := c.saveConversation(ctx, tx, record, now); err != nil {
		return err
	}
	if _, err := c.store.appendEvent(ctx, tx, record.ID, conversation.EventExecutionUpdated, projectExecution(execution), now); err != nil {
		return err
	}
	return nil
}

// recordReceipt stores the receipt and emits command.receipt. attemptID is
// the execution owner the receipt belongs to, empty when no attempt owns the
// conversation; it is logged so a receipt can be tied to a generation.
func (c *conversationService) recordReceipt(ctx context.Context, tx *sql.Tx, conversationID, attemptID string, receipt conversation.Receipt, now time.Time) error {
	receipt.UpdatedAt = now
	if err := c.store.updateReceipt(ctx, tx, conversationID, receipt.Key, receipt); err != nil {
		return err
	}
	if _, err := c.store.appendEvent(ctx, tx, conversationID, conversation.EventCommandReceipt, receipt, now); err != nil {
		return err
	}
	attributes := []any{"conversation_id", conversationID, "command_key", receipt.Key, "kind", receipt.Kind, "status", receipt.Status, "attempt_id", attemptID}
	if receipt.Error != nil {
		attributes = append(attributes, "error_code", receipt.Error.Code)
	}
	c.logger.Info("conversation.receipt", attributes...)
	return nil
}
