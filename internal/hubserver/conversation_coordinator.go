package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/coordinator"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/credits"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Coordinator limits. The durable store is the queue, so every limit here
// bounds a single process rather than the amount of stored work.
const (
	coordinatorConcurrency        = 4
	coordinatorTurnTimeout        = 10 * time.Minute
	coordinatorMaxDuration        = 15 * time.Minute
	coordinatorDeltaFlushInterval = 150 * time.Millisecond
	coordinatorDeltaFlushBytes    = 2048
	coordinatorTranscriptMessages = 20

	coordinatorErrorRunes = 2000
	// coordinatorPendingMessages and coordinatorPendingRunes bound the
	// prompt a single turn carries: the durable store may hold far more
	// queued text than one provider request should ever receive.
	coordinatorPendingMessages = 20
	coordinatorPendingRunes    = coordinator.PendingRunes
	coordinatorStopTimeout     = 5 * time.Second
	coordinatorWriteTimeout    = 10 * time.Second
	coordinatorDefaultEffort   = "low"
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
	pending, err := c.pendingMessages(ctx, nil, conversationID)
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

// pendingMessages returns the user text messages saved after the last
// assistant message, oldest first.
func (c *conversationTurnCoordinator) pendingMessages(ctx context.Context, query hubapi.NativeQueryer, conversationID string) ([]conversationMessageRecord, error) {
	return c.coordinatorMessages(ctx, conversationstore.MessageFilter{ConversationID: conversationID, Role: conversation.RoleUser, Kind: conversation.MessageText, Deliveries: []conversation.Delivery{conversation.DeliverySaved, conversation.DeliveryQueued}, Limit: coordinatorPendingMessages, Oldest: true})
}

func (c *conversationTurnCoordinator) appendPreparedMessage(ctx context.Context, conversationID string, message conversationMessageRecord) error {
	return c.write(ctx, conversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error {
		return c.appendMessage(ctx, tx, record, &message, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		return p.AppendMessage(ctx, &message)
	})
}

// errCoordinatorNothingPending aborts the start transaction when the
// re-check inside the transaction finds no work.
var errCoordinatorNothingPending = errors.New("coordinator: nothing pending")

// coordinatorTurnState is the mutable state of one running turn. mu
// serializes the delta buffer, the assistant message and tool messages
// between the backend's update callback, the flush ticker and completion.
type coordinatorTurnState struct {
	coordinator    *conversationTurnCoordinator
	conversationID string
	organizationID tracker.OrganizationID
	projectID      tracker.ProjectID
	model          string
	usage          runner.AgentTokenCounts
	normalizeUsage func(runner.AgentTokenUsage) runner.AgentTokenUsage
	mu             sync.Mutex
	assistant      conversationMessageRecord
	users          []conversationMessageRecord
	buffered       strings.Builder
	firstBuffered  time.Time
	textBoundary   bool
	threadID       string
	// preferences are the conversation's turn preferences as they stood when
	// the turn started.
	preferences   conversation.Preferences
	modelChoice   bool
	budgetStopped bool
	toolMessages  map[string]conversationMessageRecord
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

	state := &coordinatorTurnState{coordinator: c, conversationID: conversationID, toolMessages: map[string]conversationMessageRecord{}}
	var transcript []conversationMessageRecord
	var pending []conversationMessageRecord
	var refusal error
	turnCtx, err := c.service.PrepareConversationContents(turnCtx, conversationID, conversationMessageRecord{})
	if err != nil {
		return false, fmt.Errorf("prepare coordinator assistant: %w", err)
	}
	err = c.write(turnCtx, conversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error {
		if !coordinatorHandles(*record) {
			return errCoordinatorNothingPending
		}
		if len(pending) == 0 {
			return errCoordinatorNothingPending
		}
		decision, err := coordinatorMonthlyBudget(ctx, tx, record.ProjectID, now)
		if err != nil {
			return err
		}
		if !decision.Allowed {
			return errCoordinatorNothingPending
		}
		refusal = hubapi.FromFeatureError(c.server.database.plansService().RequireFeatureRecord(ctx, tx, "native_execution", now))
		var limit *hostedplans.LimitError
		if refusal != nil && !errors.As(refusal, &limit) {
			return refusal
		}
		if c.server.hasLunaCoordinator() && c.server.database.aiCreditMode != "" {
			d := c.server.database
			account, err := tx.BillingCreditAccount(ctx, d.aiCreditMode)
			if err != nil {
				return err
			}
			balance, failure := account.BalanceMicros, account.Failure
			refusal = nil
			included, err := d.includedAICreditsFromReader(ctx, tx, now)
			if err != nil {
				return err
			}
			if balance+included.remaining() <= 0 {
				owner := false
				credential, err := tx.AccessCredential(ctx, pending[len(pending)-1].Actor.PrincipalID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if credential.UserID != "" {
					member, err := tx.AccessMember(ctx, credential.UserID)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					owner = member.Active && member.Role == "owner"
				}
				message := "AI credits are exhausted. Ask an organization owner to buy credits in billing settings."
				if owner {
					message = "AI credits are exhausted. Buy credits in billing settings."
					if failure != "" {
						message += " Auto-fund is disabled: " + failure + "."
					}
				}
				refusal = errors.New(message)
			}
		}
		if refusal == nil && c.server.hasLunaCoordinator() {
			state.modelChoice, err = c.modelChoiceGranted(ctx, tx, record.OrganizationID, now)
			if err != nil {
				return err
			}
			if model := record.Preferences.ModelValue(); model != "" {
				models, err := c.modelChoices(ctx, tx, record.ProjectID, now)
				if err != nil {
					return err
				}
				if !slices.ContainsFunc(models, func(choice conversationModel) bool { return choice.ID == model }) {
					refusal = fmt.Errorf("preferences: model %q is not one of the available choices", model)
				}
			}
		}
		for i := range pending {
			pending[i].Delivery = conversation.DeliverySending
			if err := c.updateMessage(ctx, tx, pending[i], now); err != nil {
				return err
			}
			pending[i].UpdatedAt = now
		}
		state.users = pending
		state.organizationID = record.OrganizationID
		state.projectID = record.ProjectID
		state.threadID = record.ProviderThreadID
		state.preferences = record.Preferences
		if err := c.appendMessage(ctx, tx, record, &state.assistant, now); err != nil {
			return err
		}
		execution := conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{ThreadID: record.ProviderThreadID, TurnID: state.assistant.ID}}
		return c.updateExecution(ctx, tx, record, execution, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		var err error
		pending, err = c.pendingMessages(p.Context(ctx), nil, p.Record.ID)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return errCoordinatorNothingPending
		}
		if p.Record.ProviderThreadID == "" {
			transcript, err = c.transcript(p.Context(ctx), nil, p.Record.ID, pending[0].Seq)
			if err != nil {
				return err
			}
		}
		for _, message := range pending {
			message.Delivery = conversation.DeliverySending
			if err := p.UpdateMessage(ctx, message); err != nil {
				return err
			}
		}
		if state.assistant.ID == "" {
			state.assistant = conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
		}
		state.assistant.ThreadID = p.Record.ProviderThreadID
		if err := p.AppendMessage(ctx, &state.assistant); err != nil {
			return err
		}
		c.mu.Lock()
		turn.id = state.assistant.ID
		c.mu.Unlock()
		return p.UpdateExecution(ctx, conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{ThreadID: p.Record.ProviderThreadID, TurnID: state.assistant.ID}})
	})
	if errors.Is(err, errCoordinatorNothingPending) || errors.Is(err, errCoordinatorLinked) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("start coordinator turn: %w", err)
	}

	if refusal != nil {
		state.assistant.Text = refusal.Error()
		ctx, cancel := context.WithTimeout(context.Background(), coordinatorWriteTimeout)
		defer cancel()
		return true, state.finish(ctx, conversation.DeliveryFailed, refusal, nil)
	}

	// Files the user attached to the pending messages ride the turn: the
	// hub-side coordinator reads them straight out of the store, where the
	// runner-dispatched one downloads them.
	attachments, runErr := c.coordinatorAttachments(turnCtx, state.users)
	prompt := coordinatorPrompt(transcript, state.users)
	if runErr == nil {
		record, err := c.readConversation(turnCtx, nil, conversationID)
		if err != nil {
			runErr = err
		} else {
			tools := newCoordinatorToolset(c, state)
			actorCtx, contextErr := tools.actionContext(turnCtx, record)
			if contextErr != nil {
				if record.ReadAudience != "" || c.server.config.Hosted != nil {
					runErr = contextErr
				}
			} else {
				credential, authErr := currentHubOperator(actorCtx)
				if authErr != nil {
					runErr = authErr
				} else {
					actorCtx, authErr = operatortool.AuthorizeCurrent(actorCtx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(record.ProjectID)})
					if authErr != nil {
						runErr = authErr
					} else {
						runErr = c.authorizeRecord(actorCtx, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential}, record, false)
						if runErr == nil {
							runErr = tools.restrictReadAudience(actorCtx, record, credential)
						}
					}
				}
			}
			if runErr == nil {
				evidence := tools.workflowEvidence(turnCtx, record)
				raw, err := json.Marshal(evidence)
				if err != nil {
					runErr = err
				} else if len(raw) > coordinatorToolResultBytes {
					prompt = "Workflow evidence is too large; read scoped workflow tools before answering.\n\n" + prompt
				} else {
					prompt = "<workflow_evidence>\n" + escapeCoordinatorData(string(raw)) + "\n</workflow_evidence>\n\n" + prompt
				}
			}
		}
		if runErr == nil && record.SubjectWorkItemID != "" {
			data, err := newCoordinatorToolset(c, state).issueContext(turnCtx, record)
			if err != nil {
				runErr = err
			} else {
				prompt, runErr = coordinatorSubjectPrompt(prompt, record, data)
			}
		}
	}
	if runErr == nil {
		prompt, runErr = coordinatorSkillsPrompt(prompt)
	}
	request := runner.AgentTurnRequest{
		Workspace:        c.service.Config.Workspace,
		Prompt:           appendCoordinatorData(prompt, attachments),
		Resume:           runner.AgentResume{ThreadID: state.threadID},
		ReadOnly:         true,
		Model:            c.service.Config.Model,
		ReasoningEffort:  c.service.Config.ReasoningEffort,
		TurnTimeout:      coordinatorTurnTimeout,
		MaxDuration:      coordinatorMaxDuration,
		ToolInstructions: coordinatorInstructions,
	}
	if strings.TrimSpace(request.ReasoningEffort) == "" {
		request.ReasoningEffort = coordinatorDefaultEffort
	}
	// An explicit turn preference overrides the hub's configured default;
	// "auto" leaves it alone.
	backend := c.service.Config.Backend
	if c.server.hasLunaCoordinator() && (!state.modelChoice || state.preferences.ModelValue() == "" || state.preferences.ModelValue() == genkitbackend.Model) {
		if state.preferences.ModelValue() == genkitbackend.Model {
			request.Model = genkitbackend.Model
		}
		if effort := state.preferences.EffortValue(); effort == "low" || effort == "medium" {
			request.ReasoningEffort = effort
		}
	} else {
		if model := state.preferences.ModelValue(); model != "" {
			request.Model = model
		}
		if effort := state.preferences.EffortValue(); effort != "" {
			request.ReasoningEffort = effort
		}
		if c.server.hasLunaCoordinator() && runErr == nil {
			if c.service.Config.ModelBackend == nil {
				runErr = errors.New("conversation coordinator model backend is unavailable")
			} else {
				backend, request.Workspace, runErr = c.service.Config.ModelBackend()
				if runErr == nil && backend == nil {
					runErr = errors.New("conversation coordinator model backend is unavailable")
				}
			}
		}
	}
	state.model = request.Model
	_, genkit := backend.(*genkitbackend.Backend)
	state.normalizeUsage = runner.NewTokenUsageNormalizer(request.Resume.ThreadID != "" && !genkit)
	var result runner.AgentTurnResult
	if runErr == nil {
		// A turn that cannot read its attachments ends as failed through
		// the same completion as a provider failure, so its messages and
		// execution never stay in flight.
		stopFlusher := state.startFlusher(turnCtx)
		result, runErr = c.callBackend(turnCtx, backend, request, state)
		stopFlusher()
	}
	// The final writes use a fresh context: cancellation must persist its
	// outcome rather than lose it.
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), coordinatorWriteTimeout)
	defer cancelWrite()
	state.flush(writeCtx)
	if result.ThreadID != "" {
		state.observeThread(writeCtx, result.ThreadID)
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
	if state.usage.InputTokens > 0 || state.usage.OutputTokens > 0 {
		provider := "codex"
		if genkit {
			provider = "openai"
		}
		usage = &ConversationUsage{OrganizationID: state.organizationID, ProjectID: state.projectID, ConversationID: conversationID, TurnID: state.assistant.ID, Provider: provider, Model: state.model, Tokens: state.usage, Outcome: outcome, OccurredAt: state.assistant.CreatedAt}
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

// transcript returns the last messages before seq as context for a turn
// without a provider thread.
func (c *conversationTurnCoordinator) transcript(ctx context.Context, query hubapi.NativeQueryer, conversationID string, beforeSeq int64) ([]conversationMessageRecord, error) {
	messages, err := c.coordinatorMessages(ctx, conversationstore.MessageFilter{ConversationID: conversationID, BeforeSequence: beforeSeq, Limit: coordinatorTranscriptMessages})
	if err != nil {
		return nil, err
	}
	filtered := messages[:0]
	for _, message := range messages {
		if message.Kind == conversation.MessageText && (message.Role == conversation.RoleUser || message.Role == conversation.RoleAssistant) && strings.TrimSpace(message.Text) != "" {
			filtered = append(filtered, message)
		}
	}
	return filtered, nil
}

// coordinatorAttachment is one text file a pending message carries, with its
// bytes read from the store.
func (s *coordinatorTurnState) handleUpdate(ctx context.Context, update runner.AgentUpdate) {
	if model := strings.TrimSpace(update.Model); model != "" {
		s.mu.Lock()
		s.model = model
		s.mu.Unlock()
	}
	if update.ThreadID != "" {
		s.observeThread(ctx, update.ThreadID)
	}
	switch update.Type {
	case runner.AgentUpdateMessageDelta:
		s.bufferDelta(ctx, update.Delta)
	case runner.AgentUpdateTokenUsage:
		s.mu.Lock()
		if s.normalizeUsage != nil {
			update.Tokens = s.normalizeUsage(update.Tokens)
		}
		s.usage = runner.AgentTokenCounts{
			InputTokens: update.Tokens.InputTokens, CachedInputTokens: update.Tokens.CachedInputTokens,
			OutputTokens: update.Tokens.OutputTokens, ReasoningOutputTokens: update.Tokens.ReasoningOutputTokens,
			TotalTokens: update.Tokens.TotalTokens,
		}
		s.mu.Unlock()
	case runner.AgentUpdateToolStarted, runner.AgentUpdateToolCompleted:
		s.endTextSegment(ctx)
		s.recordTool(ctx, update)
	}
}

// observeThread persists a provider thread id the first time it is seen or
// when it changes.
func (s *coordinatorTurnState) observeThread(ctx context.Context, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threadID == "" || threadID == s.threadID {
		return
	}
	s.threadID = threadID
	err := s.coordinator.write(ctx, s.conversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error {
		if record.ProviderThreadID == threadID && record.ProviderThreadRunnerID == "" && record.ProviderThreadOrigin == conversation.ThreadOriginCoordinator {
			return nil
		}
		record.ProviderThreadID = threadID
		// The hub-side coordinator owns this thread, not a runner.
		record.ProviderThreadRunnerID = ""
		// It is still a coordinator thread, so a worker attempt is handed a
		// transcript even if a runner ever reached it.
		record.ProviderThreadOrigin = conversation.ThreadOriginCoordinator
		return s.coordinator.saveConversation(ctx, tx, record, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		if p.Record.ProviderThreadID == threadID && p.Record.ProviderThreadRunnerID == "" && p.Record.ProviderThreadOrigin == conversation.ThreadOriginCoordinator {
			return nil
		}
		p.Record.ProviderThreadID = threadID
		p.Record.ProviderThreadRunnerID = ""
		p.Record.ProviderThreadOrigin = conversation.ThreadOriginCoordinator
		return p.SaveConversation(ctx)
	})
	if err != nil {
		s.coordinator.logger.Warn("coordinator could not persist provider thread", "conversation_id", s.conversationID, "error", err)
	}
}

func (s *coordinatorTurnState) bufferDelta(ctx context.Context, delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	if s.buffered.Len() == 0 {
		s.firstBuffered = time.Now()
	}
	if s.textBoundary {
		if s.assistant.Text != "" {
			s.buffered.WriteString("\n\n")
		}
		s.textBoundary = false
	}
	s.buffered.WriteString(delta)
	due := s.buffered.Len() >= coordinatorDeltaFlushBytes || time.Since(s.firstBuffered) >= coordinatorDeltaFlushInterval
	s.mu.Unlock()
	if due {
		s.flush(ctx)
	}
}

func (s *coordinatorTurnState) endTextSegment(ctx context.Context) {
	s.flush(ctx)
	s.mu.Lock()
	s.textBoundary = true
	s.mu.Unlock()
}

// startFlusher writes buffered deltas at the coalescing interval. The
// returned function stops it and waits for it to exit.
func (s *coordinatorTurnState) startFlusher(ctx context.Context) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(coordinatorDeltaFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.flush(ctx)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// flush stores the buffered text as one delta event.
func (s *coordinatorTurnState) flush(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buffered.Len() == 0 {
		return
	}
	text := s.buffered.String()
	s.buffered.Reset()
	updated := s.assistant
	err := s.coordinator.write(ctx, s.conversationID, func(ctx context.Context, tx tenantstore.Writer, _ *conversationRecord, now time.Time) error {
		return s.coordinator.appendDelta(ctx, tx, &updated, text, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		return p.AppendDelta(ctx, s.assistant, text)
	})
	if err == nil {
		s.assistant = updated
	}
	if err != nil {
		// Keep the text so the completion write carries it.
		s.assistant.Text += text
		s.coordinator.logger.Warn("coordinator could not persist delta", "conversation_id", s.conversationID, "error", err)
	}
}

// recordTool appends a system tool message when a tool starts and updates
// it with the outcome when it completes.
func (s *coordinatorTurnState) recordTool(ctx context.Context, update runner.AgentUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary := coordinatorToolSummary(update)
	data, err := json.Marshal(map[string]string{"tool": update.Tool, "item_id": update.ItemID, "status": update.Status})
	if err != nil {
		return
	}
	prepared, err := s.coordinator.service.PrepareConversationContents(ctx, s.conversationID, conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageTool, Text: summary, Data: data, Delivery: conversation.DeliveryCompleted})
	if err != nil {
		s.coordinator.logger.Warn("publish tool content", "error", err)
		return
	}
	ctx = prepared
	existing, known := s.toolMessages[update.ItemID]
	var message conversationMessageRecord
	err = s.coordinator.write(ctx, s.conversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error {
		if known && update.ItemID != "" {
			existing.Text = summary
			existing.Data = data
			if err := s.coordinator.updateMessage(ctx, tx, existing, now); err != nil {
				return err
			}
			s.toolMessages[update.ItemID] = existing
			return nil
		}
		if err := s.coordinator.appendMessage(ctx, tx, record, &message, now); err != nil {
			return err
		}
		if update.ItemID != "" {
			s.toolMessages[update.ItemID] = message
		}
		return nil
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		if known && update.ItemID != "" {
			updated := existing
			updated.Text, updated.Data = summary, data
			return p.UpdateMessage(ctx, updated)
		}
		if message.ID == "" {
			message = conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageTool, Text: summary, Data: data, Delivery: conversation.DeliveryCompleted, ThreadID: s.threadID, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
		}
		return p.AppendMessage(ctx, &message)
	})
	if err != nil {
		s.coordinator.logger.Warn("coordinator could not persist tool message", "conversation_id", s.conversationID, "error", err)
	}
}

// finish persists the outcome of the turn for the assistant message, the
// user messages and the execution state in one transaction.
func (s *coordinatorTurnState) finish(ctx context.Context, outcome conversation.Delivery, runErr error, usage *ConversationUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	userDelivery := conversation.DeliveryDelivered
	var failure *conversation.ReceiptError
	execution := conversation.Execution{Status: conversation.ExecutionIdle, Owner: conversation.Owner{ThreadID: s.threadID}}
	switch outcome {
	case conversation.DeliveryFailed:
		userDelivery = conversation.DeliveryFailed
		execution.Status = conversation.ExecutionFailed
		execution.Error = boundRunes(runErr.Error(), coordinatorErrorRunes)
		failure = &conversation.ReceiptError{Code: "coordinator_error", Message: execution.Error}
	case conversation.DeliveryUnknown:
		userDelivery = conversation.DeliveryUnknown
		execution.Status = conversation.ExecutionUnknown
		failure = &conversation.ReceiptError{Code: "unknown", Message: "The coordinator turn stopped before it answered"}
	}
	if s.budgetStopped {
		userDelivery = conversation.DeliverySaved
		failure = nil
	}

	s.assistant.Delivery = outcome
	s.assistant.ThreadID = s.threadID
	if outcome == conversation.DeliveryFailed {
		data, err := json.Marshal(map[string]string{"error": execution.Error})
		if err != nil {
			return err
		}
		s.assistant.Data = data
	}
	prepared, err := s.coordinator.service.PrepareConversationContents(ctx, s.conversationID, s.assistant)
	if err != nil {
		return err
	}
	ctx = prepared
	var preparedUsage preparedConversationUsage
	if usage != nil {
		preparedUsage, err = hubapi.FeatureResult(s.coordinator.server.database.creditsService().PrepareUsage(ctx, credits.Usage(*usage)))
		if err != nil {
			return err
		}
	}
	return s.coordinator.write(ctx, s.conversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error {
		if usage != nil {
			if err := hubapi.FromFeatureError(s.coordinator.server.database.creditsService().WriteUsage(ctx, tx, preparedUsage)); err != nil {
				return err
			}
		}
		if err := s.coordinator.updateMessage(ctx, tx, s.assistant, now); err != nil {
			return err
		}
		for i := range s.users {
			// setControlDelivery rather than updateMessage: the receipt must
			// follow the message so the client can offer the retry command
			// a failed or unknown turn leaves as the only way forward.
			if err := s.coordinator.setControlDelivery(ctx, tx, &s.users[i], userDelivery, failure, now); err != nil {
				return err
			}
		}
		if s.threadID != "" {
			record.ProviderThreadID = s.threadID
		}
		return s.coordinator.updateExecution(ctx, tx, record, execution, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		if err := p.UpdateMessage(ctx, s.assistant); err != nil {
			return err
		}
		for _, message := range s.users {
			if err := p.SetControlDelivery(ctx, message, userDelivery, failure); err != nil {
				return err
			}
		}
		if s.threadID != "" {
			p.Record.ProviderThreadID = s.threadID
		}
		return p.UpdateExecution(ctx, execution)
	})
}
