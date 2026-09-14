package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
)

// scriptedCoordinatorBackend is the customer runner's provider for a
// coordinator turn: it records the turn request and the attached tools,
// optionally calls the coordinator tools through the run's own handler, and
// streams a short answer. An interruptible turn holds itself open until an
// interrupt control arrives and then ends the run as cancelled, which is what
// a provider that aborts a turn reports.
type scriptedCoordinatorBackend struct {
	thread        string
	turnID        string
	deltas        []string
	callTools     bool
	interruptible bool

	mu       sync.Mutex
	requests []runner.AgentTurnRequest
	tools    []runner.AgentTool
	results  map[string]runner.AgentToolResult
	controls []runner.AgentControl
}

const (
	scriptedProposalTitle     = "Split the parser"
	scriptedProposalObjective = "Extract a lexer so the parser can be tested on its own."
)

func newScriptedCoordinatorBackend(thread, turn string, deltas ...string) *scriptedCoordinatorBackend {
	return &scriptedCoordinatorBackend{thread: thread, turnID: turn, deltas: deltas}
}

func (*scriptedCoordinatorBackend) SupportsLiveControl() bool { return true }

func (b *scriptedCoordinatorBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return b.run(ctx, request, nil, onUpdate)
}

func (b *scriptedCoordinatorBackend) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handler runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.mu.Lock()
	b.tools = tools
	b.mu.Unlock()
	return b.run(ctx, request, handler, onUpdate)
}

func (b *scriptedCoordinatorBackend) run(ctx context.Context, request runner.AgentTurnRequest, handler runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.mu.Lock()
	b.requests = append(b.requests, request)
	b.mu.Unlock()
	if request.ConversationControl == nil {
		return runner.AgentTurnResult{}, errors.New("coordinator turn ran without a conversation control")
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: b.thread, TurnID: b.turnID}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if b.callTools && handler != nil {
		if err := b.useTools(ctx, handler); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	for _, delta := range b.deltas {
		update := runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ThreadID: b.thread, TurnID: b.turnID, ItemID: "item-1", Delta: delta}
		if err := onUpdate(update); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	if b.interruptible {
		if _, err := b.consume(ctx, request, runner.AgentControlInterrupt); err != nil {
			return runner.AgentTurnResult{}, err
		}
		update := runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: b.thread, TurnID: b.turnID, Status: "interrupted"}
		if err := onUpdate(update); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: b.thread, TurnID: b.turnID}, fmt.Errorf("provider aborted the turn: %w", context.Canceled)
	}
	update := runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: b.thread, TurnID: b.turnID, Status: "completed"}
	if err := onUpdate(update); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: b.thread, TurnID: b.turnID}, nil
}

// useTools calls the read tool and the proposal tool exactly as a provider
// would: through the handler the run attached to the turn.
func (b *scriptedCoordinatorBackend) useTools(ctx context.Context, handler runner.AgentToolHandler) error {
	proposal := fmt.Sprintf(`{"title":%q,"objective":%q}`, scriptedProposalTitle, scriptedProposalObjective)
	results := map[string]runner.AgentToolResult{}
	for _, call := range []runner.AgentToolCall{
		{Name: "list_attention", Arguments: json.RawMessage(`{}`)},
		{Name: "propose_issue", Arguments: json.RawMessage(proposal)},
	} {
		result, err := handler(ctx, call)
		if err != nil {
			return err
		}
		results[call.Name] = result
	}
	b.mu.Lock()
	b.results = results
	b.mu.Unlock()
	return nil
}

// consume waits for the next control of the wanted kind and settles it the
// way a real transport does: validate, re-check ownership, then reply.
func (b *scriptedCoordinatorBackend) consume(ctx context.Context, request runner.AgentTurnRequest, kind runner.AgentControlKind) (runner.AgentControl, error) {
	deadline := time.After(scriptedControlWait)
	for {
		select {
		case command, ok := <-request.ConversationControl.Commands:
			if !ok {
				return runner.AgentControl{}, errors.New("conversation control queue closed")
			}
			if err := command.Validate(); err != nil {
				command.Reply <- err
				return runner.AgentControl{}, err
			}
			err := command.Check(ctx)
			command.Reply <- err
			if err != nil {
				return runner.AgentControl{}, err
			}
			b.mu.Lock()
			b.controls = append(b.controls, command)
			b.mu.Unlock()
			if command.Kind == kind {
				return command, nil
			}
		case <-ctx.Done():
			return runner.AgentControl{}, ctx.Err()
		case <-deadline:
			return runner.AgentControl{}, fmt.Errorf("no %s control arrived within %s", kind, scriptedControlWait)
		}
	}
}

func (b *scriptedCoordinatorBackend) turnRequest(t *testing.T) runner.AgentTurnRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) != 1 {
		t.Fatalf("provider turns = %d, want exactly one per attempt", len(b.requests))
	}
	return b.requests[0]
}

func (b *scriptedCoordinatorBackend) toolNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.tools))
	for _, tool := range b.tools {
		names = append(names, tool.Name)
	}
	return names
}

func (b *scriptedCoordinatorBackend) toolResults() map[string]runner.AgentToolResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	results := make(map[string]runner.AgentToolResult, len(b.results))
	for name, result := range b.results {
		results[name] = result
	}
	return results
}

func (b *scriptedCoordinatorBackend) recordedControls() []runner.AgentControl {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runner.AgentControl(nil), b.controls...)
}

// coordinatorSlice carries what one scenario established for the next.
type coordinatorSlice struct {
	conversationID string
	// item is the coordinator work item the previous turn used, so a
	// follow-up can be proved to open a different one.
	item string
	// thread is the provider thread the first runner produced.
	thread string
}

// TestConversationCoordinatorIntegration proves the runner-dispatched
// coordinator end to end (decisions sections 1 and 9) against a real hub with
// no coordinator backend, a real database, real hub authorization, enrolled
// runner identities, the production claim path and the production runner in
// coordinator mode.
func TestConversationCoordinatorIntegration(t *testing.T) {
	f := newRunnerCoordinatorFixture(t)
	primary := f.enrollRunner(t, "primary")
	var slice coordinatorSlice
	t.Run("first message dispatches", func(t *testing.T) { slice = assertCoordinatorFirstTurn(t, f, primary) })
	t.Run("follow-up resumes the thread", func(t *testing.T) { assertCoordinatorFollowUp(t, f, primary, &slice) })
	t.Run("another runner gets a transcript", func(t *testing.T) { assertCoordinatorTranscriptHandover(t, f, &slice) })
	t.Run("cancel while running", func(t *testing.T) { assertCoordinatorCancel(t, f, primary) })
	t.Run("link closes the coordinator item", func(t *testing.T) { assertCoordinatorLink(t, f, primary) })
}

// assertCoordinatorFirstTurn walks the first message of a private chat all
// the way to a completed, runner-answered turn.
func assertCoordinatorFirstTurn(t *testing.T, f *conversationFixture, primary *conversationRunner) coordinatorSlice {
	created := f.createConversation(t, f.owner, "", "chat-1", "What is blocked right now?")
	id := created.Conversation.ID
	if created.Receipt == nil || created.Receipt.Status != conversation.DeliveryQueued {
		t.Fatalf("first message receipt = %#v, want queued for a runner", created.Receipt)
	}
	if created.Conversation.Execution.Status != conversation.ExecutionWaitingForRunner {
		t.Fatalf("execution = %#v, want waiting_for_runner", created.Conversation.Execution)
	}
	if created.Conversation.WorkItemID != nil || created.Conversation.Visibility != conversation.VisibilityPrivate {
		t.Fatalf("chat = %#v, want an unlinked private conversation", created.Conversation)
	}
	if created.Conversation.Execution.Capabilities != (conversation.Capabilities{}) {
		t.Fatalf("capabilities = %#v, want none while the item waits", created.Conversation.Execution.Capabilities)
	}
	stream := f.stream(t, f.owner, id, 0)

	// The message opened one coordinator work item, hidden from the ordinary
	// issue list and visible only when it is asked for.
	item := f.awaitCoordinatorItem(t, id)
	// The item names the conversation, never its content: private chat text
	// must not reach project readers through the issue list.
	if !strings.HasPrefix(item.Title, "Coordinator turn for conversation ") || !strings.HasSuffix(item.Title, id[len(id)-8:]) {
		t.Fatalf("coordinator item title = %q, want it to name the conversation %s only", item.Title, id)
	}
	if !strings.Contains(item.Body, id) {
		t.Fatalf("coordinator item body = %q, want it to name the conversation", item.Body)
	}
	if strings.Contains(item.Title, "What is blocked right now?") || strings.Contains(item.Body, "What is blocked right now?") {
		t.Fatalf("coordinator item carries the user's text: %#v", item)
	}
	if item.State != "Todo" || item.Terminal {
		t.Fatalf("coordinator item state = %q (terminal %t), want the first dispatchable state", item.State, item.Terminal)
	}
	for _, listed := range f.coordinatorItems(t, f.owner, false) {
		if string(listed.WorkItemID) == string(item.WorkItemID) {
			t.Fatalf("GET /work-items listed the coordinator item %s without include=coordinator", item.WorkItemID)
		}
	}

	// Only a runner that reports a fresh live-control backend may take it. A
	// legacy machine worker sees no claimable work at all, which is the
	// enrolment effect the bootstrap capability reports for the hosted shell.
	backend := newScriptedCoordinatorBackend("runner-thread-1", "coordinator-turn-1", "Two issues are in review. ", "Nothing is blocked. ")
	backend.callTools = true
	agent, git := newCoordinatorRunner(t, backend)
	plain := f.newWorkerScheduler(t, "machine-legacy")
	legacy, err := plain.FetchCandidateIssues(t.Context(), coordinatorSchedulingRequest(f, agent))
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 0 {
		t.Fatalf("a legacy machine worker claimed %#v, want the coordinator item withheld", legacy)
	}

	issue := primary.claim(t, agent, string(item.WorkItemID))
	if harnessDispatchMode(issue) != runner.RunModeCoordinator {
		t.Fatalf("claimed issue labels = %v, want the dispatcher to select the coordinator mode", issue.Labels)
	}
	done := primary.start(t, agent, issue)

	// The subscriber sees the whole start ladder and the streamed answer.
	frames := stream.await(t, "the execution to start and run", func(frames []sseFrame) bool {
		return containsExecutionStatus(t, frames, conversation.ExecutionStarting) && containsExecutionStatus(t, frames, conversation.ExecutionRunning)
	})
	if got := executionStatuses(t, frames); len(got) < 3 || got[0] != conversation.ExecutionWaitingForRunner ||
		got[1] != conversation.ExecutionStarting || got[2] != conversation.ExecutionRunning {
		t.Fatalf("execution statuses = %v, want waiting_for_runner then starting then running", got)
	}
	frames = stream.await(t, "the streamed coordinator answer", func(frames []sseFrame) bool {
		return strings.Contains(deltaText(frames), "Nothing is blocked.")
	})
	// The runner batches a turn's deltas, so the subscriber sees at least one
	// message.delta rather than one per provider delta.
	if countFrames(frames, conversation.EventMessageDelta) == 0 {
		t.Fatalf("the answer did not reach the subscriber as deltas: %s", describeFrames(frames))
	}
	running := f.awaitSnapshot(t, f.owner, id, "the queued message to be delivered", func(s wireSnapshot) bool {
		for _, message := range s.Messages {
			if message.CommandKey != nil && *message.CommandKey == "chat-1" {
				return message.Delivery == conversation.DeliveryDelivered
			}
		}
		return false
	})
	if running.Conversation.Execution.AttemptID == nil || *running.Conversation.Execution.AttemptID == "" {
		t.Fatalf("running execution has no attempt: %#v", running.Conversation.Execution)
	}
	// The scripted turn can finish and unbind before the snapshot above is
	// read, which clears the capabilities again, so assert them on the
	// execution frame that announced the running attempt.
	live := executionFrame(t, frames, conversation.ExecutionRunning)
	if !live.Capabilities.Steer || !live.Capabilities.Interrupt {
		t.Fatalf("capabilities = %#v, want the runner's live control set", live.Capabilities)
	}

	// The proposal the propose_issue tool posted is a status message with the
	// structured proposal card in its data.
	proposed := f.awaitSnapshot(t, f.owner, id, "the proposal status item", func(s wireSnapshot) bool {
		_, ok := findProposal(s.Messages)
		return ok
	})
	proposal, _ := findProposal(proposed.Messages)
	if proposal.Title != scriptedProposalTitle || proposal.Objective != scriptedProposalObjective {
		t.Fatalf("proposal = %#v", proposal)
	}
	// The proposal names the hub project the tools read, not the runner's
	// local workflow key: the client creates the issue with that id.
	if proposal.ProjectID != string(f.project.ID) {
		t.Fatalf("proposal project = %q, want the hub project %q", proposal.ProjectID, string(f.project.ID))
	}

	outcome := awaitOutcome(t, done)
	if outcome.err != nil {
		t.Fatalf("coordinator run error = %v", outcome.err)
	}
	if outcome.result.FinalState != runner.FinalStateCompleted {
		t.Fatalf("coordinator run final state = %q", outcome.result.FinalState)
	}
	primary.release(t, issue.ID, "completed")

	// The turn request is a coordinator turn: no git workspace, read only, no
	// deliverable, the coordinator instructions, the queued user text as a
	// follow-up and the three read-only tools.
	request := backend.turnRequest(t)
	if git.reachedGit() {
		t.Fatal("the git workspace backend was used; a coordinator run stays in its own temporary directory")
	}
	if !request.ReadOnly {
		t.Fatal("coordinator turn is not read-only")
	}
	if request.DeliverableKind != "" || request.DeliverableRepository != "" {
		t.Fatalf("coordinator turn carries a deliverable: %q %q", request.DeliverableKind, request.DeliverableRepository)
	}
	if request.Workspace == "" {
		t.Fatal("coordinator turn has no scratch directory")
	}
	if !strings.Contains(request.Prompt, "You are the Detent coordinator") {
		t.Fatalf("prompt is missing the coordinator instructions: %q", request.Prompt)
	}
	if !strings.Contains(request.ToolInstructions, "Detent coordinator answering a conversation") {
		t.Fatalf("tool instructions = %q", request.ToolInstructions)
	}
	follow := strings.Index(request.Prompt, "What is blocked right now?")
	instructions := strings.Index(request.Prompt, "You are the Detent coordinator")
	block := strings.Index(request.Prompt, "<pending-follow-ups>")
	// Instructions first, the queued message after them as delimited data.
	if instructions != 0 || block < instructions || follow < block {
		t.Fatalf("the queued message is not carried as a pending follow-up after the instructions: %q", request.Prompt)
	}
	if request.Resume.ThreadID != "" {
		t.Fatalf("first turn resume = %q, want no thread for a conversation that never had one", request.Resume.ThreadID)
	}
	if got := strings.Join(backend.toolNames(), ","); got != "list_attention,explain_issue,propose_issue" {
		t.Fatalf("attached tools = %q", got)
	}
	for name, result := range backend.toolResults() {
		if !result.Success {
			t.Fatalf("tool %s = %#v, want success", name, result)
		}
	}
	if attention := backend.toolResults()["list_attention"]; !strings.Contains(attention.Content, `"project_id":"`+string(f.project.ID)+`"`) {
		t.Fatalf("list_attention answered %q, want the hub project %q", attention.Content, f.project.ID)
	}

	// The turn ended: the execution is completed and the item is closed in a
	// terminal state, so the next message opens a new one.
	stream.await(t, "the completed execution", func(frames []sseFrame) bool {
		return containsExecutionStatus(t, frames, conversation.ExecutionCompleted)
	})
	completed := f.awaitSnapshot(t, f.owner, id, "the completed execution", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionCompleted
	})
	if completed.Conversation.WorkItemID != nil {
		t.Fatalf("the chat was linked to %v; a coordinator turn never links it", completed.Conversation.WorkItemID)
	}
	state, terminal := f.coordinatorItemState(t, string(item.WorkItemID))
	if !terminal {
		t.Fatalf("coordinator item state after the turn = %q, want a terminal state", state)
	}
	return coordinatorSlice{conversationID: id, item: string(item.WorkItemID), thread: backend.thread}
}

// assertCoordinatorFollowUp proves a follow-up opens a new item and that the
// same runner resumes its own provider thread instead of replaying history.
func assertCoordinatorFollowUp(t *testing.T, f *conversationFixture, primary *conversationRunner, slice *coordinatorSlice) {
	id := slice.conversationID
	stream := f.stream(t, f.owner, id, f.snapshot(t, f.owner, id).Cursor)
	receipt := f.command(t, f.owner, id, conversation.Command{Key: "chat-2", Kind: conversation.CommandMessage, Text: "And after the review?"})
	if receipt.Status != conversation.DeliveryQueued {
		t.Fatalf("follow-up receipt = %#v, want queued", receipt)
	}
	waiting := f.snapshot(t, f.owner, id)
	if waiting.Conversation.Execution.Status != conversation.ExecutionWaitingForRunner {
		t.Fatalf("execution after the follow-up = %#v, want waiting_for_runner", waiting.Conversation.Execution)
	}
	item := f.awaitCoordinatorItem(t, id)
	if string(item.WorkItemID) == slice.item {
		t.Fatalf("the follow-up reused the closed coordinator item %s", slice.item)
	}

	backend := newScriptedCoordinatorBackend(slice.thread, "coordinator-turn-2", "The review lands today. ")
	agent, git := newCoordinatorRunner(t, backend)
	issue := primary.claim(t, agent, string(item.WorkItemID))
	done := primary.start(t, agent, issue)
	stream.await(t, "the second coordinator answer", func(frames []sseFrame) bool {
		return strings.Contains(deltaText(frames), "The review lands today.")
	})
	outcome := awaitOutcome(t, done)
	if outcome.err != nil {
		t.Fatalf("second coordinator run error = %v", outcome.err)
	}
	primary.release(t, issue.ID, "completed")

	request := backend.turnRequest(t)
	if git.reachedGit() {
		t.Fatal("the git workspace backend was used for the second coordinator turn")
	}
	if request.Resume.ThreadID != slice.thread {
		t.Fatalf("resume thread = %q, want the thread this runner produced (%q)", request.Resume.ThreadID, slice.thread)
	}
	if strings.Contains(request.Prompt, "<transcript>") {
		t.Fatalf("a resuming runner was handed a transcript as well: %q", request.Prompt)
	}
	if !strings.Contains(request.Prompt, "And after the review?") {
		t.Fatalf("the follow-up never reached the prompt: %q", request.Prompt)
	}
	f.awaitSnapshot(t, f.owner, id, "the second completed execution", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionCompleted
	})
	slice.item = string(item.WorkItemID)
}

// assertCoordinatorTranscriptHandover proves a different runner is handed the
// bounded transcript rather than a thread it cannot resume.
func assertCoordinatorTranscriptHandover(t *testing.T, f *conversationFixture, slice *coordinatorSlice) {
	secondary := f.enrollRunner(t, "secondary")
	id := slice.conversationID
	stream := f.stream(t, f.owner, id, f.snapshot(t, f.owner, id).Cursor)
	if receipt := f.command(t, f.owner, id, conversation.Command{Key: "chat-3", Kind: conversation.CommandMessage, Text: "Who is reviewing it?"}); receipt.Status != conversation.DeliveryQueued {
		t.Fatalf("third message receipt = %#v, want queued", receipt)
	}
	item := f.awaitCoordinatorItem(t, id)
	if string(item.WorkItemID) == slice.item {
		t.Fatalf("the third message reused the closed coordinator item %s", slice.item)
	}

	backend := newScriptedCoordinatorBackend("runner-thread-2", "coordinator-turn-3", "Nobody is assigned yet. ")
	agent, _ := newCoordinatorRunner(t, backend)
	issue := secondary.claim(t, agent, string(item.WorkItemID))
	done := secondary.start(t, agent, issue)
	stream.await(t, "the third coordinator answer", func(frames []sseFrame) bool {
		return strings.Contains(deltaText(frames), "Nobody is assigned yet.")
	})
	outcome := awaitOutcome(t, done)
	if outcome.err != nil {
		t.Fatalf("third coordinator run error = %v", outcome.err)
	}
	secondary.release(t, issue.ID, "completed")

	request := backend.turnRequest(t)
	if request.Resume.ThreadID != "" {
		t.Fatalf("resume thread = %q, want none: the thread belongs to another runner", request.Resume.ThreadID)
	}
	transcript := strings.Index(request.Prompt, "<transcript>")
	closing := strings.Index(request.Prompt, "</transcript>")
	follow := strings.Index(request.Prompt, "Who is reviewing it?")
	if transcript < 0 || closing < transcript || follow < closing {
		t.Fatalf("the transcript block is missing or out of order: %q", request.Prompt)
	}
	block := request.Prompt[transcript:closing]
	for _, want := range []string{"[user] What is blocked right now?", "[assistant] Two issues are in review.", "[user] And after the review?"} {
		if !strings.Contains(block, want) {
			t.Fatalf("transcript block %q is missing %q", block, want)
		}
	}
	if strings.Contains(block, "Who is reviewing it?") {
		t.Fatalf("the pending control was replayed in the transcript as well: %q", block)
	}
}

// assertCoordinatorCancel proves a cancel on a live coordinator turn reaches
// the provider as an interrupt and ends the execution interrupted.
func assertCoordinatorCancel(t *testing.T, f *conversationFixture, primary *conversationRunner) {
	created := f.createConversation(t, f.owner, "", "cancel-1", "Summarize the release.")
	id := created.Conversation.ID
	stream := f.stream(t, f.owner, id, 0)
	item := f.awaitCoordinatorItem(t, id)

	backend := newScriptedCoordinatorBackend("runner-thread-cancel", "coordinator-turn-cancel", "Working on it. ")
	backend.interruptible = true
	agent, _ := newCoordinatorRunner(t, backend)
	issue := primary.claim(t, agent, string(item.WorkItemID))
	done := primary.start(t, agent, issue)

	stream.await(t, "the running turn", func(frames []sseFrame) bool {
		return strings.Contains(deltaText(frames), "Working on it.")
	})
	receipt := f.command(t, f.owner, id, conversation.Command{Key: "cancel-command", Kind: conversation.CommandCancel})
	if receipt.Status != conversation.DeliveryQueued || receipt.Kind != conversation.CommandCancel {
		t.Fatalf("cancel receipt = %#v, want a queued cancel", receipt)
	}
	stream.await(t, "the interrupting execution", func(frames []sseFrame) bool {
		return containsExecutionStatus(t, frames, conversation.ExecutionInterrupting)
	})

	outcome := awaitOutcome(t, done)
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("interrupted run error = %v, want the provider's cancellation", outcome.err)
	}
	primary.release(t, issue.ID, "cancelled")

	controls := backend.recordedControls()
	if len(controls) != 1 || controls[0].Kind != runner.AgentControlInterrupt {
		t.Fatalf("provider consumed %#v, want exactly one interrupt", controls)
	}
	frames := stream.await(t, "the interrupted execution", func(frames []sseFrame) bool {
		return containsExecutionStatus(t, frames, conversation.ExecutionInterrupted)
	})
	if got := executionStatuses(t, frames); len(got) == 0 || got[len(got)-1] != conversation.ExecutionInterrupted {
		t.Fatalf("execution statuses = %v, want interrupted last", got)
	}
	final := f.awaitSnapshot(t, f.owner, id, "the interrupted execution", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionInterrupted
	})
	assertDelivery(t, final.Messages, "cancel-command", conversation.DeliveryDelivered)
	state, terminal := f.coordinatorItemState(t, string(item.WorkItemID))
	if !terminal {
		t.Fatalf("coordinator item state after the interrupt = %q, want a terminal state", state)
	}
}

// assertCoordinatorLink proves linking a chat closes its open coordinator
// item and hands the conversation to the linked issue.
func assertCoordinatorLink(t *testing.T, f *conversationFixture, primary *conversationRunner) {
	created := f.createConversation(t, f.owner, "", "link-chat", "Let's rewrite the parser.")
	id := created.Conversation.ID
	item := f.awaitCoordinatorItem(t, id)
	if state, terminal := f.coordinatorItemState(t, string(item.WorkItemID)); terminal {
		t.Fatalf("the fresh coordinator item is already terminal in %q", state)
	}

	var link wireLinkResult
	request := map[string]any{"key": "link-coordinator", "share_history": true, "issue": map[string]any{"title": "Rewrite the parser"}}
	f.expect(t, f.owner, http.MethodPost, f.base+"/conversations/"+id+"/link", request, http.StatusOK, &link)
	if link.Conversation.WorkItem == nil || link.Conversation.WorkItem.ID != link.Issue.ID {
		t.Fatalf("linked conversation = %#v", link.Conversation)
	}
	state, terminal := f.coordinatorItemState(t, string(item.WorkItemID))
	if !terminal {
		t.Fatalf("coordinator item state after the link = %q, want a terminal state", state)
	}

	// The linked issue is what a run binds to now: it is the only claimable
	// work, and it is dispatched as ordinary work rather than a coordinator
	// turn.
	backend := newScriptedCoordinatorBackend("runner-thread-link", "linked-turn-1", "Reading the issue. ")
	agent, _ := newCoordinatorRunner(t, backend)
	issues := primary.candidates(t, agent)
	if len(issues) != 1 || issues[0].ID != link.Issue.ID {
		t.Fatalf("claimed %#v, want only the linked issue %s", issues, link.Issue.ID)
	}
	if harnessDispatchMode(issues[0]) == runner.RunModeCoordinator {
		t.Fatalf("the linked issue %s was dispatched as a coordinator turn", link.Issue.ID)
	}
	primary.release(t, issues[0].ID, "released")
}

// findProposal returns the proposal card a status message carries.
func findProposal(messages []wireMessage) (runner.CoordinatorProposal, bool) {
	for _, message := range messages {
		if message.Kind != conversation.MessageStatus || len(message.Data) == 0 {
			continue
		}
		var payload struct {
			Proposal *runner.CoordinatorProposal `json:"proposal"`
		}
		if err := json.Unmarshal(message.Data, &payload); err != nil || payload.Proposal == nil {
			continue
		}
		return *payload.Proposal, true
	}
	return runner.CoordinatorProposal{}, false
}
