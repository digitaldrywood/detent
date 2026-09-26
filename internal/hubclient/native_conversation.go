package hubclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// ErrStaleConversation reports that the hub no longer recognises this
// attempt as the owner of the conversation execution.
var ErrStaleConversation = errors.New("conversation execution is stale")

// Tunables shared by every conversation session. Tests shorten them.
var (
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
)

const (
	conversationCommandQueueSize = 64
	conversationMaxBatchEvents   = 500
	conversationReportAttempts   = 3
	conversationReportTimeout    = 15 * time.Second
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
	Coordinator    bool   `json:"coordinator"`
	// Preferences are the conversation's turn preferences: the runner
	// applies the explicit ones to every turn it runs for this attempt.
	Preferences runner.ConversationPreferences `json:"preferences"`
	Resume      struct {
		ThreadID string `json:"thread_id"`
		// ThreadOrigin names the kind of turn the thread belongs to, so a
		// runner can tell a coordinator thread from a worker one without
		// keeping a registry of its own (decisions section 9.3).
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
	// that streams it (decisions section 17.1).
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
	path, err := nativeItemPath(item)
	if err != nil {
		return response, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/conversation/bind", request, &response)
	return response, conversationError(err)
}

// UnbindConversation ends the attempt's binding with the run outcome.
func (c *NativeClient) UnbindConversation(ctx context.Context, item tracker.NativeWorkItemID, request ConversationUnbindRequest) error {
	path, err := nativeItemPath(item)
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
		return fmt.Errorf("%w: %w", ErrStaleConversation, err)
	default:
		return err
	}
}

// BindConversation implements runner.ConversationExecution for the native
// execution: the attempt binds with its own lease and fencing token.
func (e *nativeExecution) BindConversation(ctx context.Context, capabilities runner.ConversationCapabilities) (runner.ConversationSession, error) {
	if e.claim.source == nil || e.claim.source.client == nil || e.data.AttemptID == "" {
		return nil, runner.ErrNoConversation
	}
	identity := ConversationIdentity{LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken, AttemptID: e.data.AttemptID}
	response, err := e.claim.source.client.BindConversation(ctx, e.claim.lease.WorkItemID, ConversationBindRequest{ConversationIdentity: identity, RunID: e.data.RunID, Capabilities: capabilities})
	if err != nil {
		return nil, err
	}
	logger := slog.Default().With("work_item", e.claim.lease.WorkItemID, "attempt", e.data.AttemptID)
	return newConversationSession(ctx, e.claim.source.client, e.claim.lease.WorkItemID, identity, response, e.leaseCheck, logger), nil
}

// leaseCheck reports whether the attempt's lease is still current locally.
func (e *nativeExecution) leaseCheck(context.Context) error {
	if e.remaining() <= 0 {
		return fmt.Errorf("%w: %w", runner.ErrStaleConversationControl, runner.ErrExecutionAuthorityUnavailable)
	}
	return nil
}

// conversationSession is one attempt's binding. A poll goroutine feeds the
// command queue and acknowledges controls by cursor; a report goroutine
// batches turn events; a waiter per control turns its Reply into a
// control_result.
type conversationSession struct {
	client       *NativeClient
	item         tracker.NativeWorkItemID
	id           string
	identity     ConversationIdentity
	resume       string
	resumeMode   string
	threadOrigin string
	coordinator  bool
	preferences  runner.ConversationPreferences
	pending      string
	pendingKey   []string
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

	mu         sync.Mutex
	thread     string
	turn       string
	turnEnded  chan struct{}
	cursor     int64
	events     []runner.ConversationTurnEvent
	lastStatus string
	closing    bool
	closeOnce  sync.Once
}

func newConversationSession(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, identity ConversationIdentity, response ConversationBindResponse, leaseCheck func(context.Context) error, logger *slog.Logger) *conversationSession {
	pollCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	flushCtx, flushCancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &conversationSession{
		client: client, item: item, id: response.ConversationID, identity: identity, resume: response.Resume.ThreadID,
		resumeMode:   conversationResumeMode(response),
		threadOrigin: response.Resume.ThreadOrigin,
		coordinator:  response.Coordinator,
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
	go s.pollLoop(pollCtx, queued)
	go s.reportLoop(pollCtx, flushCtx)
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

func (s *conversationSession) ResumeThreadID() string { return s.resume }

// ThreadOrigin reports the kind of turn the bind's thread belongs to:
// "coordinator" or "worker" (decisions section 9.3). It is empty when the hub
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

// Coordinator reports whether the hub bound a coordinator work item.
func (s *conversationSession) Coordinator() bool { return s.coordinator }

// Preferences are the conversation's turn preferences as the bind reported
// them. They are fixed for the life of the attempt: a change mid-attempt
// reaches the next turn through the next bind.
func (s *conversationSession) Preferences() runner.ConversationPreferences { return s.preferences }

// PostStatus queues a structured status item addressed to the live turn.
func (s *conversationSession) PostStatus(ctx context.Context, data map[string]any, summary string) error {
	s.mu.Lock()
	event := runner.ConversationTurnEvent{
		Type: runner.ConversationEventItem, Kind: runner.ConversationItemStatus,
		ThreadID: s.thread, TurnID: s.turn, Summary: summary, Data: data,
	}
	s.mu.Unlock()
	return s.Report(ctx, []runner.ConversationTurnEvent{event})
}

func (s *conversationSession) PendingPrompt() (string, []string) {
	return s.pending, append([]string(nil), s.pendingKey...)
}

func (s *conversationSession) Control(turn runner.ConversationTurnHooks) *runner.AgentConversationControl {
	return &runner.AgentConversationControl{Commands: s.commands, InputRequested: turn.InputRequested, QuestionTimeout: turn.QuestionTimeout}
}

// FinishTurn ends the current turn: waiters of unconsumed controls report
// unknown and the queue is drained so a later turn never sees stale controls.
func (s *conversationSession) FinishTurn() {
	s.mu.Lock()
	ended := s.turnEnded
	s.turnEnded = make(chan struct{})
	s.turn = ""
	s.mu.Unlock()
	close(ended)
	s.drainCommands()
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
func (s *conversationSession) Report(_ context.Context, events []runner.ConversationTurnEvent) error {
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return fmt.Errorf("report conversation turn events: %w", ErrStaleConversation)
	}
	for _, event := range events {
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
	s.events = append(s.events, events...)
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return nil
}

// Close stops polling, settles outstanding controls, flushes the remaining
// events and unbinds with the outcome, in that order. Every step is bounded
// by the caller's context: a hub that stopped answering cannot hold the run's
// teardown past the deadline the runner gave. It is idempotent.
func (s *conversationSession) Close(ctx context.Context, outcome string, runErr error) error {
	if ctx == nil {
		ctx = context.Background()
	}
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
		s.cancel()
		close(s.stop)
		s.loops.Wait()
		close(s.closed)
		s.waiters.Wait()
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.drainCommands()
		close(s.commands)
		if err := s.flush(ctx); err != nil {
			s.logger.Warn("conversation turn events dropped at close", "error", err)
		}
		request := ConversationUnbindRequest{ConversationIdentity: s.identity, Outcome: outcome}
		if runErr != nil {
			request.Error = runErr.Error()
		}
		if err := s.client.UnbindConversation(ctx, s.item, request); err != nil {
			closeErr = fmt.Errorf("unbind conversation: %w", err)
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
		page, err := s.client.ConversationControls(ctx, s.id, s.identity, after, conversationPollWait)
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
			if !sleepContext(ctx, conversationReportBackoff*time.Duration(min(failures, 8))) {
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
	command := runner.AgentControl{
		Kind: runner.AgentControlKind(control.Kind), MessageID: control.MessageID, Text: control.Text,
		RequestID: control.RequestID, Answers: control.Answers, Reply: make(chan error, 1),
		Attachments: s.fetchAttachments(ctx, control.Attachments),
	}
	s.mu.Lock()
	command.ThreadID = s.thread
	command.TurnID = control.Expected.TurnID
	if command.TurnID == "" {
		command.TurnID = s.turn
	}
	ended := s.turnEnded
	if control.Cursor > s.cursor {
		s.cursor = control.Cursor
	}
	s.mu.Unlock()
	expected := control.Expected.AttemptID
	command.Check = func(ctx context.Context) error {
		if expected != "" && expected != s.identity.AttemptID {
			return fmt.Errorf("%w: control expects attempt %q, this attempt is %q", runner.ErrStaleConversationControl, expected, s.identity.AttemptID)
		}
		if s.leaseCheck != nil {
			return s.leaseCheck(ctx)
		}
		return nil
	}
	s.waiters.Add(1)
	go s.await(control.Key, command, ended)
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

// await turns a control's Reply into a control_result. A turn end or session
// close settles a control whose Reply never resolved as unknown.
func (s *conversationSession) await(key string, command runner.AgentControl, ended <-chan struct{}) {
	defer s.waiters.Done()
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
	s.mu.Lock()
	s.events = append(s.events, event)
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
		if !sleepContext(ctx, conversationFlushInterval) {
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
	s.mu.Lock()
	events := coalesceConversationDeltas(s.events)
	s.events = nil
	s.mu.Unlock()
	var errs []error
	for len(events) > 0 {
		batch := events[:min(len(events), conversationMaxBatchEvents)]
		events = events[len(batch):]
		if err := s.post(ctx, batch); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// post delivers one batch. A batch the hub refuses for its content is retried
// event by event so a single rejected event is dropped alone and the events
// around it keep their order.
func (s *conversationSession) post(ctx context.Context, batch []runner.ConversationTurnEvent) error {
	err := s.postBatch(ctx, batch)
	if err == nil || len(batch) < 2 || !conversationBatchRejected(err) {
		return err
	}
	var errs []error
	for _, event := range batch {
		if err := s.postBatch(ctx, []runner.ConversationTurnEvent{event}); err != nil {
			s.logger.Warn("conversation turn event dropped", "event_type", event.Type, "event_key", event.Key, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// conversationBatchRejected reports whether the hub refused a batch for what
// it contained. A session the hub no longer owns is not a rejection: every
// event of the batch would fail the same way.
func conversationBatchRejected(err error) bool {
	if errors.Is(err, ErrStaleConversation) || errors.Is(err, runner.ErrNoConversation) {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return apiErr.Status >= 400 && apiErr.Status < 500
}

func (s *conversationSession) postBatch(ctx context.Context, batch []runner.ConversationTurnEvent) error {
	var errs []error
	// One key for the batch, repeated by every attempt: a retry of a batch the
	// hub already applied is a duplicate it can drop.
	key := conversationBatchKey()
	for attempt := range conversationReportAttempts {
		if attempt > 0 && !sleepContext(ctx, conversationReportBackoff<<uint(attempt-1)) {
			break
		}
		postCtx, cancel := context.WithTimeout(ctx, conversationReportTimeout)
		_, err := s.client.ReportConversationTurnEvents(postCtx, s.id, ConversationTurnEventsRequest{ConversationIdentity: s.identity, BatchKey: key, Events: batch})
		cancel()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("attempt %d: %w", attempt+1, err))
		if !errors.Is(err, ErrUnavailable) {
			break
		}
	}
	return fmt.Errorf("report %d conversation turn events: %w", len(batch), errors.Join(errs...))
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

// coalesceConversationDeltas merges adjacent deltas of the same provider
// item so a streamed message costs one event per flush.
func coalesceConversationDeltas(events []runner.ConversationTurnEvent) []runner.ConversationTurnEvent {
	merged := make([]runner.ConversationTurnEvent, 0, len(events))
	for _, event := range events {
		if event.Type == runner.ConversationEventDelta && len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.Type == runner.ConversationEventDelta && last.ProviderItemID == event.ProviderItemID && last.MessageID == event.MessageID {
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
