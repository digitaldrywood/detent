package hubclient

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
)

// sliceResult carries the identities the vertical slice established so the
// later scenarios can assert against the same conversation.
type sliceResult struct {
	conversationID string
	// midCursor is the event sequence right after the first attempt, the
	// cursor a reloading client would reconnect from.
	midCursor int64
	// frames is everything the live subscriber saw, so the replay can be
	// compared against it frame by frame.
	frames []sseFrame
}

// TestConversationIntegration proves the production vertical slice end to
// end against a real hub service, a real database, real hub authorization,
// the production worker client and scheduler, and the production runner
// turn path with a scripted live-control backend.
func TestConversationIntegration(t *testing.T) {
	f := newConversationFixture(t)
	var slice sliceResult
	t.Run("vertical slice", func(t *testing.T) { slice = runConversationVerticalSlice(t, f) })
	t.Run("reload replay", func(t *testing.T) { assertConversationReload(t, f, slice) })
	t.Run("isolation", func(t *testing.T) { assertConversationIsolation(t, f, slice) })
	t.Run("link double submit", func(t *testing.T) { assertConversationLinkDoubleSubmit(t, f) })
}

// runConversationVerticalSlice walks new chat → discussion → linked issue →
// runner claim → streamed work → question → authorized answer → steer →
// completion → continuation → second attempt.
func runConversationVerticalSlice(t *testing.T, f *conversationFixture) sliceResult {
	created := f.createConversation(t, f.owner, "", "chat-first", "Let's rewrite the parser.")
	id := created.Conversation.ID
	if created.Conversation.Visibility != conversation.VisibilityPrivate || created.Conversation.Status != conversation.StatusActive {
		t.Fatalf("new chat = %#v, want a private active conversation", created.Conversation)
	}
	if created.Conversation.Execution.Status != conversation.ExecutionIdle || created.Conversation.WorkItemID != nil {
		t.Fatalf("new chat execution = %#v, work item = %v", created.Conversation.Execution, created.Conversation.WorkItemID)
	}
	if created.Receipt == nil || created.Receipt.Status != conversation.DeliverySaved || created.Receipt.Key != "chat-first" {
		t.Fatalf("first message receipt = %#v", created.Receipt)
	}
	if created.Conversation.Owner.PrincipalID != f.ownerID {
		t.Fatalf("owner = %#v, want the creating principal %s", created.Conversation.Owner, f.ownerID)
	}
	if created.Conversation.Title != "Let's rewrite the parser." {
		t.Fatalf("derived title = %q", created.Conversation.Title)
	}

	stream := f.stream(t, f.owner, id, 0)

	// The coordinator answers the unlinked chat: accepted, streamed deltas,
	// a completed assistant message and an execution that returns to idle.
	frames := stream.await(t, "the first coordinator reply", func(frames []sseFrame) bool {
		return conversationSettled(t, frames, "Let's rewrite the parser.")
	})
	if countFrames(frames, conversation.EventMessageDelta) == 0 {
		t.Fatalf("coordinator reply did not stream deltas: %s", describeFrames(frames))
	}
	if got := executionStatuses(t, frames); len(got) < 2 || got[0] != conversation.ExecutionRunning || got[len(got)-1] != conversation.ExecutionIdle {
		t.Fatalf("coordinator execution statuses = %v, want running then idle", got)
	}

	// A second discussion turn, so the linked history is more than one
	// exchange and the transcript is what the handoff shares.
	second := f.command(t, f.owner, id, conversation.Command{Key: "chat-second", Kind: conversation.CommandMessage, Text: "Split it into a lexer and a parser."})
	if second.Status != conversation.DeliverySaved {
		t.Fatalf("second message receipt = %#v", second)
	}
	stream.await(t, "the second coordinator reply", func(frames []sseFrame) bool {
		return conversationSettled(t, frames, "Split it into a lexer and a parser.")
	})
	// The first turn carries the transcript; the second resumes the
	// provider thread instead of replaying history.
	prompts := f.coordinator.recordedPrompts()
	if len(prompts) != 2 || prompts[0] != "Let's rewrite the parser." || prompts[1] != "Split it into a lexer and a parser." {
		t.Fatalf("coordinator prompts = %#v", prompts)
	}

	// Before the link the chat is private: another project member cannot
	// see it at all, and the failure is opaque.
	f.failure(t, f.member, http.MethodGet, f.base+"/conversations/"+id, nil, http.StatusNotFound)

	// The handoff. share_history is mandatory, and the link answers with
	// the created issue and its lane.
	if refused := f.failure(t, f.owner, http.MethodPost, f.base+"/conversations/"+id+"/link", map[string]any{"key": "link-refused", "issue": map[string]any{"title": "Rewrite the parser"}}, http.StatusUnprocessableEntity); refused.Code != "share_history_required" {
		t.Fatalf("link without sharing = %#v", refused)
	}
	linkRequest := map[string]any{
		"key": "link-1", "share_history": true,
		"issue": map[string]any{"title": "Rewrite the parser", "description": "Split the parser into a lexer and a parser."},
	}
	var link wireLinkResult
	f.expect(t, f.owner, http.MethodPost, f.base+"/conversations/"+id+"/link", linkRequest, http.StatusOK, &link)
	if link.Issue.ID == "" || link.Issue.Identifier != "product#1" {
		t.Fatalf("linked issue = %#v", link.Issue)
	}
	if link.Scheduling.Lane != "Todo" || link.Scheduling.RunnerBound {
		t.Fatalf("scheduling = %#v, want the Todo lane with no runner yet", link.Scheduling)
	}
	if link.Conversation.Visibility != conversation.VisibilityShared || link.Conversation.WorkItem == nil || link.Conversation.WorkItem.ID != link.Issue.ID {
		t.Fatalf("linked conversation = %#v", link.Conversation)
	}
	issueID := link.Issue.ID

	snapshot := f.snapshot(t, f.owner, id)
	if snapshot.Conversation.Execution.Status != conversation.ExecutionWaitingForRunner {
		t.Fatalf("execution after link = %#v, want waiting_for_runner", snapshot.Conversation.Execution)
	}
	if snapshot.Conversation.WorkItem == nil || snapshot.Conversation.WorkItem.Lane != "Todo" || snapshot.Conversation.WorkItem.RunnerBound {
		t.Fatalf("work item after link = %#v", snapshot.Conversation.WorkItem)
	}
	status := findMessage(t, snapshot.Messages, func(message wireMessage) bool {
		return message.Role == conversation.RoleSystem && message.Kind == conversation.MessageStatus
	})
	var issueResult struct {
		Issue struct {
			ID          string `json:"id"`
			Identifier  string `json:"identifier"`
			Lane        string `json:"lane"`
			RunnerBound bool   `json:"runner_bound"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(status.Data, &issueResult); err != nil {
		t.Fatalf("decode issue result message: %v: %s", err, status.Data)
	}
	if issueResult.Issue.ID != issueID || issueResult.Issue.Identifier != "product#1" || issueResult.Issue.Lane != "Todo" {
		t.Fatalf("issue result message = %#v", issueResult.Issue)
	}

	// The other project member reads the shared history now.
	shared := f.snapshot(t, f.member, id)
	if len(shared.Messages) != len(snapshot.Messages) {
		t.Fatalf("shared history = %d messages, owner sees %d", len(shared.Messages), len(snapshot.Messages))
	}

	// A follow-up sent before any runner exists is queued, not lost.
	followUp := f.command(t, f.owner, id, conversation.Command{Key: "follow-up", Kind: conversation.CommandMessage, Text: "Keep the public API stable."})
	if followUp.Status != conversation.DeliveryQueued {
		t.Fatalf("follow-up receipt = %#v, want queued", followUp)
	}

	// The worker claims the linked issue through the production scheduler
	// and runs it through the production runner turn path.
	backend := &scriptedLiveBackend{}
	scheduler := f.newWorkerScheduler(t, "machine-conversation")
	issue := f.claimIssue(t, scheduler, issueID)
	firstRun := f.startAttempt(t, scheduler, backend, issue)

	// The subscriber sees the whole start ladder, not just the end state.
	stream.await(t, "the execution to start and run", func(frames []sseFrame) bool {
		return containsExecutionStatus(t, frames, conversation.ExecutionStarting) && containsExecutionStatus(t, frames, conversation.ExecutionRunning)
	})
	running := f.awaitSnapshot(t, f.owner, id, "the attempt to start", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionRunning || s.Conversation.Execution.Status == conversation.ExecutionWaitingInput
	})
	if running.Conversation.Execution.AttemptID == nil || *running.Conversation.Execution.AttemptID == "" {
		t.Fatalf("running execution has no attempt: %#v", running.Conversation.Execution)
	}
	firstAttempt := *running.Conversation.Execution.AttemptID
	if !running.Conversation.Execution.Capabilities.Steer || !running.Conversation.Execution.Capabilities.Answer {
		t.Fatalf("capabilities = %#v, want the runner's live control set", running.Conversation.Execution.Capabilities)
	}
	if running.Conversation.WorkItem == nil || !running.Conversation.WorkItem.RunnerBound {
		t.Fatalf("work item = %#v, want runner_bound once an attempt owns the issue", running.Conversation.WorkItem)
	}

	// The question reaches the subscriber, and the execution says it waits.
	frames = stream.await(t, "the question to open", func(frames []sseFrame) bool {
		return countFrames(frames, conversation.EventQuestionOpened) == 1
	})
	var opened wireQuestion
	lastFrame(t, frames, conversation.EventQuestionOpened, &opened)
	if opened.Status != conversation.QuestionPending || len(opened.Prompts) != 1 || len(opened.Prompts[0].Options) != 2 {
		t.Fatalf("question = %#v", opened)
	}
	if opened.Owner.AttemptID != firstAttempt {
		t.Fatalf("question owner = %#v, want attempt %s", opened.Owner, firstAttempt)
	}
	waiting := f.awaitSnapshot(t, f.owner, id, "waiting_input", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionWaitingInput
	})
	if len(waiting.Questions) != 1 || waiting.Questions[0].ID != opened.ID {
		t.Fatalf("snapshot questions = %#v, want the pending question", waiting.Questions)
	}
	// The queued follow-up was handed to the attempt and delivered as part
	// of the turn prompt.
	deliveredFollowUp := findMessage(t, waiting.Messages, func(message wireMessage) bool {
		return message.CommandKey != nil && *message.CommandKey == "follow-up"
	})
	if deliveredFollowUp.Delivery != conversation.DeliveryDelivered {
		t.Fatalf("queued follow-up delivery = %q, want delivered", deliveredFollowUp.Delivery)
	}

	// A different project member answers the question. Answering is a write
	// on a shared conversation, not an escalation.
	answer := f.command(t, f.member, id, conversation.Command{
		Key: "answer-1", Kind: conversation.CommandAnswer, QuestionID: opened.ID,
		Answers: map[string][]string{"approach": {"Incremental"}},
	})
	if answer.Status != conversation.DeliveryQueued || answer.QuestionID != opened.ID {
		t.Fatalf("answer receipt = %#v", answer)
	}
	// The question moves to answered and the turn keeps streaming. The
	// control result and the deltas are separate batches, so both are
	// awaited rather than assumed to arrive together.
	frames = stream.await(t, "the answered question and the deltas that follow", func(frames []sseFrame) bool {
		if !strings.Contains(deltaText(frames), "Taking the Incremental approach.") {
			return false
		}
		var question wireQuestion
		for _, frame := range frames {
			if frame.Event != string(conversation.EventQuestionUpdated) {
				continue
			}
			if err := json.Unmarshal([]byte(frame.Data), &question); err == nil && question.Status == conversation.QuestionAnswered {
				return true
			}
		}
		return false
	})
	var answered wireQuestion
	lastFrame(t, frames, conversation.EventQuestionUpdated, &answered)
	if answered.Status != conversation.QuestionAnswered || answered.AnsweredBy == nil || *answered.AnsweredBy != f.memberID {
		t.Fatalf("answered question = %#v, want answered by the member principal %s", answered, f.memberID)
	}

	// Steering the live turn: the message reaches the provider and its echo
	// comes back as a delta.
	steer := f.command(t, f.owner, id, conversation.Command{Key: "steer-1", Kind: conversation.CommandMessage, Text: "Also update the changelog."})
	if steer.Status != conversation.DeliveryQueued {
		t.Fatalf("steer receipt = %#v, want queued before hand-off", steer)
	}
	stream.await(t, "the steered delta", func(frames []sseFrame) bool {
		return strings.Contains(deltaText(frames), "Noted: Also update the changelog.")
	})

	result := awaitAttempt(t, firstRun)
	if result.FinalState != runner.FinalStateCompleted {
		t.Fatalf("first attempt final state = %q", result.FinalState)
	}
	if err := scheduler.ReleaseClaim(t.Context(), issueID, "completed"); err != nil {
		t.Fatal(err)
	}

	stream.await(t, "the completed execution", func(frames []sseFrame) bool {
		return containsExecutionStatus(t, frames, conversation.ExecutionCompleted)
	})
	completed := f.awaitSnapshot(t, f.owner, id, "the completed execution", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionCompleted
	})
	for _, message := range completed.Messages {
		if message.Role == conversation.RoleAssistant && message.Kind == conversation.MessageText && message.Delivery != conversation.DeliveryCompleted {
			t.Fatalf("assistant message %s delivery = %q, want completed", message.ID, message.Delivery)
		}
	}
	// The provider acknowledges no answer, so the answer control is still
	// outstanding when the turn ends and the hub settles it with the
	// execution's own outcome rather than leaving it in flight.
	assertDelivery(t, completed.Messages, "answer-1", conversation.DeliveryCompleted)
	assertDelivery(t, completed.Messages, "steer-1", conversation.DeliveryDelivered)
	midCursor := completed.Cursor

	// Continuation: an explicit continue records intent and waits for a
	// runner; the hub never starts one.
	// A continue names the attempt the caller last saw, so it cannot land on
	// a conversation someone else moved on in the meantime.
	continued := f.command(t, f.owner, id, conversation.Command{
		Key: "continue-1", Kind: conversation.CommandContinue, Text: "Continue with the release notes.",
		Expected: conversation.Expected{AttemptID: firstAttempt},
	})
	if continued.Status != conversation.DeliverySaved {
		t.Fatalf("continue receipt = %#v", continued)
	}
	waitingAgain := f.snapshot(t, f.owner, id)
	if waitingAgain.Conversation.Execution.Status != conversation.ExecutionWaitingForRunner {
		t.Fatalf("execution after continue = %#v", waitingAgain.Conversation.Execution)
	}

	// The second attempt: a new claim, a new attempt id, the same provider
	// thread and the queued continuation text in the prompt.
	nextIssue := f.claimIssue(t, scheduler, issueID)
	secondRun := f.startAttempt(t, scheduler, backend, nextIssue)
	secondWaiting := f.awaitSnapshot(t, f.owner, id, "the second attempt to ask", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionWaitingInput
	})
	secondAttempt := *secondWaiting.Conversation.Execution.AttemptID
	if secondAttempt == firstAttempt {
		t.Fatalf("second attempt reused the first attempt id %s", firstAttempt)
	}
	requests := backend.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider turns = %d, want one per attempt", len(requests))
	}
	// The hub-side coordinator produced the conversation's thread, so no
	// runner can resume it: the first turn is handed the transcript instead.
	if requests[0].Resume.ThreadID != "" {
		t.Fatalf("first turn resume = %q, want no thread: this runner did not produce it", requests[0].Resume.ThreadID)
	}
	if !strings.Contains(requests[0].Prompt, "<transcript>") {
		t.Fatalf("first turn prompt has no transcript to continue from: %q", requests[0].Prompt)
	}
	if !strings.Contains(requests[0].Prompt, "Keep the public API stable.") {
		t.Fatalf("first turn prompt lost the queued follow-up: %q", requests[0].Prompt)
	}
	// This worker holds a legacy machine credential with no runner identity,
	// so the hub never hands it a provider thread back: it continues from the
	// transcript instead (decisions section 9.3).
	if requests[1].Resume.ThreadID != "" {
		t.Fatalf("continuation resume = %q, want no thread for a credential with no runner identity", requests[1].Resume.ThreadID)
	}
	if !strings.Contains(requests[1].Prompt, "<transcript>") {
		t.Fatalf("continuation prompt has no transcript to continue from: %q", requests[1].Prompt)
	}
	if !strings.Contains(requests[1].Prompt, "Continue with the release notes.") {
		t.Fatalf("continuation prompt lost the continue text: %q", requests[1].Prompt)
	}

	// A stale answer that names the finished attempt is refused, and the
	// question the first attempt already settled cannot be answered twice.
	secondQuestion := secondWaiting.Questions[len(secondWaiting.Questions)-1]
	for _, question := range secondWaiting.Questions {
		if question.Status == conversation.QuestionPending {
			secondQuestion = question
		}
	}
	stale := f.failure(t, f.owner, http.MethodPost, f.base+"/conversations/"+id+"/commands", conversation.Command{
		Key: "answer-stale", Kind: conversation.CommandAnswer, QuestionID: secondQuestion.ID,
		Answers:  map[string][]string{"ship": {"Yes"}},
		Expected: conversation.Expected{AttemptID: firstAttempt},
	}, http.StatusConflict)
	if stale.Code != "stale_execution" {
		t.Fatalf("stale answer = %#v, want stale_execution", stale)
	}
	if stale.Details["expected_attempt_id"] != firstAttempt || stale.Details["current_attempt_id"] != secondAttempt {
		t.Fatalf("stale answer details = %#v", stale.Details)
	}
	replay := f.failure(t, f.owner, http.MethodPost, f.base+"/conversations/"+id+"/commands", conversation.Command{
		Key: "answer-replayed", Kind: conversation.CommandAnswer, QuestionID: opened.ID,
		Answers: map[string][]string{"approach": {"Rewrite"}},
	}, http.StatusConflict)
	if replay.Code != "question_already_answered" {
		t.Fatalf("re-answering the settled question = %#v", replay)
	}

	f.command(t, f.owner, id, conversation.Command{
		Key: "answer-2", Kind: conversation.CommandAnswer, QuestionID: secondQuestion.ID,
		Answers:  map[string][]string{"ship": {"Yes"}},
		Expected: conversation.Expected{AttemptID: secondAttempt},
	})
	secondResult := awaitAttempt(t, secondRun)
	if secondResult.FinalState != runner.FinalStateCompleted {
		t.Fatalf("second attempt final state = %q", secondResult.FinalState)
	}
	if err := scheduler.ReleaseClaim(t.Context(), issueID, "completed"); err != nil {
		t.Fatal(err)
	}
	final := f.awaitSnapshot(t, f.owner, id, "the second completed execution", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionCompleted && s.Conversation.Execution.AttemptID != nil && *s.Conversation.Execution.AttemptID == secondAttempt
	})

	// The whole history is one conversation, in sequence order.
	if len(final.Messages) < 12 {
		t.Fatalf("history = %d messages, want the whole slice", len(final.Messages))
	}
	for index, message := range final.Messages {
		if message.ConversationID != id {
			t.Fatalf("message %s belongs to conversation %s", message.ID, message.ConversationID)
		}
		if index > 0 && message.Seq <= final.Messages[index-1].Seq {
			t.Fatalf("messages out of order at %d: %d after %d", index, message.Seq, final.Messages[index-1].Seq)
		}
	}
	if got := deltaText(stream.collected()); !strings.Contains(got, "Shipping decision: Yes.") {
		t.Fatalf("continuation deltas never reached the subscriber: %q", got)
	}
	if controls := backend.recordedControls(); len(controls) != 3 {
		t.Fatalf("provider consumed %d controls, want the answer, the steer and the continuation answer", len(controls))
	}
	return sliceResult{conversationID: id, midCursor: midCursor, frames: stream.collected()}
}

// assertConversationReload proves that a snapshot plus the events after its
// cursor is exact: no gap, no duplicate, ordered by id.
func assertConversationReload(t *testing.T, f *conversationFixture, slice sliceResult) {
	if slice.conversationID == "" {
		t.Skip("the vertical slice did not run")
	}
	snapshot := f.snapshot(t, f.owner, slice.conversationID)
	replay := f.stream(t, f.owner, slice.conversationID, slice.midCursor)
	want := make([]sseFrame, 0, len(slice.frames))
	for _, frame := range slice.frames {
		seq, err := strconv.ParseInt(frame.ID, 10, 64)
		if err != nil {
			t.Fatalf("frame without a cursor: %#v", frame)
		}
		if seq > slice.midCursor && seq <= snapshot.Cursor {
			want = append(want, frame)
		}
	}
	if len(want) == 0 {
		t.Fatal("the vertical slice left no events after the mid-stream cursor")
	}
	got := replay.await(t, "the replay to reach the snapshot cursor", func(frames []sseFrame) bool {
		return len(frames) >= len(want)
	})
	got = got[:len(want)]
	previous := slice.midCursor
	for index, frame := range got {
		seq, err := strconv.ParseInt(frame.ID, 10, 64)
		if err != nil {
			t.Fatalf("replayed frame without a cursor: %#v", frame)
		}
		if seq <= previous {
			t.Fatalf("replay out of order or duplicated at %d: %d after %d", index, seq, previous)
		}
		previous = seq
		if frame.ID != want[index].ID || frame.Event != want[index].Event || frame.Data != want[index].Data {
			t.Fatalf("replayed frame %d = %#v, live stream saw %#v", index, frame, want[index])
		}
	}
	// A cursor beyond the retained window is not silently accepted.
	ahead := f.stream(t, f.owner, slice.conversationID, snapshot.Cursor+1000)
	frames := ahead.await(t, "the closed frame", func(frames []sseFrame) bool { return len(frames) > 0 })
	if frames[0].Event != string(conversation.EventClosed) || !strings.Contains(frames[0].Data, "cursor_expired") {
		t.Fatalf("stream from an impossible cursor = %#v", frames[0])
	}
}

// assertConversationIsolation proves that concurrent chats do not leak into
// each other and that the audience rules hold on every surface.
func assertConversationIsolation(t *testing.T, f *conversationFixture, slice sliceResult) {
	if slice.conversationID == "" {
		t.Skip("the vertical slice did not run")
	}
	// Both chats are created before either reply is awaited, so the hub
	// answers them concurrently.
	first := f.createConversation(t, f.owner, "", "isolated-a", "Marker ALPHA for the first chat.")
	second := f.createConversation(t, f.owner, "", "isolated-b", "Marker BRAVO for the second chat.")
	settled := func(s wireSnapshot) bool {
		for _, message := range s.Messages {
			if message.Role == conversation.RoleAssistant && message.Delivery == conversation.DeliveryCompleted {
				return true
			}
		}
		return false
	}
	for _, chat := range []struct {
		id     string
		mine   string
		theirs string
	}{
		{first.Conversation.ID, "Marker ALPHA", "Marker BRAVO"},
		{second.Conversation.ID, "Marker BRAVO", "Marker ALPHA"},
	} {
		snapshot := f.awaitSnapshot(t, f.owner, chat.id, "the coordinator reply for "+chat.mine, settled)
		var replies strings.Builder
		for _, message := range snapshot.Messages {
			if message.Role == conversation.RoleAssistant {
				replies.WriteString(message.Text)
			}
		}
		if !strings.Contains(replies.String(), chat.mine) || strings.Contains(replies.String(), chat.theirs) {
			t.Fatalf("conversation %s replies = %q, want only %s", chat.id, replies.String(), chat.mine)
		}
	}

	// A member of the project cannot see a private chat; the shared,
	// linked one is readable.
	f.failure(t, f.member, http.MethodGet, f.base+"/conversations/"+first.Conversation.ID, nil, http.StatusNotFound)
	f.expect(t, f.member, http.MethodGet, f.base+"/conversations/"+slice.conversationID, nil, http.StatusOK, nil)

	// An operator of another project sees nothing at all, on every surface.
	for _, path := range []string{
		f.base + "/conversations/" + slice.conversationID,
		f.base + "/conversations/" + slice.conversationID + "/events?after=0",
		f.base + "/conversations/" + slice.conversationID + "/messages",
	} {
		f.failure(t, f.outsider, http.MethodGet, path, nil, http.StatusNotFound)
	}
	f.failure(t, f.outsider, http.MethodPost, f.base+"/conversations/"+slice.conversationID+"/commands",
		conversation.Command{Key: "outsider", Kind: conversation.CommandMessage, Text: "let me in"}, http.StatusNotFound)
	// The outsider's own project answers, so the refusals above are the
	// audience rule rather than a broken credential.
	f.expect(t, f.outsider, http.MethodGet, f.otherBase+"/conversations", nil, http.StatusOK, nil)
}

// assertConversationLinkDoubleSubmit proves that a double submit creates one
// issue and that a second link attempt names the existing conversation.
func assertConversationLinkDoubleSubmit(t *testing.T, f *conversationFixture) {
	created := f.createConversation(t, f.owner, "Double submit", "", "")
	id := created.Conversation.ID
	request := map[string]any{"key": "link-double", "share_history": true, "issue": map[string]any{"title": "Double submitted issue"}}
	type linkAttempt struct {
		status  int
		payload []byte
		err     error
	}
	results := make([]linkAttempt, 2)
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			status, payload, err := f.tryCall(f.owner, http.MethodPost, f.base+"/conversations/"+id+"/link", request)
			results[index] = linkAttempt{status: status, payload: payload, err: err}
		}()
	}
	wait.Wait()
	for _, result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.status != http.StatusOK {
			t.Fatalf("concurrent link = %d: %s", result.status, result.payload)
		}
	}
	var first, second wireLinkResult
	if err := json.Unmarshal(results[0].payload, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(results[1].payload, &second); err != nil {
		t.Fatal(err)
	}
	if first.Issue.ID != second.Issue.ID {
		t.Fatalf("double submit created two issues: %s and %s", first.Issue.ID, second.Issue.ID)
	}
	if string(results[0].payload) != string(results[1].payload) {
		t.Fatalf("replayed link differs:\n%s\n%s", results[0].payload, results[1].payload)
	}

	// A different key afterwards is a link conflict that names where to go.
	conflict := f.failure(t, f.owner, http.MethodPost, f.base+"/conversations/"+id+"/link",
		map[string]any{"key": "link-double-again", "share_history": true, "issue": map[string]any{"title": "Second issue"}}, http.StatusConflict)
	if conflict.Code != "conversation_already_linked" || conflict.Details["existing_conversation_id"] != id {
		t.Fatalf("second link = %#v", conflict)
	}
}

// conversationSettled reports whether the stream shows a completed assistant
// reply that answers the given user message.
func conversationSettled(t *testing.T, frames []sseFrame, userText string) bool {
	t.Helper()
	seenUser := false
	for _, frame := range frames {
		if frame.Event != string(conversation.EventMessageAccepted) {
			continue
		}
		var message wireMessage
		if err := json.Unmarshal([]byte(frame.Data), &message); err != nil {
			t.Fatalf("decode accepted message: %v", err)
		}
		if message.Role == conversation.RoleUser && message.Text == userText {
			seenUser = true
		}
	}
	if !seenUser {
		return false
	}
	for _, frame := range frames {
		if frame.Event != string(conversation.EventMessageUpdated) {
			continue
		}
		var message wireMessage
		if err := json.Unmarshal([]byte(frame.Data), &message); err != nil {
			t.Fatalf("decode updated message: %v", err)
		}
		if message.Role == conversation.RoleAssistant && message.Delivery == conversation.DeliveryCompleted && strings.Contains(message.Text, userText) {
			// The coordinator commits the completed reply and the idle
			// execution in one transaction; wait for both frames so the
			// caller never observes half of it.
			return lastExecutionStatus(t, frames) == conversation.ExecutionIdle
		}
	}
	return false
}

// containsExecutionStatus reports whether the stream carried the status.
func containsExecutionStatus(t *testing.T, frames []sseFrame, want conversation.ExecutionStatus) bool {
	t.Helper()
	for _, status := range executionStatuses(t, frames) {
		if status == want {
			return true
		}
	}
	return false
}

// lastExecutionStatus is the newest execution.updated status on the stream.
func lastExecutionStatus(t *testing.T, frames []sseFrame) conversation.ExecutionStatus {
	t.Helper()
	statuses := executionStatuses(t, frames)
	if len(statuses) == 0 {
		return ""
	}
	return statuses[len(statuses)-1]
}

func findMessage(t *testing.T, messages []wireMessage, match func(wireMessage) bool) wireMessage {
	t.Helper()
	for _, message := range messages {
		if match(message) {
			return message
		}
	}
	t.Fatalf("no matching message among %d", len(messages))
	return wireMessage{}
}

func assertDelivery(t *testing.T, messages []wireMessage, key string, want conversation.Delivery) {
	t.Helper()
	message := findMessage(t, messages, func(message wireMessage) bool {
		return message.CommandKey != nil && *message.CommandKey == key
	})
	if message.Delivery != want {
		t.Fatalf("control %q delivery = %q, want %q", key, message.Delivery, want)
	}
}
