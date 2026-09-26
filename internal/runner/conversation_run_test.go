package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// conversationAgentBackend is a live backend that consumes one control from
// the turn's command queue, asks one question and streams a short turn.
type conversationAgentBackend struct {
	mu       sync.Mutex
	requests []AgentTurnRequest
	consumed []AgentControl
	inputErr error
	turnErr  error
}

func (*conversationAgentBackend) SupportsLiveControl() bool { return true }

func (b *conversationAgentBackend) RunTurn(_ context.Context, req AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()
	if err := onUpdate(AgentUpdate{Type: AgentUpdateTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}); err != nil {
		return AgentTurnResult{}, err
	}
	if req.ConversationControl != nil {
		select {
		case command := <-req.ConversationControl.Commands:
			b.mu.Lock()
			b.consumed = append(b.consumed, command)
			b.mu.Unlock()
			command.Reply <- nil
		case <-time.After(time.Second):
		}
		if req.ConversationControl.InputRequested != nil {
			b.inputErr = req.ConversationControl.InputRequested(AgentInputRequest{ID: "req-1", ThreadID: "thread-1", TurnID: "turn-1", Questions: json.RawMessage(`[{"id":"color","header":"Color","question":"Which color?","options":[{"label":"Blue","description":"calm"}],"isOther":true}]`)})
		}
	}
	for _, update := range []AgentUpdate{
		{Type: AgentUpdateMessageDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1", Delta: "Hello"},
		{Type: AgentUpdateMessageDelta, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1", Delta: " there"},
		{Type: AgentUpdateToolStarted, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "tool-1", Tool: "shell", Command: "go test ./...", Delta: "go test ./..."},
		{Type: AgentUpdateToolCompleted, ThreadID: "thread-1", TurnID: "turn-1", ItemID: "tool-1", Tool: "shell", Command: "go test ./...", Status: "completed", Delta: "ok"},
		{Type: AgentUpdateTurnCompleted, ThreadID: "thread-1", TurnID: "turn-1", Status: "completed"},
	} {
		if err := onUpdate(update); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-1"}, b.turnErr
}

// conversationTestExecution adds conversation binding to testExecution.
type conversationTestExecution struct {
	testExecution
	bindErr      error
	session      *fakeConversationSession
	capabilities ConversationCapabilities
	bound        bool
}

func (e *conversationTestExecution) BindConversation(_ context.Context, capabilities ConversationCapabilities) (ConversationSession, error) {
	e.bound = true
	e.capabilities = capabilities
	if e.bindErr != nil {
		return nil, e.bindErr
	}
	return e.session, nil
}

type fakeConversationSession struct {
	pendingAttachments []AgentAttachment
	mu                 sync.Mutex
	commands           chan AgentControl
	pending            string
	pendingKeys        []string
	resume             string
	coordinator        bool
	preferences        ConversationPreferences
	thread             string
	turn               string
	events             []ConversationTurnEvent
	lastStatus         string
	resumeMode         string
	controls           int
	finished           int
	closed             bool
	outcome            string
	closeErr           error
}

func newFakeConversationSession() *fakeConversationSession {
	return &fakeConversationSession{commands: make(chan AgentControl, 4)}
}

func (*fakeConversationSession) ConversationID() string              { return "conv_1" }
func (s *fakeConversationSession) ResumeThreadID() string            { return s.resume }
func (s *fakeConversationSession) PendingPrompt() (string, []string) { return s.pending, s.pendingKeys }

func (s *fakeConversationSession) PendingAttachments() []AgentAttachment { return s.pendingAttachments }
func (s *fakeConversationSession) Coordinator() bool                     { return s.coordinator }

func (s *fakeConversationSession) Preferences() ConversationPreferences { return s.preferences }

func (s *fakeConversationSession) ResumeMode() string { return s.resumeMode }

func (s *fakeConversationSession) LastTurnStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStatus
}

// PostStatus mirrors the hub client: the session addresses the status item to
// the turn it last saw start.
func (s *fakeConversationSession) PostStatus(ctx context.Context, data map[string]any, summary string) error {
	s.mu.Lock()
	event := ConversationTurnEvent{Type: ConversationEventItem, Kind: ConversationItemStatus, ThreadID: s.thread, TurnID: s.turn, Summary: summary, Data: data}
	s.mu.Unlock()
	return s.Report(ctx, []ConversationTurnEvent{event})
}

func (s *fakeConversationSession) Control(turn ConversationTurnHooks) *AgentConversationControl {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controls++
	return &AgentConversationControl{Commands: s.commands, InputRequested: turn.InputRequested, QuestionTimeout: turn.QuestionTimeout}
}

func (s *fakeConversationSession) FinishTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
}

func (s *fakeConversationSession) Report(_ context.Context, events []ConversationTurnEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	for _, event := range events {
		switch event.Type {
		case ConversationEventTurnStarted:
			s.thread, s.turn = event.ThreadID, event.TurnID
		case ConversationEventTurnCompleted:
			s.lastStatus = event.Status
		}
	}
	s.events = append(s.events, events...)
	return nil
}

func (s *fakeConversationSession) Close(_ context.Context, outcome string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.outcome = outcome
	s.closeErr = err
	return nil
}

func (s *fakeConversationSession) eventTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	types := make([]string, 0, len(s.events))
	for _, event := range s.events {
		label := event.Type
		if event.Key != "" {
			label += ":" + event.Key + ":" + event.Status
		}
		types = append(types, label)
	}
	return types
}

func newConversationRunner(t *testing.T, agent AgentBackend) *Runner {
	t.Helper()
	backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"}, Workspace: backend, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunWithoutConversationStillSucceeds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
	}{
		{"no conversation", ErrNoConversation},
		{"bind failure", errors.New("hub unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			agent := &conversationAgentBackend{}
			execution := &conversationTestExecution{bindErr: test.err}
			r := newConversationRunner(t, agent)
			result, err := r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
			if err != nil {
				t.Fatalf("run error = %v", err)
			}
			if result.FinalState != FinalStateCompleted || !execution.bound || execution.finish != "succeeded" {
				t.Fatalf("run without a conversation = %#v execution %#v", result, execution)
			}
			if len(agent.requests) != 1 || agent.requests[0].ConversationControl != nil {
				t.Fatalf("turn acquired a control without a session: %#v", agent.requests)
			}
		})
	}
}

func TestRunBindsConversationAndReportsTurn(t *testing.T) {
	t.Parallel()
	agent := &conversationAgentBackend{}
	session := newFakeConversationSession()
	session.pending = "Please also update the docs."
	session.pendingKeys = []string{"k1", "k2"}
	session.resume = "thread-resume"
	session.commands <- AgentControl{Kind: AgentControlMessage, ThreadID: "thread-1", TurnID: "turn-1", MessageID: "msg_1", Text: "steer", Check: func(context.Context) error { return nil }, Reply: make(chan error, 1)}
	execution := &conversationTestExecution{session: session}
	r := newConversationRunner(t, agent)
	result, err := r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
	if err != nil {
		t.Fatalf("run error = %v", err)
	}
	if result.FinalState != FinalStateCompleted {
		t.Fatalf("final state = %s", result.FinalState)
	}
	if !execution.capabilities.Steer || !execution.capabilities.Interrupt || !execution.capabilities.Answer {
		t.Fatalf("capabilities = %#v", execution.capabilities)
	}
	if len(agent.requests) != 1 {
		t.Fatalf("turns = %d", len(agent.requests))
	}
	request := agent.requests[0]
	if request.ConversationControl == nil || session.controls != 1 {
		t.Fatal("turn did not receive the conversation control")
	}
	if request.Resume.ThreadID != "thread-resume" {
		t.Fatalf("resume thread = %q, want the conversation thread", request.Resume.ThreadID)
	}
	// Instructions come first: the workflow prompt leads and the queued
	// messages follow it as a delimited data section.
	pending := strings.Index(request.Prompt, "Please also update the docs.")
	workflow := strings.Index(request.Prompt, "Complete the native issue")
	if workflow != 0 || pending < workflow {
		t.Fatalf("pending prompt was not appended after the instructions: %q", request.Prompt)
	}
	if len(agent.consumed) != 1 || agent.consumed[0].MessageID != "msg_1" {
		t.Fatalf("consumed controls = %#v", agent.consumed)
	}
	if agent.inputErr != nil {
		t.Fatalf("question hook = %v", agent.inputErr)
	}
	want := []string{
		"turn_started", "control_result:k1:delivered", "control_result:k2:delivered",
		"question_opened", "delta", "delta", "item", "item", "turn_completed",
	}
	if got := session.eventTypes(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("turn events = %v, want %v", got, want)
	}
	var question ConversationTurnEvent
	for _, event := range session.events {
		if event.Type == ConversationEventQuestionOpened {
			question = event
		}
	}
	var prompts []map[string]any
	if err := json.Unmarshal(question.Prompts, &prompts); err != nil {
		t.Fatalf("decode prompts: %v", err)
	}
	if question.RequestID != "req-1" || question.TurnID != "turn-1" || len(prompts) != 1 || prompts[0]["id"] != "color" || prompts[0]["free_text"] != true {
		t.Fatalf("question_opened = %#v prompts %#v", question, prompts)
	}
	events := session.events
	if events[0].ThreadID != "thread-1" || events[0].TurnID != "turn-1" {
		t.Fatalf("turn_started = %#v", events[0])
	}
	if delta := events[4]; delta.ProviderItemID != "item-1" || delta.Text != "Hello" {
		t.Fatalf("delta = %#v", delta)
	}
	if item := events[6]; item.Kind != ConversationItemTool || item.ProviderItemID != "tool-1" || !strings.Contains(item.Summary, "go test") {
		t.Fatalf("item = %#v", item)
	}
	if completed := events[8]; completed.TurnID != "turn-1" || completed.Status != ConversationTurnCompleted {
		t.Fatalf("turn_completed = %#v", completed)
	}
	if session.finished != 1 || !session.closed || session.outcome != ConversationOutcomeSucceeded || session.closeErr != nil {
		t.Fatalf("session lifecycle: finished %d closed %v outcome %q err %v", session.finished, session.closed, session.outcome, session.closeErr)
	}
	if execution.finish != "succeeded" {
		t.Fatalf("execution finish = %q", execution.finish)
	}
}

func TestConversationOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		ended      bool
		result     RunResult
		err        error
		lastStatus string
		want       string
	}{
		{name: "completed", result: RunResult{FinalState: FinalStateCompleted}, want: ConversationOutcomeSucceeded},
		{name: "failed state", result: RunResult{FinalState: FinalStateFailed}, want: ConversationOutcomeFailed},
		{name: "error", result: RunResult{FinalState: FinalStateCompleted}, err: errors.New("boom"), want: ConversationOutcomeFailed},
		{name: "operator stop", err: ErrOperatorStopped, want: ConversationOutcomeCancelled},
		{name: "context ended", ended: true, result: RunResult{FinalState: FinalStateCompleted}, want: ConversationOutcomeInterrupted},
		{name: "reported completed", result: RunResult{FinalState: FinalStateCompleted}, lastStatus: ConversationTurnCompleted, want: ConversationOutcomeSucceeded},
		{name: "reported interrupted", result: RunResult{FinalState: FinalStateCompleted}, lastStatus: ConversationTurnInterrupted, want: ConversationOutcomeInterrupted},
		{name: "reported failed", result: RunResult{FinalState: FinalStateCompleted}, lastStatus: ConversationTurnFailed, want: ConversationOutcomeFailed},
		{name: "reported status is mixed case", result: RunResult{FinalState: FinalStateCompleted}, lastStatus: " Interrupted ", want: ConversationOutcomeInterrupted},
		{name: "reported status never overrides a stop", err: ErrOperatorStopped, lastStatus: ConversationTurnFailed, want: ConversationOutcomeCancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if test.ended {
				ended, cancel := context.WithCancel(ctx)
				cancel()
				ctx = ended
			}
			if got := conversationOutcome(ctx, test.result, test.err, test.lastStatus); got != test.want {
				t.Fatalf("outcome = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConversationTurnStatus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		ended bool
		err   error
		want  string
	}{
		{name: "completed", want: ConversationTurnCompleted},
		{name: "failed", err: errors.New("boom"), want: ConversationTurnFailed},
		{name: "cancelled", ended: true, err: context.Canceled, want: ConversationTurnInterrupted},
		{name: "operator stop", err: ErrOperatorStopped, want: ConversationTurnInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if test.ended {
				ended, cancel := context.WithCancel(ctx)
				cancel()
				ctx = ended
			}
			if got := conversationTurnStatus(ctx, test.err); got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConversationPrompts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"codex question", `[{"id":"q1","header":"H","question":"Q?","options":[{"label":"A","description":"a"}],"isOther":true}]`, `[{"id":"q1","header":"H","question":"Q?","options":[{"label":"A","description":"a"}],"free_text":true}]`},
		{"missing id and options", `[{"question":"Free?"}]`, `[{"id":"q1","header":"","question":"Free?","options":[],"free_text":true}]`},
		{"empty", `[]`, `[{"id":"q1","header":"","question":"Input requested","options":[],"free_text":true}]`},
		{"invalid", `not json`, `[{"id":"q1","header":"","question":"Input requested","options":[],"free_text":true}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := conversationPrompts(json.RawMessage(test.raw))
			if string(got) != test.want {
				t.Fatalf("prompts = %s, want %s", got, test.want)
			}
		})
	}
}

func TestConversationItemSummary(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 3000)
	for _, test := range []struct {
		name   string
		update AgentUpdate
		want   string
		maxLen int
	}{
		{"started", AgentUpdate{Type: AgentUpdateToolStarted, Tool: "shell", Command: "go build ./..."}, "shell: go build ./...", 0},
		{"completed", AgentUpdate{Type: AgentUpdateToolCompleted, Tool: "shell", Command: "go build ./...", Status: "failed"}, "shell failed: go build ./...", 0},
		{"bounded", AgentUpdate{Type: AgentUpdateToolStarted, Tool: "shell", Command: long}, "", conversationSummaryLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := conversationItemSummary(test.update)
			if test.maxLen > 0 {
				if len(got) > test.maxLen {
					t.Fatalf("summary length %d exceeds %d", len(got), test.maxLen)
				}
				return
			}
			if got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
}

// conversationStatusBackend reports one turn whose completion status is
// scripted, and returns without an error: the provider considers the run
// finished even when the turn itself was interrupted or failed.
type conversationStatusBackend struct {
	mu       sync.Mutex
	status   string
	requests []AgentTurnRequest
}

func (*conversationStatusBackend) SupportsLiveControl() bool { return true }

func (b *conversationStatusBackend) RunTurn(_ context.Context, req AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()
	for _, update := range []AgentUpdate{
		{Type: AgentUpdateTurnStarted, ThreadID: "thread-s", TurnID: "turn-s"},
		{Type: AgentUpdateMessageDelta, ThreadID: "thread-s", TurnID: "turn-s", ItemID: "item-1", Delta: "Stopped."},
		{Type: AgentUpdateTurnCompleted, ThreadID: "thread-s", TurnID: "turn-s", Status: b.status},
	} {
		if err := onUpdate(update); err != nil {
			return AgentTurnResult{}, err
		}
	}
	return AgentTurnResult{ThreadID: "thread-s", TurnID: "turn-s"}, nil
}

// A turn the provider reported as interrupted or failed must reach the hub as
// that outcome even though the backend returned no error and the run itself
// completed.
func TestRunMapsReportedTurnStatusToOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		status      string
		wantOutcome string
		wantTurn    string
	}{
		{name: "completed", status: "completed", wantOutcome: ConversationOutcomeSucceeded, wantTurn: ConversationTurnCompleted},
		{name: "interrupted", status: "interrupted", wantOutcome: ConversationOutcomeInterrupted, wantTurn: ConversationTurnInterrupted},
		{name: "failed", status: "failed", wantOutcome: ConversationOutcomeFailed, wantTurn: ConversationTurnFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			agent := &conversationStatusBackend{status: test.status}
			session := newFakeConversationSession()
			execution := &conversationTestExecution{session: session}
			r := newConversationRunner(t, agent)
			result, err := r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan})
			if err != nil {
				t.Fatalf("run error = %v", err)
			}
			if result.FinalState != FinalStateCompleted {
				t.Fatalf("final state = %q, want the provider's own result", result.FinalState)
			}
			var completed ConversationTurnEvent
			for _, event := range session.events {
				if event.Type == ConversationEventTurnCompleted {
					completed = event
				}
			}
			if completed.Status != test.wantTurn {
				t.Fatalf("turn_completed = %#v, want status %q", completed, test.wantTurn)
			}
			if session.outcome != test.wantOutcome {
				t.Fatalf("unbind outcome = %q, want %q", session.outcome, test.wantOutcome)
			}
		})
	}
}

// syncBuffer collects log output from the run's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The bind log says how the conversation was resumed so an operator can see
// that a run continued from a transcript rather than the provider thread. It
// never carries conversation text.
func TestBindConversationLogsResumeMode(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mode   string
		resume string
	}{
		{name: "thread", mode: ConversationResumeThread, resume: "thread-resume"},
		{name: "transcript", mode: ConversationResumeTranscript},
		{name: "none", mode: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			logs := &syncBuffer{}
			agent := &conversationAgentBackend{}
			session := newFakeConversationSession()
			session.resumeMode, session.resume = test.mode, test.resume
			execution := &conversationTestExecution{session: session}
			backend := &retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "native", Branch: "native"}}}
			r, err := NewRunner(Dependencies{
				Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Complete the native issue"},
				Workspace:    backend,
				AgentBackend: agent,
				Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(t.Context(), RunRequest{Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#1"}, Mode: RunModePlan}); err != nil {
				t.Fatalf("run error = %v", err)
			}
			line := ""
			for _, entry := range strings.Split(logs.String(), "\n") {
				if strings.Contains(entry, "worker_conversation_bound") {
					line = entry
				}
			}
			if line == "" {
				t.Fatalf("no bind log line in %q", logs.String())
			}
			if !strings.Contains(line, `resume=`+strconv.Quote(test.mode)) && !strings.Contains(line, "resume="+test.mode) {
				t.Fatalf("bind log = %q, want resume %q", line, test.mode)
			}
		})
	}
}

// A turn started from queued messages carries their files: images as
// provider image input and text files as one delimited data block after the
// prompt (decisions section 17.1).
func TestConversationRunPrepareTurnCarriesPendingAttachments(t *testing.T) {
	t.Parallel()
	image := AgentAttachment{ID: "att_1", Name: "shot.png", MIME: "image/png", Size: 3, Content: []byte("png")}
	notes := AgentAttachment{ID: "att_2", Name: "notes.md", MIME: "text/markdown", Size: 9, Content: []byte("lease log")}
	cases := []struct {
		name        string
		pending     string
		attachments []AgentAttachment
		delivered   bool
		wantCount   int
		wantInBlock bool
	}{
		{name: "no attachments", pending: "Fix the lease"},
		{name: "an image is provider input only", pending: "Look", attachments: []AgentAttachment{image}, wantCount: 1},
		{name: "a text file is also a data block", pending: "Read", attachments: []AgentAttachment{notes}, wantCount: 1, wantInBlock: true},
		{name: "both", pending: "Both", attachments: []AgentAttachment{image, notes}, wantCount: 2, wantInBlock: true},
		{name: "already delivered carries nothing again", pending: "Again", attachments: []AgentAttachment{image, notes}, delivered: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			session := newFakeConversationSession()
			session.pending = test.pending
			session.pendingAttachments = test.attachments
			run := &conversationRun{session: session, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), pendingDelivered: test.delivered}
			request := run.prepareTurn(AgentTurnRequest{Prompt: "Do the work"})
			if len(request.Attachments) != test.wantCount {
				t.Fatalf("turn attachments = %+v, want %d", request.Attachments, test.wantCount)
			}
			if !strings.Contains(request.Prompt, "Do the work") {
				t.Fatalf("prompt lost the workflow instructions: %q", request.Prompt)
			}
			if got := strings.Contains(request.Prompt, "lease log"); got != test.wantInBlock {
				t.Fatalf("prompt carries the text file = %t, want %t: %q", got, test.wantInBlock, request.Prompt)
			}
			if strings.Contains(request.Prompt, "png") && !test.wantInBlock {
				t.Fatalf("an image must never be inlined as text: %q", request.Prompt)
			}
			if test.wantCount > 0 && strings.Index(request.Prompt, "Do the work") > strings.Index(request.Prompt, "<attachments>") && test.wantInBlock {
				t.Fatalf("the data block must follow the instructions: %q", request.Prompt)
			}
		})
	}
}
