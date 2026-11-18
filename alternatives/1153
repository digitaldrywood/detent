package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// ErrStaleConversation reports that the hub no longer recognises this
// attempt as the owner of the conversation execution.
var ErrStaleConversation = errors.New("conversation execution is stale")

const (
	conversationFlushInterval = 200 * time.Millisecond
	conversationReportBackoff = 250 * time.Millisecond
	conversationPollWait      = 25 * time.Second
	// conversationPollSlack is how long past the hub's wait one poll may take
	// for the round trip and the hub's own answer before the client gives up.
	conversationPollSlack = 10 * time.Second
	// conversationPollWarnInterval throttles the poll failure warning. A poll
	// that keeps failing repeats every backoff, so logging each one buries the
	// runner log; one line a minute says the same thing and carries the count.
	conversationPollWarnInterval = time.Minute
	// conversationMaxQueuedBytes bounds the event text a session holds while
	// posting is slow or failing. Past it the oldest streamed deltas are
	// dropped; every other event is kept.
	conversationMaxQueuedBytes = 1 << 20
)

const (
	conversationCommandQueueSize       = 64
	conversationMaxBatchEvents         = 500
	conversationReportTimeout          = 15 * time.Second
	conversationQuestionReportAttempts = 3
)

// ConversationIdentity is the owner tuple every worker request carries.
type ConversationIdentity struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token"`
	AttemptID    string               `json:"attempt_id"`
}

// ConversationBindRequest binds an attempt to the issue's conversation.
type ConversationBindRequest struct {
	ConversationIdentity
	RunID        string                          `json:"run_id"`
	Capabilities runner.ConversationCapabilities `json:"capabilities"`
	ThreadID     string                          `json:"thread_id,omitempty"`
}

// ConversationBindResponse is the hub's answer to a bind. The hub returns a
// resume thread only when this runner produced it; otherwise it sends a
// bounded transcript for the runner to prepend as data.
type ConversationBindResponse struct {
	ConversationID string `json:"conversation_id"`
	Continuation   *bool  `json:"continuation"`
	// Preferences are the conversation's turn preferences: the runner
	// applies the explicit ones to every turn it runs for this attempt.
	Preferences runner.ConversationPreferences `json:"preferences"`
	Resume      struct {
		ThreadID string `json:"thread_id"`
		// ThreadOrigin names the kind of turn the thread belongs to, so a
		// runner can tell a coordinator thread from a worker one without
		// keeping a registry of its own.
		ThreadOrigin string                               `json:"thread_origin"`
		Transcript   []runner.ConversationTranscriptEntry `json:"transcript"`
	} `json:"resume"`
	Pending []ConversationControl `json:"pending"`
	Cursor  int64                 `json:"cursor"`
}

// ConversationExpected is the owner generation a control was accepted for.
type ConversationExpected struct {
	AttemptID string `json:"attempt_id"`
	TurnID    string `json:"turn_id"`
}

// ConversationControl is one user control handed to the worker.
type ConversationControl struct {
	Cursor     int64               `json:"cursor"`
	Key        string              `json:"key"`
	Kind       string              `json:"kind"`
	MessageID  string              `json:"message_id"`
	Text       string              `json:"text,omitempty"`
	QuestionID string              `json:"question_id,omitempty"`
	RequestID  string              `json:"request_id,omitempty"`
	Answers    map[string][]string `json:"answers,omitempty"`
	// Attachments are the files the message carries, each with the Hub path
	// that streams it.
	Attachments []ConversationAttachment `json:"attachments,omitempty"`
	Expected    ConversationExpected     `json:"expected"`
}

// ConversationControlsPage is one long-poll result.
type ConversationControlsPage struct {
	Controls []ConversationControl `json:"controls"`
	Cursor   int64                 `json:"cursor"`
}

// ConversationTurnEventsRequest posts turn events for the bound attempt.
// BatchKey identifies the batch: a retry of the same batch repeats it, so the
// hub can drop a duplicate it already applied.
type ConversationTurnEventsRequest struct {
	ConversationIdentity
	BatchKey string                         `json:"batch_key,omitempty"`
	Events   []runner.ConversationTurnEvent `json:"events"`
}

// ConversationUnbindRequest ends the attempt's binding with its outcome.
type ConversationUnbindRequest struct {
	ConversationIdentity
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// BindConversation binds the attempt to the work item's conversation.
func (c *NativeClient) BindConversation(ctx context.Context, item tracker.NativeWorkItemID, request ConversationBindRequest) (ConversationBindResponse, error) {
	var response ConversationBindResponse
	path, err := NativeItemPath(item)
	if err != nil {
		return response, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/conversation/bind", request, &response)
	return response, conversationError(err)
}

// UnbindConversation ends the attempt's binding with the run outcome.
func (c *NativeClient) UnbindConversation(ctx context.Context, item tracker.NativeWorkItemID, request ConversationUnbindRequest) error {
	path, err := NativeItemPath(item)
	if err != nil {
		return err
	}
	return conversationError(c.client.request(ctx, http.MethodPost, c.base()+path+"/conversation/unbind", request, nil))
}

// ReportConversationTurnEvents posts a batch of turn events and returns the
// conversation's event sequence after the batch.
func (c *NativeClient) ReportConversationTurnEvents(ctx context.Context, conversationID string, request ConversationTurnEventsRequest) (int64, error) {
	var response struct {
		EventSeq int64 `json:"event_seq"`
	}
	err := c.client.request(ctx, http.MethodPost, c.base()+"/conversations/"+url.PathEscape(conversationID)+"/turn-events", request, &response)
	return response.EventSeq, conversationError(err)
}

// ConversationControls long-polls for controls after the cursor. Passing a
// cursor acknowledges every control up to it as sent.
//
// The poll carries its own deadline of wait plus slack rather than the Hub
// client's default request timeout. The default is
// global.DefaultHubRequestTimeoutMillis (10s), shorter than the 25s wait this
// poll asks the hub to hold, so on a default runner configuration every poll
// aborted with "context deadline exceeded" and no steer, answer or interrupt
// ever reached the bound attempt. This is one request that legitimately
// outlives an ordinary Hub call; the client's default still bounds every other
// call.
func (c *NativeClient) ConversationControls(ctx context.Context, conversationID string, identity ConversationIdentity, after int64, wait time.Duration) (ConversationControlsPage, error) {
	page := ConversationControlsPage{Controls: []ConversationControl{}, Cursor: after}
	query := url.Values{"after": {strconv.FormatInt(after, 10)}, "wait": {strconv.Itoa(int(wait / time.Second))}}
	headers := http.Header{
		"X-Detent-Lease":         {string(identity.LeaseID)},
		"X-Detent-Fencing-Token": {strconv.FormatInt(int64(identity.FencingToken), 10)},
		"X-Detent-Attempt":       {identity.AttemptID},
	}
	err := c.client.requestWithDeadline(ctx, conversationPollTimeout(wait), http.MethodGet, c.base()+"/conversations/"+url.PathEscape(conversationID)+"/controls?"+query.Encode(), headers, nil, &page)
	return page, conversationError(err)
}

// conversationPollTimeout is the deadline one long poll gets: the wait the hub
// was asked to hold, plus slack for the round trip and the hub's own answer.
// A non-positive wait is not a long poll and keeps the client's default.
func conversationPollTimeout(wait time.Duration) time.Duration {
	if wait <= 0 {
		return 0
	}
	return wait + conversationPollSlack
}

// conversationError maps hub error codes onto the worker vocabulary.
func conversationError(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case apiErr.Status == http.StatusNotFound:
		return fmt.Errorf("%w: %w", runner.ErrNoConversation, err)
	case apiErr.Code == "stale_execution":
		return fmt.Errorf("%w: %w: %w", ErrStaleConversation, runner.ErrExecutionAuthorityUnavailable, err)
	case apiErr.Code == "revision_conflict":
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		return err
	}
}

// conversationSession is one attempt's binding. A poll goroutine feeds the
// command queue and acknowledges controls by cursor; a report goroutine
// batches turn events; a waiter per control turns its Reply into a
// control_result.
type conversationSession struct {
	lifetimeDone  <-chan struct{}
	lifetimeCause func() error
	timings       SessionTimings
	client        *NativeClient
	item          tracker.NativeWorkItemID
	id            string
	identity      ConversationIdentity
	resume        string
	resumeMode    string
	threadOrigin  string
	continuation  bool
	preferences   runner.ConversationPreferences
	pending       string
	pendingKey    []string
	// pendingAttachments are the files of the messages the bind handed over,
	// fetched once so the first turn can hand them to the provider.
	pendingAttachments []runner.AgentAttachment
	leaseCheck         func(context.Context) error
	logger             *slog.Logger

	commands    chan runner.AgentControl
	cancel      context.CancelFunc
	flushCancel context.CancelFunc
	// pollDone is closed when the control poll loop exits, whether because
	// the session is closing or because the hub refused the poll for good.
	pollDone chan struct{}
	closed   chan struct{}
	stop     chan struct{}
	kick     chan struct{}
	loops    sync.WaitGroup
	waiters  sync.WaitGroup

	mu        sync.Mutex
	thread    string
	turn      string
	turnEnded chan struct{}
	// turnWaiters counts the control waiters of the current turn, so the
	// turn's end is reported only after every control it consumed.
	turnWaiters    *sync.WaitGroup
	cursor         int64
	controlResults int
	events         []runner.ConversationTurnEvent
	reportRoom     chan struct{}
	// held keeps a reported turn_completed, and anything after it, until
	// FinishTurn has settled the turn's controls. The hub refuses turn events
	// once a turn has ended, so a control result posted after the turn's end
	// would be lost and the control settled with the turn instead.
	held       []runner.ConversationTurnEvent
	lastStatus string
	closing    bool
	closeOnce  sync.Once
}

func newConversationSession(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, identity ConversationIdentity, response ConversationBindResponse, leaseCheck func(context.Context) error, logger *slog.Logger, ready <-chan struct{}) *conversationSession {
	pollCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	flushCtx, flushCancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &conversationSession{
		lifetimeDone:  ctx.Done(),
		lifetimeCause: func() error { return context.Cause(ctx) },
		timings:       client.client.sessionTimings.withDefaults(),
		client:        client, item: item, id: response.ConversationID, identity: identity, resume: response.Resume.ThreadID,
		continuation: response.Continuation == nil || *response.Continuation,
		resumeMode:   conversationResumeMode(response),
		threadOrigin: response.Resume.ThreadOrigin,
		preferences:  response.Preferences,
		leaseCheck:   leaseCheck, logger: logger.With("conversation", response.ConversationID),
		commands:    make(chan runner.AgentControl, conversationCommandQueueSize),
		cancel:      cancel,
		flushCancel: flushCancel,
		closed:      make(chan struct{}),
		pollDone:    make(chan struct{}),
		stop:        make(chan struct{}),
		kick:        make(chan struct{}, 1),
		thread:      response.Resume.ThreadID,
		turnEnded:   make(chan struct{}),
		turnWaiters: &sync.WaitGroup{},
		cursor:      response.Cursor,
	}
	var pendingText strings.Builder
	// A transcript arrives only when the provider thread cannot be resumed
	// here; it leads the conversation's data blocks, which the runner appends
	// after the instructions it built.
	if response.Resume.ThreadID == "" {
		pendingText.WriteString(runner.RenderConversationTranscript(response.Resume.Transcript))
	}
	var queued []ConversationControl
	var followUps []string
	var attached []ConversationAttachment
	for _, control := range response.Pending {
		if control.Kind == string(runner.AgentControlMessage) {
			followUps = append(followUps, control.Text)
			s.pendingKey = append(s.pendingKey, control.Key)
			attached = append(attached, control.Attachments...)
			continue
		}
		queued = append(queued, control)
	}
	s.pendingAttachments = s.fetchAttachments(ctx, attached)
	pendingText.WriteString(runner.RenderConversationFollowUps(followUps))
	s.pending = pendingText.String()
	s.loops.Add(2)
	go func() {
		if ready != nil {
			select {
			case <-ready:
			case <-pollCtx.Done():
			}
		}
		s.pollLoop(pollCtx, queued)
	}()
	go func() {
		if ready != nil {
			select {
			case <-ready:
			case <-pollCtx.Done():
			}
		}
		s.reportLoop(pollCtx, flushCtx)
	}()
	return s
}

// conversationResumeMode reports how the bind handed this attempt the
// conversation's history: its own provider thread, a transcript of it, or
// nothing at all.
func conversationResumeMode(response ConversationBindResponse) string {
	switch {
	case response.Resume.ThreadID != "":
		return runner.ConversationResumeThread
	case len(response.Resume.Transcript) > 0:
		return runner.ConversationResumeTranscript
	default:
		return ""
	}
}

func (s *conversationSession) ConversationID() string { return s.id }

func (s *conversationSession) Continuation() bool { return s.continuation }

func (s *conversationSession) ResumeThreadID() string { return s.resume }

// ThreadOrigin reports the kind of turn the bind's thread belongs to:
// "coordinator" or "worker". It is empty when the hub
// did not name one.
func (s *conversationSession) ThreadOrigin() string { return s.threadOrigin }

// ResumeMode reports whether the attempt resumed the provider thread, was
// handed a transcript instead, or started with no history.
func (s *conversationSession) ResumeMode() string { return s.resumeMode }

// LastTurnStatus is the status of the last turn_completed event reported
// through this session.
func (s *conversationSession) LastTurnStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStatus
}

// Preferences are the conversation's turn preferences as the bind reported
// them. They are fixed for the life of the attempt: a change mid-attempt
// reaches the next turn through the next bind.
func (s *conversationSession) Preferences() runner.ConversationPreferences { return s.preferences }

func (s *conversationSession) PendingPrompt() (string, []string) {
	return s.pending, append([]string(nil), s.pendingKey...)
}

func (s *conversationSession) Control(turn runner.ConversationTurnHooks) *runner.AgentConversationControl {
	return &runner.AgentConversationControl{Commands: s.commands, InputRequested: turn.InputRequested, QuestionTimeout: turn.QuestionTimeout}
}

// FinishTurn ends the current turn: waiters of unconsumed controls report
// unknown and the queue is drained so a later turn never sees stale controls.
// Once every control of the turn has its result queued, the held turn end
// follows them.
func (s *conversationSession) FinishTurn() {
	s.mu.Lock()
	ended, waiters := s.turnEnded, s.turnWaiters
	s.turnEnded, s.turnWaiters = make(chan struct{}), &sync.WaitGroup{}
	s.turn = ""
	s.mu.Unlock()
	close(ended)
	s.drainCommands()
	waiters.Wait()
	s.mu.Lock()
	s.releaseHeldLocked()
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// releaseHeldLocked queues the events held behind a turn's end. s.mu is held
// by the caller.
func (s *conversationSession) releaseHeldLocked() {
	if len(s.held) == 0 {
		return
	}
	s.events = append(s.events, s.held...)
	s.held = nil
	s.boundQueuedLocked()
}

// drainCommands empties the queue without blocking. Every drained control
// already has a waiter that reports it unknown.
func (s *conversationSession) drainCommands() {
	for {
		select {
		case <-s.commands:
		default:
			return
		}
	}
}

// Report queues events in order and tracks the live thread and turn so
// controls can be addressed before the hub has processed turn_started.
func (s *conversationSession) Report(ctx context.Context, events []runner.ConversationTurnEvent) error {
	if len(events) == 0 {
		return nil
	}
	lifetimeDone := ctx.Done()
	lifetimeCause := func() error { return context.Cause(ctx) }
	if s.lifetimeCause != nil {
		lifetimeDone, lifetimeCause = s.lifetimeDone, s.lifetimeCause
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}()
	for _, event := range events {
		for {
			if s.closing {
				return fmt.Errorf("report conversation turn events: %w", ErrStaleConversation)
			}
			if len(s.held) > 0 || event.Type == runner.ConversationEventTurnCompleted {
				if len(s.held) >= conversationMaxBatchEvents || conversationQueuedBytes(s.held)+conversationEventBytes(event) > s.timings.withDefaults().MaxQueuedBytes {
					if len(s.held) == 0 {
						return errors.New("conversation turn completion exceeds report capacity")
					}
					return errors.New("finish the previous conversation turn before reporting more events")
				}
				s.held = append(s.held, event)
				break
			}
			if event.Type == runner.ConversationEventQuestionOpened && (len(s.events)+s.controlResults >= conversationMaxBatchEvents || conversationQueuedBytes(s.events)+s.controlResults*s.controlResultBytes() >= s.timings.withDefaults().MaxQueuedBytes) {
				return fmt.Errorf("conversation question publication has no report capacity: %w", ErrUnavailable)
			}
			if len(s.events)+s.controlResults < conversationMaxBatchEvents && (event.Type == runner.ConversationEventDelta || conversationQueuedBytes(s.events)+s.controlResults*s.controlResultBytes() < s.timings.withDefaults().MaxQueuedBytes) {
				s.events = append(s.events, event)
				s.boundQueuedLocked()
				break
			}
			if s.reportRoom == nil {
				s.reportRoom = make(chan struct{})
			}
			room := s.reportRoom
			select {
			case s.kick <- struct{}{}:
			default:
			}
			s.mu.Unlock()
			var err error
			select {
			case <-room:
			case <-s.stop:
				err = ErrStaleConversation
			case <-ctx.Done():
				err = ctx.Err()
			case <-lifetimeDone:
				err = lifetimeCause()
			}
			s.mu.Lock()
			if err != nil {
				return fmt.Errorf("queue conversation turn events: %w", err)
			}
		}
		switch event.Type {
		case runner.ConversationEventTurnStarted:
			if event.ThreadID != "" {
				s.thread = event.ThreadID
			}
			s.turn = event.TurnID
		case runner.ConversationEventTurnCompleted:
			s.lastStatus = event.Status
		}
	}
	return nil
}

// boundQueuedLocked keeps the queued events under s.timings.withDefaults().MaxQueuedBytes
// while posting is slow or failing: adjacent deltas are merged first, then
// the oldest deltas are dropped. Turn, question, item and control events are
// never dropped. s.mu is held by the caller.
func (s *conversationSession) boundQueuedLocked() {
	limit := max(0, s.timings.withDefaults().MaxQueuedBytes-s.controlResults*s.controlResultBytes())
	if conversationQueuedBytes(s.events) <= limit {
		return
	}
	var dropped int
	s.events, dropped = trimConversationEvents(coalesceConversationDeltas(s.events), limit)
	if dropped > 0 {
		s.logger.Warn("conversation deltas dropped while the hub is slow", "dropped", dropped, "limit_bytes", limit)
	}
}

func conversationEventBytes(event runner.ConversationTurnEvent) int {
	return len(event.Text) + len(event.Summary) + len(event.Error) + len(event.Prompts)
}

func conversationQueuedBytes(events []runner.ConversationTurnEvent) int {
	total := 0
	for _, event := range events {
		total += conversationEventBytes(event)
	}
	return total
}

// trimConversationEvents drops the oldest deltas until the events fit limit,
// keeping order and every event that is not a delta. It reports how many
// deltas it dropped.
func trimConversationEvents(events []runner.ConversationTurnEvent, limit int) ([]runner.ConversationTurnEvent, int) {
	excess := conversationQueuedBytes(events) - limit
	if excess <= 0 {
		return events, 0
	}
	kept := make([]runner.ConversationTurnEvent, 0, len(events))
	dropped := 0
	for _, event := range events {
		if excess > 0 && event.Type == runner.ConversationEventDelta {
			excess -= conversationEventBytes(event)
			dropped++
			continue
		}
		kept = append(kept, event)
	}
	return kept, dropped
}

// Close stops polling, settles outstanding controls, flushes the remaining
// events and unbinds with the outcome, in that order. Every step is bounded
// by the caller's context: a hub that stopped answering cannot hold the run's
// teardown past the deadline the runner gave. It is idempotent.
func (s *conversationSession) Close(ctx context.Context, outcome string, runErr error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, conversationReportTimeout)
	defer cancel()
	var closeErr error
	s.closeOnce.Do(func() {
		// A report already in flight keeps its own bounded context, but no
		// longer than the caller allows.
		defer s.flushCancel()
		bounded := make(chan struct{})
		defer close(bounded)
		go func() {
			select {
			case <-ctx.Done():
				s.flushCancel()
			case <-bounded:
			}
		}()
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.cancel()
		close(s.stop)
		s.loops.Wait()
		close(s.closed)
		s.waiters.Wait()
		s.mu.Lock()
		s.releaseHeldLocked()
		s.mu.Unlock()
		s.drainCommands()
		close(s.commands)
		if err := s.flush(ctx); err != nil {
			closeErr = fmt.Errorf("flush conversation turn events: %w", err)
		}
		request := ConversationUnbindRequest{ConversationIdentity: s.identity, Outcome: outcome}
		if runErr != nil {
			request.Error = runErr.Error()
		}
		if err := s.client.UnbindConversation(ctx, s.item, request); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("unbind conversation: %w", err))
		}
	})
	return closeErr
}

// pollLoop long-polls controls after the last handled cursor, which
// acknowledges the previous page, and feeds each control into the queue.
func (s *conversationSession) pollLoop(ctx context.Context, queued []ConversationControl) {
	defer s.loops.Done()
	defer close(s.pollDone)
	for _, control := range queued {
		if !s.deliver(ctx, control) {
			return
		}
	}
	failures := 0
	suppressed := 0
	var lastWarn time.Time
	for ctx.Err() == nil {
		s.mu.Lock()
		after := s.cursor
		s.mu.Unlock()
		page, err := s.client.ConversationControls(ctx, s.id, s.identity, after, s.timings.withDefaults().PollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrStaleConversation) || errors.Is(err, runner.ErrNoConversation) || conversationPollRefused(err) {
				// Nothing about this attempt will change the answer: stop
				// polling and let the turn settle its controls unknown.
				s.logger.Warn("conversation control polling stopped", "error", err)
				return
			}
			failures++
			// One line a minute, not one per poll: a broken poll repeats
			// every backoff and would otherwise drown the runner log.
			if now := time.Now(); failures == 1 || now.Sub(lastWarn) >= conversationPollWarnInterval {
				s.logger.Warn("conversation control poll failed", "error", err, "failures", failures, "suppressed", suppressed)
				lastWarn = now
				suppressed = 0
			} else {
				suppressed++
			}
			if !sleepContext(ctx, s.timings.withDefaults().ReportBackoff*time.Duration(min(failures, 8))) {
				return
			}
			continue
		}
		failures = 0
		suppressed = 0
		lastWarn = time.Time{}
		for _, control := range page.Controls {
			if !s.deliver(ctx, control) {
				return
			}
		}
	}
}

// conversationPollRefused reports whether the hub refused the poll for who
// this runner is. Such a refusal is permanent for the attempt; a rate limit
// or a server error is not.
func conversationPollRefused(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden
}

// deliver converts one control, starts its waiter and queues it. It blocks
// while the queue is full and reports false once the session is stopping.
func (s *conversationSession) deliver(ctx context.Context, control ConversationControl) bool {
	if !s.reserveControlResult(ctx) {
		return false
	}
	command := runner.AgentControl{
		Kind: runner.AgentControlKind(control.Kind), MessageID: control.MessageID, Text: control.Text,
		RequestID: control.RequestID, Answers: control.Answers, Reply: make(chan error, 1),
		Attachments: s.fetchAttachments(ctx, control.Attachments),
	}
	s.mu.Lock()
	if s.closing || ctx.Err() != nil || s.lifetimeCause != nil && s.lifetimeCause() != nil {
		s.releaseControlResultLocked()
		s.mu.Unlock()
		return false
	}
	command.ThreadID = s.thread
	command.TurnID = control.Expected.TurnID
	if command.TurnID == "" {
		command.TurnID = s.turn
	}
	ended, turnWaiters := s.turnEnded, s.turnWaiters
	turnWaiters.Add(1)
	if control.Cursor > s.cursor {
		s.cursor = control.Cursor
	}
	s.mu.Unlock()
	command.Check = s.ownerCheck(control.Expected.AttemptID)
	s.waiters.Add(1)
	go s.await(control.Key, command, ended, turnWaiters)
	select {
	case s.commands <- command:
	case <-ctx.Done():
		return false
	}
	// The turn can end between reading the binding above and queueing the
	// control, so its drain runs before the control is there. Drain again:
	// polling is the only writer of the queue, so nothing else is lost, and a
	// control whose waiter already reported it unknown is never handed to the
	// next turn as well.
	s.mu.Lock()
	settled := s.turnEnded != ended
	s.mu.Unlock()
	if settled {
		s.drainCommands()
	}
	return true
}

func (s *conversationSession) controlResultBytes() int {
	return min(conversationControlErrorBytes, s.timings.withDefaults().MaxQueuedBytes)
}

func (s *conversationSession) reserveControlResult(ctx context.Context) bool {
	lifetimeDone := ctx.Done()
	lifetimeCause := func() error { return context.Cause(ctx) }
	if s.lifetimeCause != nil {
		lifetimeDone, lifetimeCause = s.lifetimeDone, s.lifetimeCause
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closing || ctx.Err() != nil || lifetimeCause() != nil {
			return false
		}
		if len(s.events)+s.controlResults < conversationMaxBatchEvents && conversationQueuedBytes(s.events)+(s.controlResults+1)*s.controlResultBytes() <= s.timings.withDefaults().MaxQueuedBytes {
			s.controlResults++
			return true
		}
		if s.reportRoom == nil {
			s.reportRoom = make(chan struct{})
		}
		room := s.reportRoom
		select {
		case s.kick <- struct{}{}:
		default:
		}
		s.mu.Unlock()
		interrupted := false
		select {
		case <-room:
		case <-s.stop:
			interrupted = true
		case <-ctx.Done():
			interrupted = true
		case <-lifetimeDone:
			interrupted = true
		}
		s.mu.Lock()
		if interrupted {
			return false
		}
	}
}

func (s *conversationSession) releaseControlResultLocked() {
	s.controlResults--
	if s.reportRoom != nil {
		close(s.reportRoom)
		s.reportRoom = nil
	}
}

// ownerCheck is the check a control runs immediately before it is written to
// the provider: the control must be meant for this attempt, when it names one,
// and the attempt's lease must still be current.
func (s *conversationSession) ownerCheck(expected string) func(context.Context) error {
	return func(ctx context.Context) error {
		if expected != "" && expected != s.identity.AttemptID {
			return fmt.Errorf("%w: control expects attempt %q, this attempt is %q", runner.ErrStaleConversationControl, expected, s.identity.AttemptID)
		}
		if s.leaseCheck != nil {
			return s.leaseCheck(ctx)
		}
		return nil
	}
}

// await turns a control's Reply into a control_result. A turn end or session
// close settles a control whose Reply never resolved as unknown.
func (s *conversationSession) await(key string, command runner.AgentControl, ended <-chan struct{}, turnWaiters *sync.WaitGroup) {
	defer s.waiters.Done()
	defer turnWaiters.Done()
	var err error
	reason := ""
	select {
	case err = <-command.Reply:
	case <-ended:
		select {
		case err = <-command.Reply:
		default:
			reason = "the turn ended before the control was consumed"
		}
	case <-s.closed:
		select {
		case err = <-command.Reply:
		default:
			reason = "the execution ended before the control was consumed"
		}
	}
	event := runner.ConversationTurnEvent{Type: runner.ConversationEventControlResult, Key: key, Status: runner.ConversationControlDelivered}
	switch {
	case reason != "":
		event.Status, event.Error = runner.ConversationControlUnknown, reason
	case err == nil:
	case errors.Is(err, runner.ErrStaleConversationControl), errors.Is(err, runner.ErrInvalidConversationControl), errors.Is(err, runner.ErrUnsupportedConversationControl):
		event.Status, event.Error = runner.ConversationControlRejected, err.Error()
	default:
		event.Status, event.Error = runner.ConversationControlUnknown, err.Error()
	}
	event.Error = conversationTruncateBytes(event.Error, s.controlResultBytes())
	s.mu.Lock()
	s.events = append(s.events, event)
	s.releaseControlResultLocked()
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// reportLoop flushes queued events after a short coalescing window. The
// poll context only ends the loop; a flush in flight finishes on the session's
// flush context so a close never cuts a posted batch short, and Close cancels
// that context when its own deadline passes.
func (s *conversationSession) reportLoop(ctx context.Context, reportCtx context.Context) {
	defer s.loops.Done()
	for {
		select {
		case <-s.kick:
		case <-s.stop:
			return
		}
		if !sleepContext(ctx, s.timings.withDefaults().FlushInterval) {
			return
		}
		if err := s.flush(reportCtx); err != nil {
			s.logger.Warn("conversation turn events dropped", "error", err)
		}
	}
}

// flush posts every queued event in order, coalescing adjacent deltas of one
// item and retrying transient failures. Events of a batch that still fails
// are dropped: the hub's unknown path covers the uncertainty.
func (s *conversationSession) flush(ctx context.Context) error {
	var firstErr error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(firstErr, err)
		}
		s.mu.Lock()
		events := coalesceConversationDeltas(s.events)
		if len(events) == 0 {
			s.mu.Unlock()
			return firstErr
		}
		count := min(len(events), conversationMaxBatchEvents)
		batch := events[:count]
		s.events = events[count:]
		if s.reportRoom != nil {
			close(s.reportRoom)
			s.reportRoom = nil
		}
		s.mu.Unlock()
		if err := s.post(ctx, batch); err != nil && firstErr == nil {
			firstErr = err
		}
	}
}

// post delivers one batch. A batch the hub refuses for its content is retried
// event by event so a single rejected event is dropped alone and the events
// around it keep their order.
//
// A question the hub never recorded can never be answered, so a dropped
// question_opened event is declined to the provider straight away rather
// than leaving the turn waiting for an answer nobody can give.
func (s *conversationSession) post(ctx context.Context, batch []runner.ConversationTurnEvent) error {
	declined := make(map[conversationQuestionIdentity]struct{})
	err := s.postBatch(ctx, batch, declined)
	if err == nil {
		return nil
	}
	if len(batch) < 2 || !conversationBatchRejected(err) {
		s.declineQuestions(batch, err, declined)
		return err
	}
	var errs []error
	for _, event := range batch {
		if err := s.postBatch(ctx, []runner.ConversationTurnEvent{event}, declined); err != nil {
			s.logger.Warn("conversation turn event dropped", "event_type", event.Type, "event_key", event.Key, "error", err)
			s.declineQuestions([]runner.ConversationTurnEvent{event}, err, declined)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

const conversationControlErrorBytes = 500

type conversationQuestionIdentity struct {
	requestID string
	threadID  string
	turnID    string
}

func conversationQuestionIdentityFor(event runner.ConversationTurnEvent) conversationQuestionIdentity {
	return conversationQuestionIdentity{requestID: event.RequestID, threadID: event.ThreadID, turnID: event.TurnID}
}

func (s *conversationSession) declineQuestions(events []runner.ConversationTurnEvent, cause error, declined map[conversationQuestionIdentity]struct{}) {
	for _, event := range events {
		if event.Type != runner.ConversationEventQuestionOpened || event.RequestID == "" {
			continue
		}
		identity := conversationQuestionIdentityFor(event)
		if _, exists := declined[identity]; exists {
			continue
		}
		reason := "Detent could not show this question to the user (" + conversationTruncateBytes(cause.Error(), conversationControlErrorBytes) + "). No answer will come; continue without one."
		var prompts []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(event.Prompts, &prompts); err != nil {
			prompts = nil
		}
		answers := make(map[string][]string, len(prompts))
		for _, prompt := range prompts {
			answers[prompt.ID] = []string{reason}
		}
		command := runner.AgentControl{
			Kind: runner.AgentControlAnswer, ThreadID: event.ThreadID, TurnID: event.TurnID, RequestID: event.RequestID,
			Answers: answers, Check: s.ownerCheck(""), Reply: make(chan error, 1),
		}
		s.mu.Lock()
		queued := false
		if !s.closing {
			select {
			case s.commands <- command:
				queued = true
			default:
			}
		}
		s.mu.Unlock()
		if queued {
			declined[identity] = struct{}{}
		}
		s.logger.Warn("conversation question declined to the provider", "request_id", event.RequestID, "queued", queued, "error", cause)
	}
}

func (s *conversationSession) declineQueuedQuestions(cause error, inFlight map[conversationQuestionIdentity]struct{}) {
	s.mu.Lock()
	var questions []runner.ConversationTurnEvent
	for _, events := range [][]runner.ConversationTurnEvent{s.events, s.held} {
		for _, event := range events {
			if event.Type == runner.ConversationEventQuestionOpened && event.RequestID != "" {
				questions = append(questions, event)
			}
		}
	}
	s.mu.Unlock()
	if len(questions) == 0 {
		return
	}
	declined := make(map[conversationQuestionIdentity]struct{}, len(inFlight)+len(questions))
	for identity := range inFlight {
		declined[identity] = struct{}{}
	}
	s.declineQuestions(questions, cause, declined)
	if len(declined) == 0 {
		return
	}
	remove := func(events []runner.ConversationTurnEvent) []runner.ConversationTurnEvent {
		kept := events[:0]
		for _, event := range events {
			if event.Type == runner.ConversationEventQuestionOpened {
				if _, ok := declined[conversationQuestionIdentityFor(event)]; ok {
					continue
				}
			}
			kept = append(kept, event)
		}
		clear(events[len(kept):])
		return kept
	}
	s.mu.Lock()
	s.events, s.held = remove(s.events), remove(s.held)
	if s.reportRoom != nil {
		close(s.reportRoom)
		s.reportRoom = nil
	}
	s.mu.Unlock()
}

// conversationTruncateBytes cuts value to at most limit bytes on a rune
// boundary.
func conversationTruncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// conversationBatchRejected reports whether the hub refused a batch for what
// it contained. A session the hub no longer owns is not a rejection: every
// event of the batch would fail the same way.
func conversationBatchRejected(err error) bool {
	if errors.Is(err, ErrStaleConversation) || errors.Is(err, runner.ErrNoConversation) || errors.Is(err, ErrUnavailable) {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return apiErr.Status >= 400 && apiErr.Status < 500
}

func (s *conversationSession) postBatch(ctx context.Context, batch []runner.ConversationTurnEvent, declined map[conversationQuestionIdentity]struct{}) error {
	if declined == nil {
		declined = make(map[conversationQuestionIdentity]struct{})
	}
	var lastErr error
	key := conversationBatchKey()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("report %d conversation turn events: %w", len(batch), errors.Join(lastErr, err))
		}
		if attempt > 0 && !sleepContext(ctx, s.timings.withDefaults().ReportBackoff<<uint(min(attempt-1, 3))) {
			return fmt.Errorf("report %d conversation turn events: %w", len(batch), errors.Join(lastErr, ctx.Err()))
		}
		postCtx, cancel := context.WithTimeout(ctx, conversationReportTimeout)
		_, err := s.client.ReportConversationTurnEvents(postCtx, s.id, ConversationTurnEventsRequest{ConversationIdentity: s.identity, BatchKey: key, Events: batch})
		cancel()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("attempt %d: %w", attempt+1, err)
		if !errors.Is(err, ErrUnavailable) {
			return fmt.Errorf("report %d conversation turn events: %w", len(batch), lastErr)
		}
		if attempt+1 >= conversationQuestionReportAttempts {
			s.declineQuestions(batch, lastErr, declined)
			s.declineQueuedQuestions(lastErr, declined)
		}
	}
}

// conversationBatchKey mints the idempotency key of one turn-events batch. A
// key the process cannot randomise still identifies the batch within the
// session, which is where a retry can duplicate.
func conversationBatchKey() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "batch-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(conversationBatchCounter.Add(1), 36)
	}
	return "batch-" + hex.EncodeToString(raw[:])
}

// conversationBatchCounter keeps the fallback key unique within the process.
var conversationBatchCounter atomic.Int64

// conversationMaxDeltaBytes is the hub's limit on one delta's text; merged
// deltas stay under it.
const conversationMaxDeltaBytes = 64 * 1024

// coalesceConversationDeltas merges adjacent deltas of the same provider
// item so a streamed message costs one event per flush.
func coalesceConversationDeltas(events []runner.ConversationTurnEvent) []runner.ConversationTurnEvent {
	merged := make([]runner.ConversationTurnEvent, 0, len(events))
	for _, event := range events {
		if event.Type == runner.ConversationEventDelta && len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.Type == runner.ConversationEventDelta && last.ProviderItemID == event.ProviderItemID && last.MessageID == event.MessageID && len(last.Text)+len(event.Text) <= conversationMaxDeltaBytes {
				last.Text += event.Text
				continue
			}
		}
		merged = append(merged, event)
	}
	return merged
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func NewConversationSession(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, identity ConversationIdentity, response ConversationBindResponse, leaseCheck func(context.Context) error, logger *slog.Logger, ready <-chan struct{}) runner.ConversationSession {
	return newConversationSession(ctx, client, item, identity, response, leaseCheck, logger, ready)
}

type SessionTimings struct {
	FlushInterval  time.Duration
	ReportBackoff  time.Duration
	PollWait       time.Duration
	MaxQueuedBytes int
}

func (t SessionTimings) withDefaults() SessionTimings {
	if t.FlushInterval <= 0 {
		t.FlushInterval = conversationFlushInterval
	}
	if t.ReportBackoff <= 0 {
		t.ReportBackoff = conversationReportBackoff
	}
	if t.PollWait <= 0 {
		t.PollWait = conversationPollWait
	}
	if t.MaxQueuedBytes <= 0 {
		t.MaxQueuedBytes = conversationMaxQueuedBytes
	}
	return t
}
