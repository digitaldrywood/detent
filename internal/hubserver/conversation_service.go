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
	"github.com/digitaldrywood/detent/internal/hubconversation"
)

type ConversationConfig = hubconversation.Config

type ConversationUsage = hubconversation.Usage

type ConversationUsageSink = hubconversation.UsageSink

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
	conversationHeartbeatInterval = 15 * time.Second
	conversationAuthorizeInterval = 30 * time.Second
	conversationEventPage         = 200
	conversationMessagePage       = 50
	// conversationSnapshotQuestions bounds the questions a snapshot carries.
	conversationSnapshotQuestions = 20
)

func normalizeConversationConfig(c ConversationConfig) ConversationConfig {
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
type conversationService struct {
	server *Service
	store  *conversationStore
	broker *notificationBroker
	config ConversationConfig
	logger *slog.Logger
	// coordinator answers messages in conversations without a linked
	// issue. It is woken after a message command committed.
	coordinator conversationCoordinator
	// controls delivers committed controls to the bound worker of a linked
	// conversation. It is woken after a control committed.
	controls conversationControlRouter
	// settle stops the settle sweep; settled waits for it to exit.
	settle              chan struct{}
	settled             chan struct{}
	monthlyBudgetPeriod string
	once                sync.Once
}

// conversationCoordinator runs model turns for unlinked conversations. The
// durable store is the queue: Wake only signals that saved user messages
// may exist for the conversation.
type conversationCoordinator interface {
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
type conversationControlRouter interface {
	Wake(conversationID string)
	Stop()
}

func newConversationService(server *Service, cfg ConversationConfig) *conversationService {
	if cfg.UsageSink == nil {
		cfg.UsageSink = server.database
	}
	service := &conversationService{
		server: server,
		store:  newConversationStore(server.database.db),
		broker: server.notifications,
		config: cfg,
		logger: server.config.Logger.With("component", "conversation"),
	}
	service.store.reader = server.database.reader
	service.store.database = server.database
	service.store.contents = cfg.ArchiveContents
	if server.config.Hosted != nil && server.config.Hosted.SharedEntry != nil {
		service.store.contents = server.config.Hosted.SharedEntry.ArchiveContents
	}
	if service.store.contents == nil {
		call := server.config.Content
		if call == nil && server.config.Hosted != nil && server.config.Hosted.SharedEntry != nil {
			call = server.config.Hosted.SharedEntry.Content
		}
		if call != nil {
			service.store.contents = conversationArchiveStore{call: call}
		}
	}
	service.coordinator = newConversationCoordinator(service)
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
	if record.WorkItemID == "" {
		c.coordinator.Wake(record.ID)
		return
	}
	c.controls.Wake(record.ID)
}

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
	if err := c.reconcileExecutions(ctx); err != nil {
		return err
	}
	return c.wakePending(ctx)
}

// settleLoop settles conversations that have been finished and idle for the
// project's settle window. The sweep is periodic rather than scheduled: a
// conversation that wakes in the meantime is simply not a candidate when the
// sweep next runs.
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
			period := now.UTC().Format("2006-01")
			if c.monthlyBudgetPeriod != period {
				c.monthlyBudgetPeriod = period
				if err := c.wakePending(context.Background()); err != nil {
					c.logger.Warn("conversation.budget_rollover", "error", err)
				}
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
			// their bytes are the expensive part.
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

// wakePending resumes work that was accepted before a restart: unlinked
// conversations with saved user messages are handed to the coordinator and
// linked conversations with queued controls to the control router. Nothing
// is replayed; the components re-read durable state.
func (c *conversationService) wakePending(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	rows, err := c.store.reader.QueryContext(ctx, `SELECT DISTINCT m.conversation_id, c.work_item_id IS NOT NULL FROM conversation_messages m
JOIN conversations c ON c.id = m.conversation_id
WHERE m.role = 'user' AND m.delivery IN ('saved', 'queued')`)
	if err != nil {
		return fmt.Errorf("list pending conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var linkedIDs, unlinkedIDs []string
	for rows.Next() {
		var id string
		var linked bool
		if err := rows.Scan(&id, &linked); err != nil {
			return fmt.Errorf("scan pending conversation: %w", err)
		}
		if linked {
			linkedIDs = append(linkedIDs, id)
			continue
		}
		unlinkedIDs = append(unlinkedIDs, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	coordinator := 0
	if c.coordinator.Available() {
		coordinator = len(unlinkedIDs)
		for _, id := range unlinkedIDs {
			c.coordinator.Wake(id)
		}
	}
	for _, id := range linkedIDs {
		c.controls.Wake(id)
	}
	c.logger.Info("conversation.wake_pending", "coordinator", coordinator, "controls", len(linkedIDs))
	return nil
}

func (c *conversationService) stop() {
	c.once.Do(func() {
		close(c.settle)
		<-c.settled
	})
	c.coordinator.Stop()
	c.controls.Stop()
	c.broker.closeAll()
}

// notificationBroker wakes subscribers when a conversation gained events.
// It carries no payload: subscribers read the durable event log from their
// cursor, so a slow subscriber can never lose or reorder events.
type notificationBroker struct {
	mu          sync.Mutex
	closed      bool
	epoch       string
	revisions   map[string]uint64
	subscribers map[string]map[*notificationSubscription]struct{}
}

type notificationSubscription struct {
	conversationID string
	wake           chan struct{}
	closed         chan struct{}
	once           sync.Once
}

func newNotificationBroker() *notificationBroker {
	return &notificationBroker{epoch: newNativeID("wait"), revisions: make(map[string]uint64), subscribers: map[string]map[*notificationSubscription]struct{}{}}
}

// subscribe registers interest in a conversation. The returned wake channel
// receives at most one pending signal; closed is closed when the broker
// shuts down. Call cancel when done.
func (b *notificationBroker) subscribe(conversationID string) (subscription *notificationSubscription, cancel func()) {
	subscription = &notificationSubscription{conversationID: conversationID, wake: make(chan struct{}, 1), closed: make(chan struct{})}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		subscription.close()
		return subscription, func() {}
	}
	set, ok := b.subscribers[conversationID]
	if !ok {
		set = map[*notificationSubscription]struct{}{}
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

func (s *notificationSubscription) close() {
	s.once.Do(func() { close(s.closed) })
}

// notify wakes every subscriber of the conversation. Call it after the
// transaction that appended events has committed.
func (b *notificationBroker) notify(conversationID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, tracked := b.revisions[conversationID]; tracked {
		b.revisions[conversationID]++
	}
	for subscription := range b.subscribers[conversationID] {
		select {
		case subscription.wake <- struct{}{}:
		default:
		}
	}
}

func (b *notificationBroker) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, set := range b.subscribers {
		for subscription := range set {
			subscription.close()
		}
	}
	b.subscribers = map[string]map[*notificationSubscription]struct{}{}
}

// Error codes specific to conversations, in addition to the shared native
// vocabulary (not_found, invalid_request, idempotency_conflict).
func conversationForbidden(message string) error {
	return &nativeError{Code: "forbidden", Message: message, status: http.StatusForbidden, publicMessage: true}
}

func conversationStale(message string) error {
	return &nativeError{Code: "stale_execution", Message: message, status: http.StatusConflict, publicMessage: true}
}

// conversationStaleOwner reports an owner-generation mismatch with both
// attempts in details so the client can tell a lost race from a stale tab
// without a second request. Either identifier may be null.
func conversationStaleOwner(message, expected, current string) error {
	return &nativeError{
		Code: "stale_execution", Message: message, status: http.StatusConflict, publicMessage: true,
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

func conversationStaleExecutionError(path, conversationID string, err error) error {
	expected, current, ok := staleExecutionAttempts(err)
	if !ok {
		return err
	}
	var failure *nativeError
	if !errors.As(err, &failure) {
		return err
	}
	refusal := *failure
	refusal.logAttributes = []any{"component", "conversation", "conversation_id", conversationID,
		"expected_attempt_id", expected, "current_attempt_id", current, "path", path}
	return &refusal
}

func conversationAlreadyLinked(existing string) error {
	err := &nativeError{Code: "conversation_already_linked", Message: "The conversation or issue is already linked", status: http.StatusConflict, publicMessage: true}
	if existing != "" {
		err.Details = map[string]any{"existing_conversation_id": existing}
	}
	return err
}

func conversationQuestionAnswered() error {
	return &nativeError{Code: "question_already_answered", Message: "The question has already been answered", status: http.StatusConflict, publicMessage: true}
}

func conversationUnsupported(message string) error {
	return &nativeError{Code: "unsupported_control", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}

func conversationShareRequired() error {
	return &nativeError{Code: "share_history_required", Message: "Linking shares the conversation history; confirm with share_history", status: http.StatusUnprocessableEntity, publicMessage: true}
}

func conversationProjectFixed() error {
	return &nativeError{Code: "conversation_project_fixed", Message: "Conversations never move between projects", status: http.StatusUnprocessableEntity, publicMessage: true}
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
		return &nativeError{Code: "revision_conflict", Message: "The conversation changed; reload and retry", status: http.StatusConflict, publicMessage: true}
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
func (c *conversationService) authorizeRead(ctx context.Context, query nativeQueryer, scope nativeScope, record conversationRecord) error {
	if record.OrganizationID != scope.organization || record.ProjectID != scope.project {
		return nativeNotFound()
	}
	if record.ReadAudience != "" {
		audience, err := conversationReadAudience(ctx, query, scope.organization, scope.credential)
		if err != nil || audience != record.ReadAudience {
			return nativeNotFound()
		}
	}
	if record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.credential.ID {
		return nativeNotFound()
	}
	return nil
}

// authorizeWrite requires read access, an active conversation and project
// write capability. Viewers and read-only grants cannot send commands.
func (c *conversationService) authorizeWrite(ctx context.Context, query nativeQueryer, scope nativeScope, record conversationRecord) error {
	if err := c.authorizeRead(ctx, query, scope, record); err != nil {
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

// Public resource projections (the conversation contract).

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
	ids, err := sqlList(linked)
	if err != nil {
		return fmt.Errorf("encode linked work items: %w", err)
	}
	rows, err := querySQL(ctx, query, `SELECT i.organization_id, i.project_id, i.native_id, p.name, COALESCE(i.number, 0), i.title, COALESCE(ws.detent_state, '')
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.native_id IN (SELECT value FROM sql_rows(?))`, ids)
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

// loadConversation reads a conversation for the request scope and applies
// the read rule. Any failure is an API error.
func (c *conversationService) loadConversation(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (conversationRecord, error) {
	record, err := c.loadConversationRow(ctx, query, scope, id)
	if err != nil {
		return conversationRecord{}, err
	}
	if err := c.store.resolveConversationRecord(ctx, query, &record); err != nil {
		return conversationRecord{}, translateConversationError(err)
	}
	return record, nil
}

func (c *conversationService) loadConversationRow(ctx context.Context, query nativeQueryer, scope nativeScope, id string) (conversationRecord, error) {
	if err := conversation.ValidateConversationID(id); err != nil {
		return conversationRecord{}, nativeNotFound()
	}
	record, err := c.store.readConversationRow(ctx, query, scope.organization, scope.project, id)
	if err != nil {
		return conversationRecord{}, translateConversationError(err)
	}
	if err := c.authorizeRead(ctx, query, scope, record); err != nil {
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
		if plan, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok && plan.messageCursor < len(plan.messageIDs) {
			message.ID = plan.messageIDs[plan.messageCursor]
			plan.messageCursor++
		} else {
			message.ID = conversation.NewMessageID()
		}
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
	// message is announced.
	if err := c.unsettle(ctx, tx, record, now); err != nil {
		return err
	}
	if _, err := c.store.appendEvent(ctx, tx, record.ID, conversation.EventMessageAccepted, projectMessage(*message), now); err != nil {
		return err
	}
	return nil
}

// unsettle returns a settled conversation to active. Any accepted command or
// new message wakes it; there is no unarchive action.
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
	return conversationMessageReference{Kind: conversation.ReferenceConversation, ID: reference.ConversationID, Label: reference.Text, URL: "/chat/c/" + reference.ConversationID}, true, nil
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
	path, err := readIssueBrowserPath(ctx, tx, workItemID)
	if err != nil {
		return found, false, err
	}
	return conversationMessageReference{Kind: conversation.ReferenceIssue, ID: workItemID, Label: reference.Text, URL: path}, true, nil
}

// updateMessage persists a changed message and its message.updated event.
// A message that reached a terminal delivery has its final text, so its
// references are extracted then rather than on every streamed delta.
// For any other delivery the caller's record must
// already carry its stored references, which every read path fills, because
// the event it emits replaces the client's copy of the message.
func (c *conversationService) updateMessage(ctx context.Context, tx *sql.Tx, message conversationMessageRecord, now time.Time) error {
	message.UpdatedAt = now
	if err := c.store.updateMessage(ctx, tx, message); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT content_attachment_id,revision FROM conversation_messages WHERE id=? AND conversation_id=?", message.ID, message.ConversationID).Scan(&message.ContentID, &message.Revision); err != nil {
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

func (c *conversationService) appendDelta(ctx context.Context, tx *sql.Tx, message *conversationMessageRecord, text string, now time.Time) error {
	if err := c.appendDeltaAt(ctx, tx, message.ConversationID, message.ID, int64(len(message.Text)), text, now); err != nil {
		return err
	}
	// The caller's record is only advanced once the write succeeded: a
	// failed write must not leave the accumulated text double-counted on
	// the next attempt.
	message.Text += text
	message.UpdatedAt = now
	return nil
}

func (c *conversationService) appendDeltaAt(ctx context.Context, tx *sql.Tx, conversationID, messageID string, offset int64, text string, now time.Time) error {
	if c.store.contents == nil {
		return errors.New("conversation content storage is unavailable")
	}
	statement := "UPDATE conversation_messages SET text_bytes=COALESCE(text_bytes,length(CAST(text AS BLOB)))+?,updated_at=?,revision=revision+1 WHERE id=? AND conversation_id=? AND COALESCE(text_bytes,length(CAST(text AS BLOB)))=? RETURNING revision"
	args := []any{len(text), conversationTime(now), messageID, conversationID, offset}
	fact := conversationDeltaFact{Text: text, MessageID: messageID, Offset: offset, Bytes: len(text)}
	err := tx.QueryRowContext(ctx, statement, args...).Scan(&fact.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return conversationStale("Message text changed")
	}
	if err != nil {
		return fmt.Errorf("append message delta: %w", err)
	}
	if _, err := c.store.appendEvent(ctx, tx, conversationID, conversation.EventMessageDelta, fact, now); err != nil {
		return err
	}
	return nil
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
	if plan, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok && plan.receiptLogs != nil {
		plan.receiptLogs = append(plan.receiptLogs, attributes)
	} else {
		c.logger.Info("conversation.receipt", attributes...)
	}
	return nil
}
