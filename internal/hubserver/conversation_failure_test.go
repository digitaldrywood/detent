package hubserver

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// This file covers plan task O02: delivery failures, authorization changes
// and recovery. Every case asserts the durable state a restart would read
// back, not only the response, because the contract's promise is that a
// control never targets the wrong turn and a mutation is never replayed
// blindly. Cases already proven elsewhere are cited in the report rather
// than repeated here.

// conversationOperatorToken issues an operator token with a grant on the
// fixture's project and returns the bearer token and its principal id, so a
// test can act as a human actor and then revoke that actor.
func conversationOperatorToken(t *testing.T, f nativeFixture, name string) (token string, principal string) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": name, "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var created tokenResponse
	decodeHubResponse(t, response, &created)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+created.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID}), http.StatusNoContent)
	return created.Token, created.ID
}

// conversationTokenID resolves the principal id of a token by its name.
func conversationTokenID(t *testing.T, service *Service, name string) string {
	t.Helper()
	var id string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = ?", name).Scan(&id); err != nil {
		t.Fatalf("read token %q: %v", name, err)
	}
	return id
}

// conversationRevokeToken revokes a principal the way an operator removing
// an actor does: the row stays, the credential stops authenticating.
func conversationRevokeToken(t *testing.T, service *Service, principal string) {
	t.Helper()
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE id = ?", testTimestamp, principal); err != nil {
		t.Fatalf("revoke token %q: %v", principal, err)
	}
}

// command posts an operator command envelope to the fixture's conversation.
func (f *conversationWorkerFixture) command(t *testing.T, token string, command conversation.Command) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", token, command)
}

// generation captures the worker identity the fixture currently drives so a
// replaced attempt can still issue its late calls.
type conversationGeneration struct {
	lease   tracker.NativeLease
	attempt string
	run     string
}

func (f *conversationWorkerFixture) generation() conversationGeneration {
	return conversationGeneration{lease: f.lease, attempt: f.attempt, run: f.run}
}

// as runs body with the fixture pointed at an earlier generation, then
// restores the current one. It is how a replaced worker's late calls are
// driven through the same helpers.
func (f *conversationWorkerFixture) as(generation conversationGeneration, body func()) {
	current := f.generation()
	defer func() { f.lease, f.attempt, f.run = current.lease, current.attempt, current.run }()
	f.lease, f.attempt, f.run = generation.lease, generation.attempt, generation.run
	body()
}

// restart closes the hub and opens a new one on the same database file, the
// way a process restart does, and points the fixture at the new service.
func (f *conversationWorkerFixture) restart(t *testing.T) {
	t.Helper()
	config := Config{DatabasePath: f.service.config.DatabasePath, Conversation: &ConversationConfig{Enabled: true, QuestionTimeout: time.Hour}, now: f.service.config.now}
	if err := f.service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	service := openTestService(t, config)
	f.service = service
	f.chat = service.conversations
	if f.chat == nil {
		t.Fatal("the reopened hub must keep the conversation product enabled")
	}
}

// requireConversationDelivery asserts the durable delivery of a message and
// of the receipt that mirrors it.
func requireConversationDelivery(t *testing.T, f *conversationWorkerFixture, message conversationMessageRecord, key string, want conversation.Delivery) {
	t.Helper()
	if got := f.messages(t)[message.ID].Delivery; got != want {
		t.Fatalf("%s delivery = %q, want %q", key, got, want)
	}
	if got := f.receipt(t, key).Status; got != want {
		t.Fatalf("%s receipt status = %q, want %q", key, got, want)
	}
}

// TestConversationReplacedLeaseKeepsControlsOnTheirOwnAttempt covers the
// replaced-lease case: worker A binds attempt 1 and is handed two controls,
// its lease is released and worker B claims attempt 2. Nothing A was handed
// is replayed: both controls become unknown for the user to retry
// explicitly, the message queued after the release reaches B as a fresh
// delivery, and every late call from A is stale_execution.
func TestConversationReplacedLeaseKeepsControlsOnTheirOwnAttempt(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "Continue?"}}},
	), http.StatusAccepted)
	question := f.questions(t)[0]
	steer := f.queue(t, conversation.MessageText, "steer-a", "Also update the docs", nil)
	answer := f.queue(t, conversation.MessageAnswer, "answer-a", "", map[string]any{"question_id": question.ID, "answers": map[string][]string{"q": {"yes"}}})
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	requireConversationDelivery(t, f, steer, "steer-a", conversation.DeliverySending)
	requireConversationDelivery(t, f, answer, "answer-a", conversation.DeliverySending)
	replaced := f.generation()

	// The lease is released without an unbind: the hub settles the execution
	// itself rather than waiting for a worker that may never come back.
	f.release(t)
	if record := f.load(t); record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != conversationLeaseLostError {
		t.Fatalf("execution after release = %#v", record.Execution)
	}
	// A control that was handed to the worker is never replayed, whatever
	// its kind (decisions section 10.3).
	requireConversationDelivery(t, f, answer, "answer-a", conversation.DeliveryUnknown)
	requireConversationDelivery(t, f, steer, "steer-a", conversation.DeliveryUnknown)
	// A control accepted after the release was never handed out, so it is
	// still queued for the replacement attempt.
	waiting := f.queue(t, conversation.MessageText, "steer-b", "And check the docs", nil)
	requireConversationDelivery(t, f, waiting, "steer-b", conversation.DeliveryQueued)
	questions := f.questions(t)
	if len(questions) == 0 {
		t.Fatal("expected a question")
	}
	if got := questions[0].Status; got != conversation.QuestionExpired {
		t.Fatalf("question status = %q, want expired", got)
	}

	f.claim(t)
	response := f.bind(t, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var bound workerBindResponse
	decodeHubResponse(t, response, &bound)
	if len(bound.Pending) != 1 || bound.Pending[0].MessageID != waiting.ID {
		t.Fatalf("pending for the replacement attempt = %#v, want only %s", bound.Pending, waiting.ID)
	}
	if bound.Pending[0].Expected.AttemptID != f.attempt {
		t.Fatalf("expected attempt = %q, want the replacement %q", bound.Pending[0].Expected.AttemptID, f.attempt)
	}
	// The still-queued control is handed to B as a fresh delivery. What A
	// was already handed stays unknown and is never handed out again.
	if got := f.messages(t)[waiting.ID]; got.Delivery != conversation.DeliverySending || got.AttemptID != f.attempt {
		t.Fatalf("handed control = %#v, want sending for %s", got, f.attempt)
	}
	requireConversationDelivery(t, f, answer, "answer-a", conversation.DeliveryUnknown)
	requireConversationDelivery(t, f, steer, "steer-a", conversation.DeliveryUnknown)

	t.Run("the replaced worker cannot reach the new turn", func(t *testing.T) {
		before := f.load(t)
		f.as(replaced, func() {
			requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
			requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-late", "text": "late"}), http.StatusConflict, "stale_execution")
			requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "control_result", "key": "steer-a", "status": "delivered"}), http.StatusConflict, "stale_execution")
		})
		after := f.load(t)
		if after.EventSeq != before.EventSeq || after.Revision != before.Revision {
			t.Fatalf("a rejected call changed state: seq %d->%d revision %d->%d", before.EventSeq, after.EventSeq, before.Revision, after.Revision)
		}
		if got := f.messages(t)[waiting.ID]; got.Delivery != conversation.DeliverySending || got.AttemptID != f.attempt {
			t.Fatalf("control after the replaced worker's calls = %#v", got)
		}
	})
}

// TestConversationRevokedActorMidFlight covers a command that was accepted
// while its actor was authorized and whose actor is revoked before the
// worker polls. Delivering it afterwards is the deliberate rule, not an
// oversight: acceptance is durable and the audit records who sent it, so the
// hub does not try to recall a control the provider may already have seen.
// The actor's later requests and streams are denied instead
// (decisions section 10.16). Worker-token revocation is the other half of
// that rule and still refuses the write; see
// TestConversationRevokedWorkerToken.
func TestConversationRevokedActorMidFlight(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	operator, principal := conversationOperatorToken(t, f.nativeFixture, "conversation-revoked-actor")
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)

	response := f.command(t, operator, conversation.Command{Key: "op-1", Kind: conversation.CommandMessage, Text: "Also update the docs"})
	requireNativeStatus(t, response, http.StatusOK)
	var receipt conversation.Receipt
	decodeHubResponse(t, response, &receipt)
	if receipt.Status != conversation.DeliveryQueued || receipt.MessageID == "" {
		t.Fatalf("receipt = %#v", receipt)
	}

	conversationRevokeToken(t, f.service, principal)

	t.Run("the accepted control is still delivered", func(t *testing.T) {
		response := f.controls(t, 0, 0)
		requireNativeStatus(t, response, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, response, &page)
		if len(page.Controls) != 1 || page.Controls[0].Key != "op-1" || page.Controls[0].Text != "Also update the docs" {
			t.Fatalf("controls = %#v, want the accepted command", page.Controls)
		}
		if got := f.messages(t)[receipt.MessageID].Delivery; got != conversation.DeliverySending {
			t.Fatalf("delivery = %q, want sending", got)
		}
		if got := f.receipt(t, "op-1").Status; got != conversation.DeliverySending {
			t.Fatalf("receipt status = %q, want sending", got)
		}
	})

	t.Run("the revoked actor is denied", func(t *testing.T) {
		before := f.load(t)
		requireConversationErrorCode(t, f.command(t, operator, conversation.Command{Key: "op-2", Kind: conversation.CommandMessage, Text: "And this"}), http.StatusUnauthorized, "unauthorized")
		requireConversationErrorCode(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+f.record.ID+"/events?after=0", operator, nil), http.StatusUnauthorized, "unauthorized")
		requireConversationErrorCode(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+f.record.ID, operator, nil), http.StatusUnauthorized, "unauthorized")
		if after := f.load(t); after.EventSeq != before.EventSeq {
			t.Fatalf("a denied command emitted events: %d -> %d", before.EventSeq, after.EventSeq)
		}
		var commands int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_commands WHERE conversation_id = ? AND key = 'op-2'", f.record.ID).Scan(&commands); err != nil {
			t.Fatal(err)
		}
		if commands != 0 {
			t.Fatal("a denied command must not reserve its key")
		}
	})
}

// TestConversationRevokedWorkerToken covers losing the runner's own
// credential mid-flight. Unlike operator revocation, which leaves an already
// accepted control on its way (decisions section 10.16), a revoked worker
// token refuses the write outright: the worker endpoints stop accepting its
// calls at once, and because it can no longer renew, the lease expires and
// reconcile settles the execution the same way lease loss does.
func TestConversationRevokedWorkerToken(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "Continue?"}}},
	), http.StatusAccepted)
	question := f.questions(t)[0]
	answer := f.queue(t, conversation.MessageAnswer, "answer-1", "", map[string]any{"question_id": question.ID, "answers": map[string][]string{"q": {"yes"}}})
	text := f.queue(t, conversation.MessageText, "text-1", "Later", nil)
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)

	conversationRevokeToken(t, f.service, conversationTokenID(t, f.service, "conversation-worker"))

	t.Run("worker endpoints stop accepting the credential", func(t *testing.T) {
		before := f.load(t)
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusUnauthorized, "unauthorized")
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "late"}), http.StatusUnauthorized, "unauthorized")
		requireConversationErrorCode(t, f.bind(t, nil), http.StatusUnauthorized, "unauthorized")
		requireConversationErrorCode(t, f.unbind(t, "failed"), http.StatusUnauthorized, "unauthorized")
		if after := f.load(t); after.EventSeq != before.EventSeq || after.Execution.Status != before.Execution.Status {
			t.Fatalf("a denied worker call changed state: %#v -> %#v", before.Execution, after.Execution)
		}
	})

	t.Run("the abandoned execution is reconciled", func(t *testing.T) {
		// A revoked worker cannot renew, so the lease runs out and the hub
		// settles the execution without the worker's cooperation.
		f.advance(11 * time.Minute)
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatalf("reconcileExecutions() error = %v", err)
		}
		record := f.load(t)
		if record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != conversationLeaseLostError {
			t.Fatalf("execution = %#v", record.Execution)
		}
		requireConversationDelivery(t, f, answer, "answer-1", conversation.DeliveryUnknown)
		// The text control had already been handed to the worker, so it is
		// unknown rather than re-queued (decisions section 10.3).
		requireConversationDelivery(t, f, text, "text-1", conversation.DeliveryUnknown)
		if got := f.questions(t)[0].Status; got != conversation.QuestionExpired {
			t.Fatalf("question status = %q, want expired", got)
		}
	})
}

// conversationQuestionFixture is a linked conversation whose current attempt
// owns one question in the requested status. The execution is written
// directly because the answer rules are what is under test, not binding.
type conversationQuestionFixture struct {
	conversationAPIFixture
	conversationID string
	questionID     string
}

func newConversationQuestionFixture(t *testing.T, status conversation.QuestionStatus) conversationQuestionFixture {
	t.Helper()
	f := newConversationAPIFixture(t, nil)
	record := f.create(t, f.token, map[string]any{"title": "Questions"}).Conversation
	requireNativeStatus(t, f.link(t, f.token, record.ID, "link", true, "Ask me"), http.StatusOK)
	ctx := t.Context()
	service := f.service.conversations
	questionID := conversation.NewQuestionID()
	err := service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		linked, err := service.store.readConversation(ctx, tx, f.project.OrganizationID, f.project.ID, record.ID)
		if err != nil {
			return err
		}
		execution := conversation.Execution{Status: conversation.ExecutionWaitingInput, Owner: conversation.Owner{AttemptID: "att_1", RunID: "run_1", TurnID: "turn_1"}, Capabilities: conversation.Capabilities{Answer: true}}
		if err := service.updateExecution(ctx, tx, &linked, execution, now); err != nil {
			return err
		}
		return service.store.upsertQuestion(ctx, tx, conversationQuestionRecord{Question: conversation.Question{
			ID: questionID, ConversationID: record.ID, Status: status, Owner: execution.Owner,
			Prompts:   []conversation.Prompt{{ID: "color", Question: "Which color?", Options: []conversation.Option{{Label: "red"}, {Label: "blue"}}}},
			CreatedAt: now, UpdatedAt: now,
		}})
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}
	return conversationQuestionFixture{conversationAPIFixture: f, conversationID: record.ID, questionID: questionID}
}

func (f conversationQuestionFixture) answer(t *testing.T, key, value string) *httptest.ResponseRecorder {
	t.Helper()
	return f.command(t, f.token, f.conversationID, conversation.Command{Key: key, Kind: conversation.CommandAnswer, QuestionID: f.questionID, Answers: map[string][]string{"color": {value}}})
}

func (f conversationQuestionFixture) questionStatus(t *testing.T) conversation.QuestionStatus {
	t.Helper()
	var status string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT status FROM conversation_questions WHERE id = ?", f.questionID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return conversation.QuestionStatus(status)
}

// TestConversationDuplicateAnswers covers the three ways one question can be
// answered twice: the same key retried, two keys racing, and a key arriving
// after the question already expired.
func TestConversationDuplicateAnswers(t *testing.T) {
	t.Parallel()

	t.Run("the same key returns the stored receipt", func(t *testing.T) {
		t.Parallel()
		f := newConversationQuestionFixture(t, conversation.QuestionPending)
		first := f.answer(t, "a1", "red")
		requireNativeStatus(t, first, http.StatusOK)
		replay := f.answer(t, "a1", "red")
		requireNativeStatus(t, replay, http.StatusOK)
		if replay.Body.String() != first.Body.String() {
			t.Fatalf("replay = %s, want %s", replay.Body.String(), first.Body.String())
		}
		var answers int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id = ? AND kind = ?", f.conversationID, conversation.MessageAnswer).Scan(&answers); err != nil {
			t.Fatal(err)
		}
		if answers != 1 {
			t.Fatalf("answer messages = %d, want 1 (a replay must not append)", answers)
		}
	})

	t.Run("two keys race and exactly one wins", func(t *testing.T) {
		t.Parallel()
		f := newConversationQuestionFixture(t, conversation.QuestionPending)
		var wg sync.WaitGroup
		results := make([]*httptest.ResponseRecorder, 2)
		for index, value := range []string{"red", "blue"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[index] = f.answer(t, "race-"+strconv.Itoa(index), value)
			}()
		}
		wg.Wait()
		accepted, refused := 0, 0
		for _, response := range results {
			switch response.Code {
			case http.StatusOK:
				accepted++
				var receipt conversation.Receipt
				decodeHubResponse(t, response, &receipt)
				if receipt.Status != conversation.DeliveryQueued || receipt.QuestionID != f.questionID {
					t.Fatalf("winning receipt = %#v", receipt)
				}
			case http.StatusConflict:
				refused++
				if code := decodeNativeError(t, response).Code; code != "question_already_answered" {
					t.Fatalf("loser code = %q, want question_already_answered", code)
				}
			default:
				t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
			}
		}
		if accepted != 1 || refused != 1 {
			t.Fatalf("accepted %d refused %d, want exactly one winner", accepted, refused)
		}
		var answers int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id = ? AND kind = ?", f.conversationID, conversation.MessageAnswer).Scan(&answers); err != nil {
			t.Fatal(err)
		}
		if answers != 1 {
			t.Fatalf("answer messages = %d, want exactly one", answers)
		}
		if got := f.questionStatus(t); got != conversation.QuestionSending {
			t.Fatalf("question status = %q, want sending", got)
		}
	})

	t.Run("an expired question is stale", func(t *testing.T) {
		t.Parallel()
		f := newConversationQuestionFixture(t, conversation.QuestionExpired)
		requireNativeError(t, f.answer(t, "late", "red"), http.StatusConflict, "stale_execution")
		if got := f.questionStatus(t); got != conversation.QuestionExpired {
			t.Fatalf("question status = %q, want expired", got)
		}
		var answers int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id = ? AND kind = ?", f.conversationID, conversation.MessageAnswer).Scan(&answers); err != nil {
			t.Fatal(err)
		}
		if answers != 0 {
			t.Fatalf("answer messages = %d, want none", answers)
		}
	})
}

// conversationBurstDeltas appends one assistant message and count deltas to
// it, waking subscribers after each commit the way a worker batch does.
// It returns the sequence of event ids the burst produced.
func conversationBurstDeltas(t *testing.T, f streamFixture, id string, count int) []string {
	t.Helper()
	ctx := t.Context()
	service := f.service.conversations
	var message conversationMessageRecord
	ids := make([]string, 0, count+1)
	emit := func(apply func(tx *sql.Tx, record *conversationRecord, now time.Time) error) {
		t.Helper()
		err := service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
			record, err := service.store.readConversation(ctx, tx, f.project.OrganizationID, f.project.ID, id)
			if err != nil {
				return err
			}
			if err := apply(tx, &record, now); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, "SELECT event_seq FROM conversations WHERE id = ?", id).Scan(&record.EventSeq)
		})
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		service.broker.notify(id)
	}
	emit(func(tx *sql.Tx, record *conversationRecord, now time.Time) error {
		message = conversationMessageRecord{
			Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding,
			Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "runner"},
		}
		return service.appendMessage(ctx, tx, record, &message, now)
	})
	ids = append(ids, "1")
	for i := range count {
		emit(func(tx *sql.Tx, _ *conversationRecord, now time.Time) error {
			return service.appendDelta(ctx, tx, &message, "d"+strconv.Itoa(i), now)
		})
		ids = append(ids, strconv.Itoa(i+2))
	}
	return ids
}

// TestConversationTwoTabsShareOneOrderedLog covers two tabs on one
// conversation: both read the same ordered ids for a burst, and a tab that
// reconnects with a stale cursor receives exactly the gap, never a
// duplicate and never a hole.
func TestConversationTwoTabsShareOneOrderedLog(t *testing.T) {
	f := newStreamFixture(t)
	id := f.create(t, f.token, map[string]any{"title": "Two tabs"}).Conversation.ID
	first := f.open(t, f.token, id, 0)
	second := f.open(t, f.token, id, 0)

	const deltas = 30
	want := conversationBurstDeltas(t, f, id, deltas)

	read := func(stream *sseStream, n int) []string {
		t.Helper()
		ids := make([]string, 0, n)
		for range n {
			ids = append(ids, stream.nextEvent(t).ID)
		}
		return ids
	}
	if got := read(first, len(want)); !conversationEqualIDs(got, want) {
		t.Fatalf("first tab ids = %v, want %v", got, want)
	}
	const reconnectAfter = 10
	if got := read(second, reconnectAfter); !conversationEqualIDs(got, want[:reconnectAfter]) {
		t.Fatalf("second tab ids = %v, want %v", got, want[:reconnectAfter])
	}
	second.close()

	cursor, err := strconv.ParseInt(want[reconnectAfter-1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	resumed := f.open(t, f.token, id, cursor)
	if got := read(resumed, len(want)-reconnectAfter); !conversationEqualIDs(got, want[reconnectAfter:]) {
		t.Fatalf("resumed ids = %v, want exactly the gap %v", got, want[reconnectAfter:])
	}
	resumed.close()
	first.close()
}

func conversationEqualIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestConversationRejectsReorderedWorkerReports covers turn events that
// arrive out of the order the ladder allows: a result for a control that was
// never handed out, and a completion for a turn that is not current. Both are
// rejected and neither may leave a trace.
func TestConversationRejectsReorderedWorkerReports(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)

	t.Run("a result before the hand-off is stale", func(t *testing.T) {
		queued := f.queue(t, conversation.MessageText, "early-1", "Not handed out yet", nil)
		if got := f.messages(t)[queued.ID]; got.Delivery != conversation.DeliveryQueued || got.AttemptID != "" {
			t.Fatalf("seeded control = %#v, want queued and unowned", got)
		}
		before := f.load(t)
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "control_result", "key": "early-1", "status": "delivered"}), http.StatusConflict, "stale_execution")
		after := f.load(t)
		if after.EventSeq != before.EventSeq || after.Revision != before.Revision {
			t.Fatalf("a rejected result changed state: seq %d->%d revision %d->%d", before.EventSeq, after.EventSeq, before.Revision, after.Revision)
		}
		requireConversationDelivery(t, f, queued, "early-1", conversation.DeliveryQueued)
	})

	t.Run("a completion for another turn is stale", func(t *testing.T) {
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
		before := f.load(t)
		requireConversationErrorCode(t, f.turnEvents(t,
			map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "would be lost"},
			map[string]any{"type": "turn_completed", "turn_id": "turn-other", "status": "completed"},
		), http.StatusConflict, "stale_execution")
		after := f.load(t)
		if after.EventSeq != before.EventSeq || after.Execution.Owner.TurnID != "turn-1" || after.Execution.Status != conversation.ExecutionRunning {
			t.Fatalf("state after a rejected batch = %#v seq %d, want unchanged from seq %d", after.Execution, after.EventSeq, before.EventSeq)
		}
		for _, message := range f.messages(t) {
			if message.ProviderItemID == "item-1" {
				t.Fatalf("a rolled-back batch left a message: %#v", message)
			}
		}
	})
}

// TestConversationQueueOverflowPersistsNothing covers the bounded control
// queue: the command that overflows it is rejected before anything is
// written, and the same key succeeds once the queue drains.
func TestConversationQueueOverflowPersistsNothing(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ControlQueueSize: 3})
	record := f.create(t, f.token, map[string]any{"title": "Bounded"}).Conversation
	requireNativeStatus(t, f.link(t, f.token, record.ID, "link", true, "Bound the queue"), http.StatusOK)
	for _, key := range []string{"q1", "q2", "q3"} {
		response := f.command(t, f.token, record.ID, conversation.Command{Key: key, Kind: conversation.CommandMessage, Text: "Steer " + key})
		requireNativeStatus(t, response, http.StatusOK)
		var receipt conversation.Receipt
		decodeHubResponse(t, response, &receipt)
		if receipt.Status != conversation.DeliveryQueued {
			t.Fatalf("%s receipt = %#v", key, receipt)
		}
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var total int
		if err := f.service.database.db.QueryRowContext(t.Context(), query, args...).Scan(&total); err != nil {
			t.Fatal(err)
		}
		return total
	}
	before := count("SELECT count(*) FROM conversation_messages WHERE conversation_id = ?", record.ID)
	overflow := conversation.Command{Key: "q4", Kind: conversation.CommandMessage, Text: "Too many"}
	requireNativeError(t, f.command(t, f.token, record.ID, overflow), http.StatusServiceUnavailable, "queue_full")
	if got := count("SELECT count(*) FROM conversation_messages WHERE conversation_id = ?", record.ID); got != before {
		t.Fatalf("messages = %d, want %d (queue_full must not append)", got, before)
	}
	if got := count("SELECT count(*) FROM conversation_commands WHERE conversation_id = ? AND key = 'q4'", record.ID); got != 0 {
		t.Fatal("queue_full must not reserve the key or store a receipt")
	}

	// Drain the queue the way a bound worker's poll does, then retry the key
	// that was rejected: it must be accepted as a first use, not replayed.
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_messages SET delivery = ? WHERE conversation_id = ? AND delivery = ?", conversation.DeliverySent, record.ID, conversation.DeliveryQueued); err != nil {
		t.Fatal(err)
	}
	response := f.command(t, f.token, record.ID, overflow)
	requireNativeStatus(t, response, http.StatusOK)
	var receipt conversation.Receipt
	decodeHubResponse(t, response, &receipt)
	if receipt.Status != conversation.DeliveryQueued || receipt.MessageID == "" {
		t.Fatalf("retry receipt = %#v", receipt)
	}
	if got := count("SELECT count(*) FROM conversation_messages WHERE conversation_id = ? AND command_key = 'q4'", record.ID); got != 1 {
		t.Fatalf("messages for the retried key = %d, want exactly 1", got)
	}
}

// TestConversationLostAcknowledgementStaysUnknown covers a control the
// worker reported as unknown: no later poll may quietly promote it to sent,
// and it is never handed out again.
func TestConversationLostAcknowledgementStaysUnknown(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
	lost := f.queue(t, conversation.MessageText, "lost-1", "Did this arrive?", nil)

	response := f.controls(t, 0, 0)
	requireNativeStatus(t, response, http.StatusOK)
	var page workerControlsResponse
	decodeHubResponse(t, response, &page)
	if len(page.Controls) != 1 || page.Controls[0].MessageID != lost.ID {
		t.Fatalf("controls = %#v", page.Controls)
	}
	requireConversationDelivery(t, f, lost, "lost-1", conversation.DeliverySending)

	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "control_result", "key": "lost-1", "status": "unknown", "error": "connection lost"}), http.StatusAccepted)
	requireConversationDelivery(t, f, lost, "lost-1", conversation.DeliveryUnknown)
	if failure := f.receipt(t, "lost-1").Error; failure == nil || failure.Code != "unknown" {
		t.Fatalf("receipt error = %#v, want the unknown reason", failure)
	}

	// The worker acknowledges the cursor it had already been handed. An
	// acknowledgement must never resurrect a control the worker itself
	// reported as unknown.
	acknowledged := f.controls(t, page.Cursor, 0)
	requireNativeStatus(t, acknowledged, http.StatusOK)
	var second workerControlsResponse
	decodeHubResponse(t, acknowledged, &second)
	if len(second.Controls) != 0 {
		t.Fatalf("controls after an unknown result = %#v, want none", second.Controls)
	}
	requireConversationDelivery(t, f, lost, "lost-1", conversation.DeliveryUnknown)

	// A poll from the start of the log must not re-deliver it either.
	replay := f.controls(t, 0, 0)
	requireNativeStatus(t, replay, http.StatusOK)
	var third workerControlsResponse
	decodeHubResponse(t, replay, &third)
	if len(third.Controls) != 0 {
		t.Fatalf("controls replayed from zero = %#v, want none", third.Controls)
	}
	requireConversationDelivery(t, f, lost, "lost-1", conversation.DeliveryUnknown)
}

// TestConversationHubRestartRecovery covers a hub process restart with work
// in flight: nothing that could not be established survives as live, the
// control that was never handed out is still queued and re-woken, stored
// receipts still answer a replay, and no message is duplicated.
func TestConversationHubRestartRecovery(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "partial"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "Continue?"}}},
	), http.StatusAccepted)
	handed := f.queue(t, conversation.MessageText, "restart-handed", "Handed out", nil)
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	waiting := f.queue(t, conversation.MessageText, "restart-waiting", "Still waiting", nil)
	requireConversationDelivery(t, f, handed, "restart-handed", conversation.DeliverySending)
	requireConversationDelivery(t, f, waiting, "restart-waiting", conversation.DeliveryQueued)
	before := f.messages(t)

	f.restart(t)

	record := f.load(t)
	if record.Execution.Status != conversation.ExecutionUnknown {
		t.Fatalf("execution after restart = %#v, want unknown", record.Execution)
	}
	if record.Execution.Owner.AttemptID != f.attempt {
		t.Fatalf("the owner tuple must survive a restart: %#v", record.Execution.Owner)
	}
	requireConversationDelivery(t, f, handed, "restart-handed", conversation.DeliveryUnknown)
	requireConversationDelivery(t, f, waiting, "restart-waiting", conversation.DeliveryQueued)
	if got := f.questions(t)[0].Status; got != conversation.QuestionExpired {
		t.Fatalf("question status = %q, want expired", got)
	}
	after := f.messages(t)
	if len(after) != len(before) {
		t.Fatalf("messages = %d, want %d (a restart must not duplicate history)", len(after), len(before))
	}
	for _, message := range after {
		if message.Role == conversation.RoleAssistant && message.ProviderItemID == "item-1" {
			if message.Delivery != conversation.DeliveryUnknown || message.Text != "partial" {
				t.Fatalf("streaming assistant message after restart = %#v", message)
			}
		}
	}

	t.Run("a replayed command key returns the stored receipt", func(t *testing.T) {
		tx := f.tx(t)
		stored, conflict, err := f.chat.store.reserveCommand(t.Context(), tx, f.record.ID, "restart-handed", conversation.CommandMessage, "hash-restart-handed", f.service.config.now())
		if err != nil || stored == nil || conflict {
			t.Fatalf("reserveCommand() = %#v, %v, %v", stored, conflict, err)
		}
		if stored.Status != conversation.DeliveryUnknown || stored.MessageID != handed.ID {
			t.Fatalf("stored receipt = %#v", stored)
		}
	})

	t.Run("pending work is woken again", func(t *testing.T) {
		router := f.chat.controls
		recorder := &recordingWaker{}
		f.chat.controls = recorder
		defer func() { f.chat.controls = router }()
		if err := f.chat.wakePending(t.Context()); err != nil {
			t.Fatalf("wakePending() error = %v", err)
		}
		if len(recorder.woken) != 1 || recorder.woken[0] != f.record.ID {
			t.Fatalf("woken = %v, want [%s]", recorder.woken, f.record.ID)
		}
	})

	t.Run("a fresh attempt receives the queued control once", func(t *testing.T) {
		f.release(t)
		f.claim(t)
		response := f.bind(t, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var bound workerBindResponse
		decodeHubResponse(t, response, &bound)
		if len(bound.Pending) != 1 || bound.Pending[0].MessageID != waiting.ID || bound.Pending[0].Expected.AttemptID != f.attempt {
			t.Fatalf("pending = %#v, want only %s for %s", bound.Pending, waiting.ID, f.attempt)
		}
		requireConversationDelivery(t, f, handed, "restart-handed", conversation.DeliveryUnknown)
		// The replacement runner cannot resume the provider thread, so the
		// bind records the transcript hand-off in the history and appends
		// nothing else (decisions section 10.4).
		if got := len(f.messages(t)); got != len(before)+1 {
			t.Fatalf("messages = %d, want %d and the transcript notice", got, len(before)+1)
		}
		requireConversationTranscriptNotice(t, f)
	})
}

// TestConversationWorkerRestartDeliversQueuedControlOnce covers a runner
// restart: the replacement attempt receives the message that is still
// queued exactly once, and never the answer the interrupted attempt was
// already sending.
func TestConversationWorkerRestartDeliversQueuedControlOnce(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "Continue?"}}},
	), http.StatusAccepted)
	question := f.questions(t)[0]
	handed := f.queue(t, conversation.MessageText, "text-1", "Later", nil)
	answer := f.queue(t, conversation.MessageAnswer, "answer-1", "", map[string]any{"question_id": question.ID, "answers": map[string][]string{"q": {"yes"}}})
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	// Accepted after the hand-off, so this one was never given to a worker.
	text := f.queue(t, conversation.MessageText, "text-2", "And this one too", nil)
	requireNativeStatus(t, f.unbind(t, "interrupted"), http.StatusOK)
	requireConversationDelivery(t, f, handed, "text-1", conversation.DeliveryUnknown)
	requireConversationDelivery(t, f, text, "text-2", conversation.DeliveryQueued)
	requireConversationDelivery(t, f, answer, "answer-1", conversation.DeliveryUnknown)
	before := len(f.messages(t))

	f.release(t)
	f.claim(t)
	response := f.bind(t, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var bound workerBindResponse
	decodeHubResponse(t, response, &bound)
	if len(bound.Pending) != 1 || bound.Pending[0].MessageID != text.ID || bound.Pending[0].Kind != "message" {
		t.Fatalf("pending = %#v, want only the queued message %s", bound.Pending, text.ID)
	}
	if bound.Pending[0].Expected.AttemptID != f.attempt {
		t.Fatalf("expected attempt = %q, want %q", bound.Pending[0].Expected.AttemptID, f.attempt)
	}
	requireConversationDelivery(t, f, answer, "answer-1", conversation.DeliveryUnknown)

	acknowledged := f.controls(t, bound.Cursor, 0)
	requireNativeStatus(t, acknowledged, http.StatusOK)
	var page workerControlsResponse
	decodeHubResponse(t, acknowledged, &page)
	if len(page.Controls) != 0 {
		t.Fatalf("controls after the acknowledgement = %#v, want none", page.Controls)
	}
	requireConversationDelivery(t, f, text, "text-2", conversation.DeliverySent)
	requireConversationDelivery(t, f, answer, "answer-1", conversation.DeliveryUnknown)
	requireConversationDelivery(t, f, handed, "text-1", conversation.DeliveryUnknown)
	// The replacement runner was handed a transcript, which the history
	// records; nothing else was appended and no control was duplicated.
	if got := len(f.messages(t)); got != before+1 {
		t.Fatalf("messages = %d, want %d and the transcript notice", got, before+1)
	}
	requireConversationTranscriptNotice(t, f)
}

// requireConversationTranscriptNotice asserts the conversation carries
// exactly one transcript hand-off notice with the contract's copy and that
// the count it names matches the transcript that was actually handed over
// (decisions section 10.4).
func requireConversationTranscriptNotice(t *testing.T, f *conversationWorkerFixture) {
	t.Helper()
	notices := make([]conversationMessageRecord, 0, 1)
	for _, message := range f.messages(t) {
		if message.Role == conversation.RoleSystem && message.Kind == conversation.MessageStatus && strings.HasPrefix(message.Text, "Provider history was not available") {
			notices = append(notices, message)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("transcript notices = %d, want exactly one", len(notices))
	}
	if !strings.HasSuffix(notices[0].Text, " messages") || !strings.Contains(notices[0].Text, "continuing from a transcript of the last ") {
		t.Fatalf("notice = %q, want the contract copy", notices[0].Text)
	}
	if notices[0].Delivery != conversation.DeliverySaved {
		t.Fatalf("notice delivery = %q, want saved", notices[0].Delivery)
	}
}

// TestConversationRetryReQueuesOneMessage covers the explicit retry that
// replaces automatic replay: a control the hub could not establish goes back
// on the ladder under its own message id, the next attempt receives it
// exactly once, a delivered message cannot be retried, and a replayed retry
// key answers with the stored receipt (decisions section 10.3).
func TestConversationRetryReQueuesOneMessage(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
	lost := f.queue(t, conversation.MessageText, "lost-1", "Did this arrive?", nil)
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	requireNativeStatus(t, f.unbind(t, "interrupted"), http.StatusOK)
	requireConversationDelivery(t, f, lost, "lost-1", conversation.DeliveryUnknown)
	before := len(f.messages(t))

	retry := conversation.Command{Key: "retry-1", Kind: conversation.CommandRetry, MessageID: lost.ID}
	response := f.command(t, f.token, retry)
	requireNativeStatus(t, response, http.StatusOK)
	var receipt conversation.Receipt
	decodeHubResponse(t, response, &receipt)
	if receipt.Status != conversation.DeliveryQueued || receipt.MessageID != lost.ID || receipt.Kind != conversation.CommandRetry {
		t.Fatalf("retry receipt = %#v, want queued for %s", receipt, lost.ID)
	}
	stored := f.messages(t)[lost.ID]
	if stored.Delivery != conversation.DeliveryQueued || stored.AttemptID != "" || stored.TurnID != "" {
		t.Fatalf("retried message = %#v, want queued and unowned", stored)
	}
	if stored.UpdatedAt.Before(lost.UpdatedAt) {
		t.Fatalf("updated_at = %s, want at least %s", stored.UpdatedAt, lost.UpdatedAt)
	}
	if !conversationEmittedMessageUpdate(t, f, lost.ID) {
		t.Fatal("a retry must emit message.updated so every tab sees the new delivery")
	}
	if got := len(f.messages(t)); got != before {
		t.Fatalf("messages = %d, want %d (a retry must not duplicate the message)", got, before)
	}

	t.Run("the retry key replays its own receipt", func(t *testing.T) {
		replay := f.command(t, f.token, retry)
		requireNativeStatus(t, replay, http.StatusOK)
		var again conversation.Receipt
		decodeHubResponse(t, replay, &again)
		if again.Key != receipt.Key || again.Status != receipt.Status || again.MessageID != receipt.MessageID {
			t.Fatalf("replayed receipt = %#v, want %#v", again, receipt)
		}
	})

	t.Run("the next attempt is handed the message once", func(t *testing.T) {
		f.release(t)
		f.claim(t)
		response := f.bind(t, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var bound workerBindResponse
		decodeHubResponse(t, response, &bound)
		if len(bound.Pending) != 1 || bound.Pending[0].MessageID != lost.ID {
			t.Fatalf("pending = %#v, want only the retried %s", bound.Pending, lost.ID)
		}
		acknowledged := f.controls(t, bound.Cursor, 0)
		requireNativeStatus(t, acknowledged, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, acknowledged, &page)
		if len(page.Controls) != 0 {
			t.Fatalf("controls after the acknowledgement = %#v, want none", page.Controls)
		}
		if got := f.messages(t)[lost.ID].Delivery; got != conversation.DeliverySent {
			t.Fatalf("delivery after the hand-off = %q, want sent", got)
		}
	})

	t.Run("a message that is not retryable is refused", func(t *testing.T) {
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-2", "turn_id": "turn-2"}), http.StatusAccepted)
		if got := f.messages(t)[lost.ID].Delivery; got != conversation.DeliveryDelivered {
			t.Fatalf("delivery after the turn started = %q, want delivered", got)
		}
		failure := requireNativeError(t, f.command(t, f.token, conversation.Command{Key: "retry-2", Kind: conversation.CommandRetry, MessageID: lost.ID}), http.StatusUnprocessableEntity, "invalid_request")
		if !strings.Contains(failure.Message, "delivered") {
			t.Fatalf("message = %q, want it to name the delivery", failure.Message)
		}
	})

	t.Run("a message from another conversation is refused", func(t *testing.T) {
		requireNativeError(t, f.command(t, f.token, conversation.Command{Key: "retry-3", Kind: conversation.CommandRetry, MessageID: conversation.NewMessageID()}), http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("an assistant message is refused", func(t *testing.T) {
		var assistant string
		for id, message := range f.messages(t) {
			if message.Role == conversation.RoleAssistant {
				assistant = id
			}
		}
		if assistant == "" {
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "hello"}), http.StatusAccepted)
			for id, message := range f.messages(t) {
				if message.Role == conversation.RoleAssistant {
					assistant = id
				}
			}
		}
		requireNativeError(t, f.command(t, f.token, conversation.Command{Key: "retry-4", Kind: conversation.CommandRetry, MessageID: assistant}), http.StatusUnprocessableEntity, "invalid_request")
	})
}

// conversationEmittedMessageUpdate reports whether the conversation's event
// log carries a message.updated frame naming the message.
func conversationEmittedMessageUpdate(t *testing.T, f *conversationWorkerFixture, messageID string) bool {
	t.Helper()
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM conversation_events WHERE conversation_id = ? AND type = ? AND json_extract(body_json, '$.id') = ?",
		f.record.ID, conversation.EventMessageUpdated, messageID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}
