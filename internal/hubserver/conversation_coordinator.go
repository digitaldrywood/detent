package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/coordinator"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Coordinator limits. The durable store is the queue, so every limit here
// bounds a single process rather than the amount of stored work.
const (
	coordinatorConcurrency = 4
	coordinatorTurnTimeout = coordinator.TurnTimeout

	// coordinatorPendingMessages and coordinatorPendingRunes bound the
	// prompt a single turn carries: the durable store may hold far more
	// queued text than one provider request should ever receive.
	coordinatorPendingMessages = coordinator.PendingMessages
	coordinatorPendingRunes    = coordinator.PendingRunes
	coordinatorStopTimeout     = 5 * time.Second
	coordinatorWriteTimeout    = 10 * time.Second
	// coordinatorAttachmentBlockBytes bounds the text one turn's attachment
	// data block adds to the prompt.
	coordinatorAttachmentBlockBytes = coordinator.AttachmentBlockBytes
)

type conversationTurnCoordinator struct {
	server  *Service
	service *conversationService
	logger  *slog.Logger
	stop    chan struct{}
	mu      sync.Mutex
	stopped bool
	// passes holds every conversation that is running or queued.
	passes map[string]*coordinatorPass
	// queue lists the queued conversations in wake order.
	queue []string
	// workers counts the running worker goroutines.
	workers int
	// held counts link hand-offs that keep a conversation from starting a
	// turn.
	held  map[string]int
	turns map[string]*coordinatorTurn
	wg    sync.WaitGroup
}

// coordinatorPass is the per-conversation scheduling state: pending records
// a wake that arrived while a pass was running.
type coordinatorPass struct {
	pending bool
}

// coordinatorTurn is a running turn. cancelled distinguishes an operator
// cancel (interrupted) from a process stop (failed); done closes once the
// turn's final writes are over.
type coordinatorTurn struct {
	id            string
	cancel        context.CancelFunc
	cancelled     bool
	budgetStopped bool
	done          chan struct{}
}

func newConversationCoordinator(server *Service, service *conversationService) conversationCoordinator {
	logger := slog.Default()
	if service != nil && service.Logger != nil {
		logger = service.Logger.With("component", "conversation.coordinator")
	}
	return &conversationTurnCoordinator{
		server:  server,
		service: service,
		logger:  logger,
		stop:    make(chan struct{}),
		passes:  map[string]*coordinatorPass{},
		held:    map[string]int{},
		turns:   map[string]*coordinatorTurn{},
	}
}

func (c *conversationTurnCoordinator) Available() bool {
	return c.service != nil && c.service.Config.Backend != nil
}

// Wake schedules a pass for the conversation. A conversation already
// running or queued absorbs the wake; otherwise it starts on a free worker
// or joins the queue.
func (c *conversationTurnCoordinator) Wake(conversationID string) {
	if !c.Available() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if pass, ok := c.passes[conversationID]; ok {
		pass.pending = true
		return
	}
	c.passes[conversationID] = &coordinatorPass{}
	if c.workers >= coordinatorConcurrency {
		c.queue = append(c.queue, conversationID)
		return
	}
	c.workers++
	c.wg.Add(1)
	go c.work(conversationID)
}

// Cancel stops the running turn of the conversation and reports whether one
// was running.
func (c *conversationTurnCoordinator) Cancel(conversationID string) bool {
	return c.cancelTurn(conversationID, false)
}

func (c *conversationTurnCoordinator) cancelRecordedTurn(conversationID, targetTurnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	turn, ok := c.turns[conversationID]
	if !ok || targetTurnID == "" || turn.id != targetTurnID {
		return false
	}
	turn.cancelled = true
	turn.cancel()
	return true
}

func (c *conversationTurnCoordinator) cancelTurn(conversationID string, budgetStop bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	turn, ok := c.turns[conversationID]
	if !ok {
		return false
	}
	turn.cancelled = true
	turn.budgetStopped = budgetStop
	turn.cancel()
	return true
}

// Hold stops the conversation's running turn, waits for its final writes
// and keeps new turns from starting until release is called, so a link
// never races a coordinator turn. release wakes the conversation again in
// case the link did not happen.
func (c *conversationTurnCoordinator) Hold(conversationID string) (func(), error) {
	c.mu.Lock()
	c.held[conversationID]++
	turn := c.turns[conversationID]
	if turn != nil {
		turn.cancelled = true
		turn.cancel()
	}
	c.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.mu.Lock()
			c.held[conversationID]--
			if c.held[conversationID] <= 0 {
				delete(c.held, conversationID)
			}
			c.mu.Unlock()
			c.Wake(conversationID)
		})
	}
	if turn == nil {
		return release, nil
	}
	select {
	case <-turn.done:
		return release, nil
	case <-time.After(coordinatorStopTimeout):
		release()
		return nil, conversationStale("The coordinator is still finishing its answer; try again")
	}
}

func (c *conversationTurnCoordinator) Stop() {
	c.mu.Lock()
	c.stopped = true
	for _, id := range c.queue {
		delete(c.passes, id)
	}
	c.queue = nil
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(coordinatorStopTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}

	c.mu.Lock()
	for _, turn := range c.turns {
		turn.cancel()
	}
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(coordinatorStopTimeout):
		c.logger.Warn("coordinator stop timed out waiting for turns")
	}
}

// work is one worker goroutine: it serves the conversation it was started
// for, then takes queued conversations until the queue is empty.
func (c *conversationTurnCoordinator) work(conversationID string) {
	defer c.wg.Done()
	for {
		c.loop(conversationID)
		c.mu.Lock()
		if c.stopped || len(c.queue) == 0 {
			c.workers--
			c.mu.Unlock()
			return
		}
		conversationID = c.queue[0]
		c.queue = c.queue[1:]
		c.mu.Unlock()
	}
}

// loop runs passes for one conversation until neither a wake nor a finished
// turn asks for another look.
func (c *conversationTurnCoordinator) loop(conversationID string) {
	for {
		ran, err := c.pass(conversationID)
		if err != nil {
			c.logger.Error("coordinator pass failed", "conversation_id", conversationID, "error", err)
		}
		c.mu.Lock()
		pass := c.passes[conversationID]
		again := pass != nil && (pass.pending || (ran && err == nil)) && !c.stopped && c.held[conversationID] == 0
		if again {
			pass.pending = false
			c.mu.Unlock()
			continue
		}
		delete(c.passes, conversationID)
		c.mu.Unlock()
		return
	}
}

// pass runs one turn when the conversation is unlinked, active and has
// pending user messages. It reports whether a turn ran.
func (c *conversationTurnCoordinator) pass(conversationID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), coordinatorWriteTimeout)
	defer cancel()
	record, err := c.readConversation(ctx, nil, conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !coordinatorHandles(record) {
		return false, nil
	}
	pending, err := c.recordDeps().PendingMessages(ctx, conversationID)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		return false, nil
	}
	return c.runTurn(conversationID)
}

func (c *conversationTurnCoordinator) readConversation(ctx context.Context, query hubapi.NativeQueryer, id string) (conversationRecord, error) {
	return c.service.ReadPublicationConversation(ctx, id)
}

func (c *conversationTurnCoordinator) appendPreparedMessage(ctx context.Context, conversationID string, message conversationMessageRecord) error {
	return c.write(ctx, conversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error {
		return c.appendMessage(ctx, tx, record, &message, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		return p.AppendMessage(ctx, &message)
	})
}

type coordinatorTurnState struct {
	output         coordinator.TurnOutput
	coordinator    *conversationTurnCoordinator
	conversationID string
	organizationID tracker.OrganizationID
	projectID      tracker.ProjectID
	users          []conversationMessageRecord
	// preferences are the conversation's turn preferences as they stood when
	// the turn started.
	preferences   conversation.Preferences
	modelChoice   bool
	budgetStopped bool
}

// runTurn marks pending messages as sending, streams one backend turn into
// the store and persists the outcome. It reports whether a turn started.
func (c *conversationTurnCoordinator) runTurn(conversationID string) (bool, error) {
	c.mu.Lock()
	if c.stopped || c.held[conversationID] > 0 {
		c.mu.Unlock()
		return false, nil
	}
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	turn := &coordinatorTurn{cancel: cancelTurn, done: make(chan struct{})}
	c.turns[conversationID] = turn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.turns, conversationID)
		c.mu.Unlock()
		cancelTurn()
		close(turn.done)
	}()

	turnCtx, err := c.service.PrepareConversationContents(turnCtx, conversationID, conversationMessageRecord{})
	if err != nil {
		return false, fmt.Errorf("prepare coordinator assistant: %w", err)
	}
	start := coordinator.TurnStartDeps{Write: c.write, Records: c.recordDeps(), Admission: c.turnAdmissionDeps()}
	started, err := start.Start(turnCtx, conversationID, func(id string) {
		c.mu.Lock()
		turn.id = id
		c.mu.Unlock()
	})
	if errors.Is(err, errCoordinatorNothingPending) || errors.Is(err, errCoordinatorLinked) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("start coordinator turn: %w", err)
	}

	state := &coordinatorTurnState{
		coordinator:    c,
		conversationID: conversationID,
		organizationID: started.OrganizationID,
		projectID:      started.ProjectID,
		users:          started.Users,
		preferences:    started.Preferences,
		modelChoice:    started.ModelChoice,
	}
	refusal := started.Refusal
	if refusal != nil {
		started.Assistant.Text = refusal.Error()
	}
	state.output.Initialize(conversationID, started.ThreadID, started.Assistant, coordinator.TurnEventDeps{
		Write:           c.write,
		PrepareContents: c.service.PrepareConversationContents,
		Logger:          c.logger,
	})
	if refusal != nil {
		ctx, cancel := context.WithTimeout(context.Background(), coordinatorWriteTimeout)
		defer cancel()
		return true, state.finish(ctx, conversation.DeliveryFailed, refusal, nil)
	}

	tools := newCoordinatorToolset(c, state)
	promptDeps := coordinator.TurnPromptDeps{
		Hosted:            c.server.config.Hosted != nil,
		AttachmentContent: c.attachmentContent,
		ReadConversation:  c.service.ReadPublicationConversation,
		ActionContext:     tools.actionContext,
		Operator:          currentHubOperator,
		AuthorizeRecord:   c.authorizeRecord,
		RestrictAudience:  tools.restrictReadAudience,
		WorkflowEvidence:  tools.workflowEvidence,
		IssueContext:      tools.issueContext,
	}
	prompt, runErr := promptDeps.Assemble(turnCtx, conversationID, started.Transcript, state.users)
	request, backend, runErr := coordinator.TurnRequest(c.service.Config, c.server.hasLunaCoordinator(), state.modelChoice, state.preferences, started.ThreadID, prompt, runErr)
	_, genkit := backend.(*genkitbackend.Backend)
	state.output.ConfigureModel(request.Model, runner.NewTokenUsageNormalizer(request.Resume.ThreadID != "" && !genkit))
	var result runner.AgentTurnResult
	if runErr == nil {
		// A turn that cannot read its attachments ends as failed through
		// the same completion as a provider failure, so its messages and
		// execution never stay in flight.
		stopFlusher := startCoordinatorOutputFlusher(turnCtx, state.output.Flush)
		result, runErr = c.callBackend(turnCtx, backend, request, state)
		stopFlusher()
	}
	// The final writes use a fresh context: cancellation must persist its
	// outcome rather than lose it.
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), coordinatorWriteTimeout)
	defer cancelWrite()
	state.output.Flush(writeCtx)
	if result.ThreadID != "" {
		state.output.ObserveThread(writeCtx, result.ThreadID)
	}

	c.mu.Lock()
	cancelled := turn.cancelled
	state.budgetStopped = turn.budgetStopped
	c.mu.Unlock()
	if runErr == nil {
		runErr = turnCtx.Err()
	}
	outcome := coordinatorOutcome(runErr, cancelled)
	var usage *ConversationUsage
	assistant, model, tokens := state.output.Summary()
	if tokens.InputTokens > 0 || tokens.OutputTokens > 0 {
		provider := "codex"
		if genkit {
			provider = "openai"
		}
		usage = &ConversationUsage{OrganizationID: state.organizationID, ProjectID: state.projectID, ConversationID: conversationID, TurnID: assistant.ID, Provider: provider, Model: model, Tokens: tokens, Outcome: outcome, OccurredAt: assistant.CreatedAt}
	}
	var durableUsage *ConversationUsage
	if c.service.Config.UsageSink == c.server.database {
		durableUsage = usage
	}
	if err := state.finish(writeCtx, outcome, runErr, durableUsage); err != nil {
		return true, fmt.Errorf("finish coordinator turn: %w", err)
	}
	c.server.stopMonthlyBudgetTurns(writeCtx, state.organizationID)
	if sink := c.service.Config.UsageSink; sink != nil && sink != c.server.database && usage != nil {
		if err := sink.RecordConversationUsage(writeCtx, *usage); err != nil {
			c.logger.Warn("coordinator usage report failed", "conversation_id", conversationID, "error", err)
		}
	}

	if runErr != nil && outcome == conversation.DeliveryFailed {
		c.logger.Warn("coordinator turn failed", "conversation_id", conversationID, "error", runErr)
	}
	return true, nil
}

func startCoordinatorOutputFlusher(ctx context.Context, flush func(context.Context)) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(coordinator.DeltaFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				flush(ctx)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
