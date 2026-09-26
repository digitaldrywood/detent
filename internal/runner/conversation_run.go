package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

// conversationSummaryLimit bounds the summary of a tool item; the hub caps
// summaries at 4000 bytes.
const conversationSummaryLimit = 2000

// conversationCloseTimeout bounds the final flush and unbind of a run.
const conversationCloseTimeout = 15 * time.Second

// conversationRun is a run's binding to its issue conversation. A nil
// receiver is safe everywhere so the turn path needs no session checks.
type conversationRun struct {
	session ConversationSession
	logger  *slog.Logger
	issue   connector.Issue

	mu               sync.Mutex
	pendingDelivered bool
	threadID         string
	turnID           string
	turnCompleted    bool
}

type conversationRunContextKey struct{}

// bindConversation binds the run to the issue's conversation when both the
// execution and the backend support live control. A conversation never
// blocks a run: every failure is logged and the run continues without one.
func (r *Runner) bindConversation(ctx context.Context, req RunRequest, backend AgentBackend) *conversationRun {
	execution, ok := req.Execution.(ConversationExecution)
	if !ok {
		return nil
	}
	live, ok := backend.(AgentLiveBackend)
	if !ok || !live.SupportsLiveControl() {
		return nil
	}
	session, err := execution.BindConversation(ctx, ConversationCapabilities{Steer: true, Interrupt: true, Answer: true, Continue: true})
	if err != nil {
		level := slog.LevelWarn
		if errors.Is(err, ErrNoConversation) {
			level = slog.LevelDebug
		}
		r.logWorkerEventLevel(level, req.Issue, "worker_conversation_bind_skipped", telemetry.WorkAttemptIDKey, req.WorkAttemptID, "error", errorString(err))
		return nil
	}
	if session == nil {
		return nil
	}
	r.logWorkerEvent(req.Issue, "worker_conversation_bound", telemetry.WorkAttemptIDKey, req.WorkAttemptID, "conversation_id", session.ConversationID(), "thread_id", session.ResumeThreadID(), "resume", session.ResumeMode(), "thread_origin", conversationThreadOrigin(session), "coordinator", session.Coordinator())
	return &conversationRun{session: session, logger: r.logger, issue: req.Issue}
}

// conversationThreadOrigin reports the kind of turn the bound thread belongs
// to when the session exposes it. The hub decides what a bind resumes
// (decisions section 9.3); the origin is recorded on the run so an inherited
// coordinator thread is visible in the log rather than only in the answer the
// model gives.
func conversationThreadOrigin(session ConversationSession) string {
	origin, ok := session.(interface{ ThreadOrigin() string })
	if !ok {
		return ""
	}
	return origin.ThreadOrigin()
}

// attach stores the run on the context so turns started under it find it.
func (c *conversationRun) attach(ctx context.Context) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, conversationRunContextKey{}, c)
}

func conversationRunFromContext(ctx context.Context) *conversationRun {
	if ctx == nil {
		return nil
	}
	run, ok := ctx.Value(conversationRunContextKey{}).(*conversationRun)
	if !ok {
		return nil
	}
	return run
}

// prepareTurn wires the conversation into a turn request: the control
// object, the conversation's thread when the run has no resume state, and
// the queued user messages until a turn has delivered them. Instructions come
// first: the conversation's data blocks are appended after the built prompt.
func (c *conversationRun) prepareTurn(request AgentTurnRequest) AgentTurnRequest {
	if c == nil {
		return request
	}
	c.mu.Lock()
	c.threadID, c.turnID, c.turnCompleted = "", "", false
	pendingDelivered := c.pendingDelivered
	c.mu.Unlock()
	request.ConversationControl = c.session.Control(ConversationTurnHooks{InputRequested: c.inputRequested})
	request = applyConversationPreferences(request, c.session.Preferences(), c.session.Coordinator())
	if request.Resume.ThreadID == "" && request.Resume.SessionID == "" {
		request.Resume.ThreadID = c.session.ResumeThreadID()
	}
	if pendingDelivered {
		return request
	}
	if text, _ := c.session.PendingPrompt(); strings.TrimSpace(text) != "" {
		request.Prompt = appendConversationData(request.Prompt, text)
	}
	// Files attached to those queued messages ride the same turn: images as
	// provider image input, text files as a data block after the prompt
	// (decisions section 17.1).
	if attachments := c.session.PendingAttachments(); len(attachments) > 0 {
		request.Attachments = append(request.Attachments, attachments...)
		request.Prompt = appendConversationData(request.Prompt, attachmentDataBlock(attachments))
	}
	return request
}

// appendConversationData appends a conversation data block after the prompt
// the workflow built, so the instructions always precede the data.
func appendConversationData(prompt string, data string) string {
	prompt, data = strings.TrimSpace(prompt), strings.TrimSpace(data)
	switch {
	case data == "":
		return prompt
	case prompt == "":
		return data
	default:
		return prompt + "\n\n" + data
	}
}

// statusPoster exposes the session's structured status channel to tools that
// publish a card for the user, such as propose_issue. A run without a bound
// conversation has none, and the tool reports that itself.
func (c *conversationRun) statusPoster() CoordinatorStatusPoster {
	if c == nil {
		return nil
	}
	return func(ctx context.Context, data map[string]any, summary string) error {
		return c.session.PostStatus(ctx, data, summary)
	}
}

// wrapUpdates translates provider updates into turn events before handing
// them to the run's own handler.
func (c *conversationRun) wrapUpdates(handler agentContextUpdateHandler) agentContextUpdateHandler {
	if c == nil {
		return handler
	}
	return func(ctx context.Context, update AgentUpdate) error {
		c.observe(ctx, update)
		return handler(ctx, update)
	}
}

func (c *conversationRun) observe(ctx context.Context, update AgentUpdate) {
	if update.AuxiliaryTurn {
		return
	}
	switch update.Type {
	case AgentUpdateTurnStarted:
		c.mu.Lock()
		c.threadID, c.turnID, c.turnCompleted = update.ThreadID, update.TurnID, false
		deliver := !c.pendingDelivered
		c.pendingDelivered = true
		c.mu.Unlock()
		events := []ConversationTurnEvent{{Type: ConversationEventTurnStarted, ThreadID: update.ThreadID, TurnID: update.TurnID}}
		if deliver {
			_, keys := c.session.PendingPrompt()
			for _, key := range keys {
				events = append(events, ConversationTurnEvent{Type: ConversationEventControlResult, Key: key, Status: ConversationControlDelivered})
			}
		}
		c.report(ctx, events...)
	case AgentUpdateMessageDelta:
		if update.ItemID == "" || update.Delta == "" {
			return
		}
		c.report(ctx, ConversationTurnEvent{Type: ConversationEventDelta, ThreadID: update.ThreadID, TurnID: update.TurnID, ProviderItemID: update.ItemID, Text: update.Delta})
	case AgentUpdateToolStarted, AgentUpdateToolCompleted:
		c.report(ctx, ConversationTurnEvent{Type: ConversationEventItem, ThreadID: update.ThreadID, TurnID: update.TurnID, ProviderItemID: update.ItemID, Kind: ConversationItemTool, Summary: conversationItemSummary(update)})
	case AgentUpdateTurnCompleted:
		c.mu.Lock()
		turnID := c.turnID
		if update.TurnID != "" {
			turnID = update.TurnID
		}
		already := c.turnCompleted || turnID == ""
		c.turnCompleted = true
		c.mu.Unlock()
		if already {
			return
		}
		status := ConversationTurnCompleted
		switch strings.ToLower(update.Status) {
		case "", "completed":
		case "interrupted", "cancelled", "canceled":
			status = ConversationTurnInterrupted
		default:
			status = ConversationTurnFailed
		}
		c.report(ctx, ConversationTurnEvent{Type: ConversationEventTurnCompleted, ThreadID: update.ThreadID, TurnID: turnID, Status: status, Error: strings.TrimSpace(update.BackendErrorMessage)})
	}
}

// inputRequested publishes a provider question to the conversation.
func (c *conversationRun) inputRequested(request AgentInputRequest) error {
	event := ConversationTurnEvent{Type: ConversationEventQuestionOpened, RequestID: request.ID, ThreadID: request.ThreadID, TurnID: request.TurnID, Prompts: conversationPrompts(request.Questions)}
	return c.session.Report(context.Background(), []ConversationTurnEvent{event})
}

// finishTurn reports the turn's end when the provider did not, and releases
// controls still queued for it.
func (c *conversationRun) finishTurn(ctx context.Context, result AgentTurnResult, turnErr error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	turnID := c.turnID
	if turnID == "" {
		turnID = result.TurnID
	}
	completed := c.turnCompleted
	c.turnCompleted = true
	c.mu.Unlock()
	if !completed && turnID != "" {
		c.report(ctx, ConversationTurnEvent{Type: ConversationEventTurnCompleted, ThreadID: result.ThreadID, TurnID: turnID, Status: conversationTurnStatus(ctx, turnErr), Error: errorString(turnErr)})
	}
	c.session.FinishTurn()
}

// close unbinds the run with its outcome.
func (c *conversationRun) close(ctx context.Context, result RunResult, runErr error) {
	if c == nil {
		return
	}
	outcome := conversationOutcome(ctx, result, runErr, c.session.LastTurnStatus())
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conversationCloseTimeout)
	defer cancel()
	if err := c.session.Close(closeCtx, outcome, runErr); err != nil {
		c.logger.Warn("conversation unbind failed", "issue_id", c.issue.ID, "conversation_id", c.session.ConversationID(), "outcome", outcome, "error", err)
	}
}

func (c *conversationRun) report(ctx context.Context, events ...ConversationTurnEvent) {
	if err := c.session.Report(ctx, events); err != nil {
		c.logger.Warn("conversation turn events not queued", "issue_id", c.issue.ID, "conversation_id", c.session.ConversationID(), "error", err)
	}
}

// conversationOutcome maps the run result onto the unbind outcome. A turn the
// provider reported as interrupted or failed keeps that outcome even when the
// backend then returned without an error.
func conversationOutcome(ctx context.Context, result RunResult, runErr error, lastTurnStatus string) string {
	switch {
	case ctx != nil && ctx.Err() != nil:
		return ConversationOutcomeInterrupted
	case errors.Is(runErr, context.Canceled) || cooperativeStopError(runErr):
		return ConversationOutcomeCancelled
	case runErr != nil || result.FinalState != FinalStateCompleted:
		return ConversationOutcomeFailed
	}
	switch strings.ToLower(strings.TrimSpace(lastTurnStatus)) {
	case ConversationTurnInterrupted:
		return ConversationOutcomeInterrupted
	case ConversationTurnFailed:
		return ConversationOutcomeFailed
	default:
		return ConversationOutcomeSucceeded
	}
}

// conversationTurnStatus maps a turn error onto the turn_completed status.
func conversationTurnStatus(ctx context.Context, turnErr error) string {
	switch {
	case turnErr == nil:
		return ConversationTurnCompleted
	case ctx != nil && ctx.Err() != nil, errors.Is(turnErr, context.Canceled), cooperativeStopError(turnErr):
		return ConversationTurnInterrupted
	default:
		return ConversationTurnFailed
	}
}

// conversationItemSummary renders a bounded, single-line summary of a tool
// update for the conversation timeline.
func conversationItemSummary(update AgentUpdate) string {
	tool := strings.TrimSpace(update.Tool)
	if tool == "" {
		tool = "tool"
	}
	detail := strings.TrimSpace(update.Command)
	if detail == "" {
		detail = strings.TrimSpace(update.Delta)
	}
	summary := tool
	if update.Type == AgentUpdateToolCompleted {
		status := strings.TrimSpace(update.Status)
		if status == "" {
			status = "completed"
		}
		summary += " " + status
	}
	if detail != "" {
		if line, _, found := strings.Cut(detail, "\n"); found {
			detail = line + " …"
		}
		summary += ": " + detail
	}
	return truncateConversationText(summary, conversationSummaryLimit)
}

func truncateConversationText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && cut < len(value) && (value[cut]&0xC0) == 0x80 {
		cut--
	}
	return value[:cut]
}

// conversationPrompt is the hub's prompt shape for question_opened.
type conversationPrompt struct {
	ID       string                     `json:"id"`
	Header   string                     `json:"header"`
	Question string                     `json:"question"`
	Options  []conversationPromptOption `json:"options"`
	FreeText bool                       `json:"free_text"`
}

type conversationPromptOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// conversationPrompts converts the provider's raw questions into the hub's
// prompt shape. Missing identifiers are generated and an unreadable or empty
// request becomes one free-text prompt, so a question can always be shown.
func conversationPrompts(raw json.RawMessage) json.RawMessage {
	var questions []struct {
		ID       string `json:"id"`
		Header   string `json:"header"`
		Question string `json:"question"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
		IsOther  *bool `json:"isOther"`
		FreeText *bool `json:"free_text"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &questions); err != nil {
			questions = nil
		}
	}
	prompts := make([]conversationPrompt, 0, max(len(questions), 1))
	for i, question := range questions {
		prompt := conversationPrompt{ID: strings.TrimSpace(question.ID), Header: question.Header, Question: question.Question, Options: []conversationPromptOption{}}
		if prompt.ID == "" {
			prompt.ID = "q" + strconv.Itoa(i+1)
		}
		for _, option := range question.Options {
			prompt.Options = append(prompt.Options, conversationPromptOption{Label: option.Label, Description: option.Description})
		}
		switch {
		case question.FreeText != nil:
			prompt.FreeText = *question.FreeText
		case question.IsOther != nil:
			prompt.FreeText = *question.IsOther
		default:
			prompt.FreeText = len(prompt.Options) == 0
		}
		prompts = append(prompts, prompt)
	}
	if len(prompts) == 0 {
		prompts = append(prompts, conversationPrompt{ID: "q1", Question: "Input requested", Options: []conversationPromptOption{}, FreeText: true})
	}
	encoded, err := json.Marshal(prompts)
	if err != nil {
		return json.RawMessage(`[{"id":"q1","header":"","question":"Input requested","options":[],"free_text":true}]`)
	}
	return encoded
}

// applyConversationPreferences applies the conversation's turn preferences
// to the request the run built. An explicit model or effort replaces the
// run's own selection; "auto" leaves it alone. Access only ever tightens:
// read_only forbids writes, and full never re-enables what the run already
// forbade, because a coordinator turn and a routine run are read-only by
// their mode (decisions section 14).
func applyConversationPreferences(request AgentTurnRequest, preferences ConversationPreferences, coordinator bool) AgentTurnRequest {
	if model := preferences.ModelValue(); model != "" {
		request.Model = model
	}
	if effort := preferences.EffortValue(); effort != "" {
		request.ReasoningEffort = effort
	}
	if preferences.ReadOnly() || coordinator {
		request.ReadOnly = true
	}
	return request
}
