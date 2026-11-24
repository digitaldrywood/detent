package conversations

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// ConversationConfig enables the conversation product on the hub.
type ConversationConfig struct {
	ArchiveContents attachment.ArchiveContentStore `yaml:"-"`
	// Enabled mounts the conversation API and streams.
	Enabled bool
	// Backend runs coordinator turns for conversations without a linked
	// issue. When nil ordinary chat reports the coordinator as unavailable
	// and messages in unlinked conversations are saved until a link.
	Backend      runner.AgentBackend
	UsageSink    ConversationUsageSink
	ModelBackend func() (runner.AgentBackend, string, error)
	// Workspace is the directory coordinator turns run in. It must exist
	// when Backend is set.
	Workspace string
	// Model is the model "auto" resolves to for this hub, offered beside the
	// models the runners report, and the model coordinator turns use.
	Model string
	// ReasoningEffort is the effort coordinator turns use and the effort
	// "auto" resolves to in the composer.
	ReasoningEffort string
	// QuestionTimeout bounds how long a pending runner question waits for
	// an answer before it expires. Zero selects the runner default.
	QuestionTimeout time.Duration
	// ControlQueueSize bounds pending controls per conversation. Zero
	// selects 64.
	ControlQueueSize int
	// SettleWindow is how long a conversation whose execution has ended or
	// never started may sit without activity before it settles. Zero selects
	// defaultConversationSettleWindow.
	SettleWindow time.Duration
}

type ConversationUsage struct {
	OccurredAt     time.Time
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ConversationID string
	TurnID         string
	Provider       string
	Model          string
	Tokens         runner.AgentTokenCounts
	Outcome        conversation.Delivery
}

type ConversationUsageSink interface {
	RecordConversationUsage(context.Context, ConversationUsage) error
}

const (
	defaultConversationControlQueueSize = 64
	// defaultConversationQuestionTimeout bounds how long a turn waits for a
	// human answer when the configuration names no timeout.
	defaultConversationQuestionTimeout = 24 * time.Hour
	// defaultConversationSettleWindow is the project settle window when the
	// configuration names none.
	defaultConversationSettleWindow = 24 * time.Hour
	// conversationSettleBatch bounds one settle sweep, and
	// conversationReferencePage bounds one "referenced from" listing.
	conversationSettleBatch       = 100
	conversationReferencePage     = 100
	ConversationHeartbeatInterval = 15 * time.Second
	ConversationAuthorizeInterval = 30 * time.Second
	conversationEventPage         = 200
	ConversationMessagePage       = 50
	// conversationSnapshotQuestions bounds the questions a snapshot carries.
	ConversationSnapshotQuestions = 20
)

func (c ConversationConfig) Normalized() ConversationConfig {
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
// HTTP handlers, coordinator turns and worker bindings live in sibling files.
type ConversationService struct {
	Server *Server
	Store  *ConversationStore
	Broker *eventbroker.Broker
	Config ConversationConfig
	Logger *slog.Logger
	// coordinator answers messages in conversations without a linked
	// issue. It is woken after a message command committed.
	Coordinator ConversationCoordinator
	// controls delivers committed controls to the bound worker of a linked
	// conversation. It is woken after a control committed.
	Controls ConversationControlRouter
	// settle stops the settle sweep; settled waits for it to exit.
	Settle           chan struct{}
	Settled          chan struct{}
	SettleCancel     context.CancelFunc
	SettleTicks      <-chan time.Time
	StopSettleTicker func()
	lifecycleMu      sync.Mutex
	lifecycleCursors map[string]string
	once             sync.Once
}

// conversationCoordinator runs model turns for unlinked conversations. The
// durable store is the queue: Wake only signals that saved user messages
// may exist for the conversation.
type ConversationCoordinator interface {
	Available() bool
	Wake(conversationID string)
	// Cancel stops a running coordinator turn and reports whether one was
	// running.
	Cancel(conversationID string) bool
	// Hold stops and settles a running turn and keeps new turns from
	// starting until release is called.
	Hold(conversationID string) (release func(), err error)
	Stop()
}

// conversationControlRouter hands committed controls to bound workers.
// Wake signals that queued controls may exist for the conversation.
type ConversationControlRouter interface {
	Wake(conversationID string)
	Stop()
}

func NewConversationService(server *Server, cfg ConversationConfig) *ConversationService {
	if cfg.UsageSink == nil {
		cfg.UsageSink = server.deps.UsageSink
	}
	settleContext, settleCancel := context.WithCancel(context.Background())
	ticker := time.NewTicker(conversationSettleInterval)
	service := &ConversationService{
		Server:           server,
		Store:            NewConversationStore(nil),
		Broker:           server.deps.Broker,
		Config:           cfg,
		Logger:           server.deps.Logger.With("component", "conversation"),
		SettleCancel:     settleCancel,
		SettleTicks:      ticker.C,
		StopSettleTicker: ticker.Stop,
	}
	service.Store.ResolveWorkItems = server.deps.ResolveWorkItems
	service.Store.Contents = server.deps.Contents(cfg.ArchiveContents)
	service.Coordinator = server.deps.NewCoordinator(service)
	service.Controls = server.deps.NewControls(service)
	service.Settle = make(chan struct{})
	service.Settled = make(chan struct{})
	go service.SettleLoop(settleContext)
	return service
}

// committed must be called after every transaction that appended events:
// it wakes stream subscribers and the component that owns the next step.
func (c *ConversationService) Committed(record ConversationRecord) {
	c.Broker.Notify(record.ID)
	if record.WorkItemID == "" {
		c.Coordinator.Wake(record.ID)
		return
	}
	c.Controls.Wake(record.ID)
}

func (c *ConversationService) wakeCoordinator(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if coordinator, ok := c.Coordinator.(CommandDeliverer); ok {
		if err := coordinator.DeliverCoordinatorCommands(ctx, id); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Coordinator.Wake(id)
	return nil
}

func (c *ConversationService) Start(ctx context.Context) error {
	organization, err := c.LifecycleOrganization()
	if err != nil {
		return err
	}
	now := c.Server.config().Now()
	summary, err := c.normalizeAfterRestartPG(ctx, organization, now)
	if err != nil {
		return err
	}
	c.Logger.Info("conversation.restart_normalized",
		"conversations", summary.Conversations, "messages", summary.Messages,
		"questions", summary.Questions, "receipts", summary.Receipts)
	if err := c.ReconcileExecutionsPG(ctx, organization); err != nil {
		return err
	}
	return c.WakePendingPG(ctx, organization)
}

// settleLoop settles conversations that have been finished and idle for the
// project's settle window. The sweep is periodic rather than scheduled: a
// conversation that wakes in the meantime is simply not a candidate when the
// sweep next runs.
func (c *ConversationService) SettleLoop(ctx context.Context) {
	defer close(c.Settled)
	if c.StopSettleTicker != nil {
		defer c.StopSettleTicker()
	}
	for {
		select {
		case <-c.Settle:
			return
		case <-ctx.Done():
			return
		case <-c.SettleTicks:
			c.settleTick(ctx)
		}
	}
}

func (c *ConversationService) settleTick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, conversationSettleInterval)
	defer cancel()
	organization, err := c.LifecycleOrganization()
	if err != nil {
		c.Logger.Warn("conversation.settle_sweep_failed", "error", err)
		return
	}
	now := c.Server.config().Now()
	if err := c.WakePendingPG(ctx, organization); err != nil {
		c.Logger.Warn("conversation.pending_wake_failed", "error", err)
	}
	settled, err := c.SettleIdleConversationsPG(ctx, organization, now)
	if err != nil {
		c.Logger.Warn("conversation.settle_sweep_failed", "error", err)
		return
	}
	if settled > 0 {
		c.Logger.Info("conversation.settle_sweep", "settled", settled)
	}
	if _, err := c.CompactEventsPG(ctx, organization, now); err != nil {
		c.Logger.Warn("conversation.compaction_failed", "error", err)
	}
	if _, err := c.SweepAttachments(ctx); err != nil {
		c.Logger.Warn("conversation.attachment_sweep_failed", "error", err)
	}
}

// wakePending resumes work that was accepted before a restart: unlinked
// conversations with saved user messages are handed to the coordinator and
// linked conversations with queued controls to the control router. Nothing
// is replayed; the components re-read durable state.
func (c *ConversationService) WakePending(ctx context.Context) error {
	organization, err := c.LifecycleOrganization()
	if err != nil {
		return err
	}
	return c.WakePendingPG(ctx, organization)
}

func (c *ConversationService) Stop() {
	c.once.Do(func() {
		close(c.Settle)
		if c.SettleCancel != nil {
			c.SettleCancel()
		}
		<-c.Settled
	})
	c.Coordinator.Stop()
	c.Controls.Stop()
	c.Broker.CloseAll()
}

// Error codes specific to conversations, in addition to the shared native
// vocabulary (not_found, invalid_request, idempotency_conflict).
func conversationForbidden(message string) error {
	return &hubapi.Error{Code: "forbidden", Message: message, Status: http.StatusForbidden, Public: true}
}

func ConversationStale(message string) error {
	return &hubapi.Error{Code: "stale_execution", Message: message, Status: http.StatusConflict, Public: true}
}

// conversationStaleOwner reports an owner-generation mismatch with both
// attempts in details so the client can tell a lost race from a stale tab
// without a second request. Either identifier may be null.
func ConversationStaleOwner(message, expected, current string) error {
	return &hubapi.Error{
		Code: "stale_execution", Message: message, Status: http.StatusConflict, Public: true,
		Details: map[string]any{"expected_attempt_id": ConversationOptional(expected), "current_attempt_id": ConversationOptional(current)},
	}
}

// conversationRequireOwner checks the expected owner generation of a control
// and reports a mismatch as stale_execution carrying both attempts.
func conversationRequireOwner(owner conversation.Owner, expected conversation.Expected) error {
	if err := owner.Matches(expected); err != nil {
		return ConversationStaleOwner(err.Error(), expected.AttemptID, owner.AttemptID)
	}
	return nil
}

// staleExecutionAttempts reports whether err is a stale_execution rejection
// and returns the expected and current attempt identifiers it carries. A
// rejection raised without a generation comparison carries neither.
func StaleExecutionAttempts(err error) (expected, current string, ok bool) {
	var native *hubapi.Error
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

func ConversationStaleExecutionError(path, conversationID string, err error) error {
	expected, current, ok := StaleExecutionAttempts(err)
	if !ok {
		return err
	}
	var failure *hubapi.Error
	if !errors.As(err, &failure) {
		return err
	}
	return failure.WithLogAttributes("component", "conversation", "conversation_id", conversationID,
		"expected_attempt_id", expected, "current_attempt_id", current, "path", path)
}

func ConversationAlreadyLinked(existing string) error {
	err := &hubapi.Error{Code: "conversation_already_linked", Message: "The conversation or issue is already linked", Status: http.StatusConflict, Public: true}
	if existing != "" {
		err.Details = map[string]any{"existing_conversation_id": existing}
	}
	return err
}

func conversationQuestionAnswered() error {
	return &hubapi.Error{Code: "question_already_answered", Message: "The question has already been answered", Status: http.StatusConflict, Public: true}
}

func ConversationUnsupported(message string) error {
	return &hubapi.Error{Code: "unsupported_control", Message: message, Status: http.StatusUnprocessableEntity, Public: true}
}

func conversationShareRequired() error {
	return &hubapi.Error{Code: "share_history_required", Message: "Linking shares the conversation history; confirm with share_history", Status: http.StatusUnprocessableEntity, Public: true}
}

func conversationProjectFixed() error {
	return &hubapi.Error{Code: "conversation_project_fixed", Message: "Conversations never move between projects", Status: http.StatusUnprocessableEntity, Public: true}
}

func ConversationQueueFull() error {
	return &hubapi.Error{Code: "queue_full", Message: "The control queue is full; retry later", Status: http.StatusServiceUnavailable}
}

// translateConversationError maps store errors onto API errors.
func TranslateConversationError(err error) error {
	var linked *ErrConversationLinked
	if errors.As(err, &linked) {
		return ConversationAlreadyLinked(linked.ExistingConversationID)
	}
	var revision *ErrConversationRevision
	if errors.As(err, &revision) {
		return &hubapi.Error{Code: "revision_conflict", Message: "The conversation changed; reload and retry", Status: http.StatusConflict, Public: true}
	}
	if errors.Is(err, conversation.ErrStaleExecution) {
		return ConversationStale(err.Error())
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, corestore.ErrNotFound) {
		return hubapi.NotFound()
	}
	return err
}

// authorizeRead applies the audience rules from the decisions document:
// the record must belong to the request scope, private conversations are
// visible only to their owner, and every denial is an opaque not_found.
func (c *ConversationService) AuthorizeRead(ctx context.Context, query Queryer, scope hubauth.Input, record ConversationRecord) error {
	if record.OrganizationID != scope.Organization || record.ProjectID != scope.Project {
		return hubapi.NotFound()
	}
	if record.ReadAudience != "" {
		audience, err := conversationReadAudience(ctx, query, scope.Organization, scope.Credential)
		if err != nil || audience != record.ReadAudience {
			return hubapi.NotFound()
		}
	}
	if record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.Credential.ID {
		return hubapi.NotFound()
	}
	return nil
}

// actorFor describes the request principal for message attribution.
func ConversationActorFor(scope hubauth.Input) conversation.Actor {
	if scope.Credential.Runner.RunnerID != "" || scope.Credential.Scope == hubauth.Worker {
		return conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: scope.Credential.ID}
	}
	return conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: scope.Credential.ID}
}

// Public resource projections (the conversation contract).

type conversationOwnerResource struct {
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
}

type ConversationExecutionResource struct {
	Status conversation.ExecutionStatus `json:"status"`
	// The owner identifiers are null rather than empty when no attempt owns
	// the conversation; the client tells "absent" from "known" by null.
	AttemptID *string `json:"attempt_id"`
	RunID     *string `json:"run_id"`
	RunnerID  *string `json:"runner_id"`
	ThreadID  *string `json:"thread_id"`
	TurnID    *string `json:"turn_id"`
	// Resume reports what the bound runner continued from: its own provider
	// thread, a transcript, or nothing.
	Resume       string                    `json:"resume"`
	Capabilities conversation.Capabilities `json:"capabilities"`
	Error        *string                   `json:"error"`
	UpdatedAt    time.Time                 `json:"updated_at"`
}

// conversationWorkItemResource is the linked issue as the client renders it:
// enough to show and open the issue card without a second request.
type ConversationWorkItemResource struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Lane       string `json:"lane"`
	// RunnerBound reports that an attempt currently owns the issue, which
	// is what the client shows instead of "waiting for runner".
	RunnerBound bool `json:"runner_bound"`
}

type ConversationResource struct {
	ID             string                  `json:"id"`
	WebURL         string                  `json:"web_url,omitempty"`
	OrganizationID tracker.OrganizationID  `json:"organization_id"`
	ProjectID      tracker.ProjectID       `json:"project_id"`
	Title          string                  `json:"title"`
	Visibility     conversation.Visibility `json:"visibility"`
	Origin         string                  `json:"origin"`
	Status         conversation.Status     `json:"status"`
	Archived       bool                    `json:"archived"`
	// Preferences are the conversation's turn preferences; each field is
	// "auto" or an explicit value.
	Preferences       conversation.Preferences      `json:"preferences"`
	SubjectWorkItemID *string                       `json:"subject_work_item_id,omitempty"`
	WorkItemID        *string                       `json:"work_item_id"`
	LinkedAt          *time.Time                    `json:"linked_at"`
	WorkItem          *ConversationWorkItemResource `json:"work_item"`
	Owner             conversationOwnerResource     `json:"owner"`
	Execution         ConversationExecutionResource `json:"execution"`
	Revision          int64                         `json:"revision"`
	EventSeq          int64                         `json:"event_seq"`
	CreatedAt         time.Time                     `json:"created_at"`
	UpdatedAt         time.Time                     `json:"updated_at"`
	LastMessageAt     *time.Time                    `json:"last_message_at"`
	// MessageCount is the whole history, so the share confirmation can say
	// how much becomes readable.
	MessageCount int64 `json:"message_count"`
}

type ConversationMessageResource struct {
	revision       int64
	contentID      string
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
	// against the project.
	References []ConversationReferenceResource `json:"references"`
	// Attachments are the uploads the message carries, each with the URL
	// that streams it.
	Attachments []conversation.Attachment `json:"attachments"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
}

// conversationReferenceResource is one resolved reference as the client
// renders it: a link with a label.
type ConversationReferenceResource struct {
	Kind  conversation.ReferenceKind `json:"kind"`
	ID    string                     `json:"id"`
	Label string                     `json:"label"`
	URL   string                     `json:"url"`
}

func projectReferences(references []ConversationMessageReference) []ConversationReferenceResource {
	projected := make([]ConversationReferenceResource, 0, len(references))
	for _, reference := range references {
		projected = append(projected, ConversationReferenceResource{
			Kind: reference.Kind, ID: reference.ID, Label: reference.Label,
			URL: reference.URL,
		})
	}
	return projected
}

func ConversationOptional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func ConversationBetweenTurns(execution conversation.Execution) bool {
	return execution.Status == conversation.ExecutionRunning && execution.Owner.AttemptID != "" && execution.Owner.TurnID == ""
}

func ProjectExecution(execution conversation.Execution) ConversationExecutionResource {
	status := execution.Status
	if status == "" {
		status = conversation.ExecutionIdle
	}
	capabilities := execution.Capabilities
	if ConversationBetweenTurns(execution) {
		capabilities.Steer = false
	}
	return ConversationExecutionResource{
		Status:       status,
		AttemptID:    ConversationOptional(execution.Owner.AttemptID),
		RunID:        ConversationOptional(execution.Owner.RunID),
		RunnerID:     ConversationOptional(execution.Owner.RunnerID),
		ThreadID:     ConversationOptional(execution.Owner.ThreadID),
		TurnID:       ConversationOptional(execution.Owner.TurnID),
		Resume:       execution.Resume,
		Capabilities: capabilities,
		Error:        ConversationOptional(execution.Error),
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
func projectWorkItem(record ConversationRecord) *ConversationWorkItemResource {
	if record.WorkItemID == "" || record.WorkItem == nil {
		return nil
	}
	return &ConversationWorkItemResource{
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
func ConversationIssueResult(record ConversationRecord) map[string]any {
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
type ConversationWorkItem struct {
	ID         string
	Identifier string
	Title      string
	Lane       string
}

func ProjectConversation(record ConversationRecord) ConversationResource {
	return ConversationResource{
		ID:                record.ID,
		OrganizationID:    record.OrganizationID,
		ProjectID:         record.ProjectID,
		Title:             record.Title,
		Visibility:        record.Visibility,
		Origin:            record.Origin,
		Status:            record.Status,
		Archived:          record.Archived,
		Preferences:       record.Preferences.Normalized(),
		SubjectWorkItemID: ConversationOptional(record.SubjectWorkItemID),
		WorkItemID:        ConversationOptional(record.WorkItemID),
		LinkedAt:          record.LinkedAt,
		WorkItem:          projectWorkItem(record),
		Owner:             conversationOwnerResource{PrincipalID: record.OwnerPrincipalID, Subject: record.OwnerSubject},
		Execution:         ProjectExecution(record.Execution),
		Revision:          record.Revision,
		EventSeq:          record.EventSeq,
		CreatedAt:         record.CreatedAt,
		UpdatedAt:         record.UpdatedAt,
		LastMessageAt:     record.LastMessageAt,
		MessageCount:      record.MessageCount,
	}
}

func ProjectMessage(record ConversationMessageRecord) ConversationMessageResource {
	var data any = map[string]any{}
	if len(record.Data) > 0 && string(record.Data) != "null" {
		data = record.Data
	}
	return ConversationMessageResource{
		contentID:      record.ContentID,
		revision:       record.Revision,
		ID:             record.ID,
		ConversationID: record.ConversationID,
		Seq:            record.Seq,
		Role:           record.Role,
		Kind:           record.Kind,
		Text:           record.Text,
		Data:           data,
		Delivery:       record.Delivery,
		AttemptID:      ConversationOptional(record.AttemptID),
		TurnID:         ConversationOptional(record.TurnID),
		ProviderItemID: ConversationOptional(record.ProviderItemID),
		Actor:          record.Actor,
		CommandKey:     ConversationOptional(record.CommandKey),
		References:     projectReferences(record.References),
		Attachments:    ProjectAttachments(record.Attachments),
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
	}
}

// loadConversation reads a conversation for the request scope and applies
// the read rule. Any failure is an API error.
func (c *ConversationService) LoadConversation(ctx context.Context, query Queryer, scope hubauth.Input, id string) (ConversationRecord, error) {
	record, err := c.loadConversationRow(ctx, query, scope, id)
	if err != nil {
		return ConversationRecord{}, err
	}
	if c.Server.conversationRecordsSelected() {
		return record, nil
	}
	if err := c.Store.resolveConversationRecord(ctx, query, &record); err != nil {
		return ConversationRecord{}, TranslateConversationError(err)
	}
	return record, nil
}

func (c *ConversationService) loadConversationRow(ctx context.Context, query Queryer, scope hubauth.Input, id string) (ConversationRecord, error) {
	if err := conversation.ValidateConversationID(id); err != nil {
		return ConversationRecord{}, hubapi.NotFound()
	}
	if c.Server.conversationRecordsSelected() {
		return c.Server.ReadPGConversation(ctx, scope, id)
	}
	record, err := c.Store.readConversationRow(ctx, query, scope.Organization, scope.Project, id)
	if err != nil {
		return ConversationRecord{}, TranslateConversationError(err)
	}
	if err := c.AuthorizeRead(ctx, query, scope, record); err != nil {
		return ConversationRecord{}, err
	}
	return record, nil
}
