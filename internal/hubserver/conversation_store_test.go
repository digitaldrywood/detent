package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationFixture struct {
	nativeFixture
	store        *conversationStore
	organization tracker.OrganizationID
	owner        string
	other        string
}

func newConversationFixture(t *testing.T) conversationFixture {
	t.Helper()
	f := newNativeFixture(t, nil, "", "conversation")
	organization := f.project.OrganizationID
	principal := func(name string) string {
		response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "principal-" + name, "scope": "operator"})
		requireNativeStatus(t, response, http.StatusCreated)
		var token tokenResponse
		decodeHubResponse(t, response, &token)
		return token.ID
	}
	return conversationFixture{
		nativeFixture: f,
		store:         newConversationStore(f.service.database.db),
		organization:  organization,
		owner:         principal("owner"),
		other:         principal("other"),
	}
}

func (f conversationFixture) tx(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func (f conversationFixture) commit(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func (f conversationFixture) record(owner, title string, at time.Time) conversationRecord {
	return conversationRecord{
		ID:               conversation.NewConversationID(),
		OrganizationID:   f.organization,
		ProjectID:        f.project.ID,
		OwnerPrincipalID: owner,
		OwnerSubject:     "user@example.test",
		Title:            title,
		Visibility:       conversation.VisibilityPrivate,
		Status:           conversation.StatusActive,
		Execution:        conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: at},
		CreatedAt:        at,
		UpdatedAt:        at,
	}
}

func (f conversationFixture) create(t *testing.T, record conversationRecord) conversationRecord {
	t.Helper()
	tx := f.tx(t)
	if err := f.store.createConversation(t.Context(), tx, &record); err != nil {
		t.Fatalf("createConversation() error = %v", err)
	}
	f.commit(t, tx)
	return record
}

func TestConversationStoreCreateReadAndList(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	base := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	older := f.create(t, f.record(f.owner, "older", base))
	newer := f.create(t, f.record(f.owner, "newer", base.Add(time.Minute)))
	foreign := f.create(t, f.record(f.other, "foreign private", base.Add(2*time.Minute)))
	shared := f.record(f.other, "shared", base.Add(3*time.Minute))
	shared.Visibility = conversation.VisibilityShared
	shared = f.create(t, shared)
	settled := f.record(f.owner, "settled", base.Add(4*time.Minute))
	settled.Status = conversation.StatusSettled
	settledAt := base.Add(4 * time.Minute)
	settled.SettledAt = &settledAt
	settled = f.create(t, settled)

	t.Run("read", func(t *testing.T) {
		tx := f.tx(t)
		got, err := f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, newer.ID)
		if err != nil {
			t.Fatalf("readConversation() error = %v", err)
		}
		if got.ID != newer.ID || got.Title != "newer" || got.Revision != 1 || got.EventSeq != 0 || got.OwnerPrincipalID != f.owner || got.Visibility != conversation.VisibilityPrivate || got.Status != conversation.StatusActive {
			t.Fatalf("readConversation() = %#v", got)
		}
		if !got.CreatedAt.Equal(newer.CreatedAt) || got.Execution.Status != conversation.ExecutionIdle || got.LastMessageAt != nil || got.WorkItemID != "" || got.LinkedAt != nil {
			t.Fatalf("readConversation() = %#v", got)
		}
	})
	t.Run("read missing is not found", func(t *testing.T) {
		tx := f.tx(t)
		_, err := f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, conversation.NewConversationID())
		var failure *nativeError
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			t.Fatalf("readConversation() error = %v, want not_found", err)
		}
	})
	t.Run("read across project is not found", func(t *testing.T) {
		tx := f.tx(t)
		_, err := f.store.readConversation(t.Context(), tx, f.organization, tracker.ProjectID("proj_other"), newer.ID)
		var failure *nativeError
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			t.Fatalf("readConversation() error = %v, want not_found", err)
		}
	})
	t.Run("list for owner", func(t *testing.T) {
		tx := f.tx(t)
		got, next, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if next != "" {
			t.Fatalf("next cursor = %q, want empty", next)
		}
		// Settled sorts after every active conversation, however recent its
		// own activity is (decisions section 14).
		wantIDs := []string{shared.ID, newer.ID, older.ID, settled.ID}
		if len(got) != len(wantIDs) {
			t.Fatalf("listConversations() returned %d conversations, want %d: %#v", len(got), len(wantIDs), got)
		}
		for i, want := range wantIDs {
			if got[i].ID != want {
				t.Fatalf("listConversations()[%d] = %q, want %q", i, got[i].ID, want)
			}
			if got[i].ID == foreign.ID {
				t.Fatal("foreign private conversation must not be visible")
			}
		}
	})
	t.Run("list for other principal", func(t *testing.T) {
		tx := f.tx(t)
		got, _, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.other, Limit: 10})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(got) != 2 || got[0].ID != shared.ID || got[1].ID != foreign.ID {
			t.Fatalf("listConversations() = %#v", got)
		}
	})
	t.Run("list filters by settled", func(t *testing.T) {
		tx := f.tx(t)
		value := true
		got, _, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10, Settled: &value})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(got) != 1 || got[0].ID != settled.ID {
			t.Fatalf("listConversations(settled) = %#v", got)
		}
		value = false
		got, _, err = f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10, Settled: &value})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("listConversations(active) = %#v", got)
		}
	})
	// A page boundary that falls between the active and the settled group
	// still resumes in order: the cursor carries the group.
	t.Run("cursor crosses the settled boundary", func(t *testing.T) {
		tx := f.tx(t)
		first, next, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 3})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(first) != 3 || next == "" {
			t.Fatalf("first page = %#v next = %q", first, next)
		}
		second, last, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 3, Cursor: next})
		if err != nil {
			t.Fatalf("listConversations(cursor) error = %v", err)
		}
		if len(second) != 1 || second[0].ID != settled.ID || last != "" {
			t.Fatalf("second page = %#v last = %q", second, last)
		}
	})
	t.Run("list across projects", func(t *testing.T) {
		tx := f.tx(t)
		got, _, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Principal: f.owner, Limit: 10})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("listConversations() returned %d conversations, want 4", len(got))
		}
	})
	t.Run("list pages with cursor", func(t *testing.T) {
		tx := f.tx(t)
		first, next, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 2})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(first) != 2 || next == "" {
			t.Fatalf("first page = %d rows, cursor %q", len(first), next)
		}
		second, last, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Cursor: next, Limit: 2})
		if err != nil {
			t.Fatalf("listConversations(cursor) error = %v", err)
		}
		if len(second) != 2 || second[0].ID != older.ID || second[1].ID != settled.ID || last != "" {
			t.Fatalf("second page = %#v, cursor %q", second, last)
		}
		if _, _, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Cursor: "not-a-cursor", Limit: 2}); err == nil {
			t.Fatal("invalid cursor must be rejected")
		}
	})
	t.Run("last message orders before updated", func(t *testing.T) {
		tx := f.tx(t)
		message := conversationMessageRecord{ConversationID: older.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "bump", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)}
		if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
			t.Fatalf("appendMessage() error = %v", err)
		}
		got, _, err := f.store.listConversations(t.Context(), tx, conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if got[0].ID != older.ID || got[0].LastMessageAt == nil || !got[0].LastMessageAt.Equal(base.Add(time.Hour)) {
			t.Fatalf("listConversations()[0] = %#v, want %q first", got[0], older.ID)
		}
	})
}

func TestConversationStoreUpdateRevision(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "title", now))

	tx := f.tx(t)
	record.Title = "renamed"
	record.UpdatedAt = now.Add(time.Second)
	if err := f.store.updateConversation(t.Context(), tx, &record, 1); err != nil {
		t.Fatalf("updateConversation() error = %v", err)
	}
	if record.Revision != 2 {
		t.Fatalf("revision = %d, want 2", record.Revision)
	}
	got, err := f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if got.Title != "renamed" || got.Revision != 2 {
		t.Fatalf("readConversation() = %#v", got)
	}
	stale := got
	stale.Title = "stale"
	err = f.store.updateConversation(t.Context(), tx, &stale, 1)
	var revision *errConversationRevision
	if !errors.As(err, &revision) || revision.Current != 2 {
		t.Fatalf("updateConversation(stale) error = %v, want revision conflict at 2", err)
	}
	missing := got
	missing.ID = conversation.NewConversationID()
	err = f.store.updateConversation(t.Context(), tx, &missing, 2)
	var failure *nativeError
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("updateConversation(missing) error = %v, want not_found", err)
	}
	f.commit(t, tx)
}

func TestConversationStoreLinkedIssueIsUnique(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	issue := f.nativeFixture.create(t, "linked")
	first := f.create(t, f.record(f.owner, "first", now))
	second := f.create(t, f.record(f.owner, "second", now))

	tx := f.tx(t)
	first.WorkItemID = string(issue.WorkItemID)
	linkedAt := now.Add(time.Second)
	first.LinkedAt = &linkedAt
	first.Visibility = conversation.VisibilityShared
	if err := f.store.updateConversation(t.Context(), tx, &first, 1); err != nil {
		t.Fatalf("updateConversation(link) error = %v", err)
	}
	second.WorkItemID = string(issue.WorkItemID)
	second.LinkedAt = &linkedAt
	err := f.store.updateConversation(t.Context(), tx, &second, 1)
	var linked *errConversationLinked
	if !errors.As(err, &linked) || linked.ExistingConversationID != first.ID {
		t.Fatalf("updateConversation(second link) error = %v, want conversation_already_linked with %q", err, first.ID)
	}
	fresh := f.record(f.owner, "fresh", now)
	fresh.WorkItemID = string(issue.WorkItemID)
	fresh.LinkedAt = &linkedAt
	err = f.store.createConversation(t.Context(), tx, &fresh)
	if !errors.As(err, &linked) || linked.ExistingConversationID != first.ID {
		t.Fatalf("createConversation(linked) error = %v, want conversation_already_linked", err)
	}
	unknown := f.record(f.owner, "unknown issue", now)
	unknown.WorkItemID = "wi_missing"
	unknown.LinkedAt = &linkedAt
	err = f.store.createConversation(t.Context(), tx, &unknown)
	var failure *nativeError
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("createConversation(unknown issue) error = %v, want not_found", err)
	}
	f.commit(t, tx)

	tx = f.tx(t)
	got, err := f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, first.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if got.WorkItemID != string(issue.WorkItemID) || got.LinkedAt == nil || got.Visibility != conversation.VisibilityShared {
		t.Fatalf("readConversation() = %#v", got)
	}
	// The partial unique index is a backstop for direct writes.
	if _, err := tx.ExecContext(t.Context(), "UPDATE conversations SET work_item_id = ? WHERE id = ?", issue.WorkItemID, second.ID); err == nil {
		t.Fatal("partial unique index must reject a second link")
	}
}

func TestConversationStoreMessagesSequenceUnderConcurrency(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "messages", now))
	const writers = 20
	var wg sync.WaitGroup
	failures := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			tx, err := f.service.database.db.BeginTx(ctx, nil)
			if err != nil {
				failures <- err
				return
			}
			defer func() { _ = tx.Rollback() }()
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "message " + strconv.Itoa(i), Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CommandKey: "key-" + strconv.Itoa(i), CreatedAt: now.Add(time.Duration(i) * time.Millisecond), UpdatedAt: now}
			if err := f.store.appendMessage(ctx, tx, &message); err != nil {
				failures <- err
				return
			}
			if message.ID == "" || message.Seq <= 0 {
				failures <- errors.New("appendMessage must allocate id and seq")
				return
			}
			if err := tx.Commit(); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent appendMessage error = %v", err)
	}
	tx := f.tx(t)
	messages, err := f.store.listMessages(t.Context(), tx, record.ID, 0, 100)
	if err != nil {
		t.Fatalf("listMessages() error = %v", err)
	}
	if len(messages) != writers {
		t.Fatalf("listMessages() returned %d, want %d", len(messages), writers)
	}
	for i, message := range messages {
		if message.Seq != int64(i+1) {
			t.Fatalf("messages[%d].Seq = %d, want %d", i, message.Seq, i+1)
		}
		if message.Role != conversation.RoleUser || message.Kind != conversation.MessageText || message.Delivery != conversation.DeliverySaved || message.Actor.PrincipalID != f.owner || message.CommandKey == "" || string(message.Data) != "{}" {
			t.Fatalf("messages[%d] = %#v", i, message)
		}
	}
	page, err := f.store.listMessages(t.Context(), tx, record.ID, 11, 5)
	if err != nil {
		t.Fatalf("listMessages(before) error = %v", err)
	}
	if len(page) != 5 || page[0].Seq != 6 || page[4].Seq != 10 {
		t.Fatalf("listMessages(before=11, limit=5) = %v", seqs(page))
	}
	updated := messages[0]
	updated.Delivery = conversation.DeliveryDelivered
	updated.AttemptID = "att_1"
	updated.TurnID = "turn_1"
	updated.ProviderItemID = "item_1"
	updated.Text = "edited"
	updated.UpdatedAt = now.Add(time.Hour)
	if err := f.store.updateMessage(t.Context(), tx, updated); err != nil {
		t.Fatalf("updateMessage() error = %v", err)
	}
	again, err := f.store.listMessages(t.Context(), tx, record.ID, 2, 1)
	if err != nil {
		t.Fatalf("listMessages() error = %v", err)
	}
	if len(again) != 1 || again[0].Delivery != conversation.DeliveryDelivered || again[0].AttemptID != "att_1" || again[0].TurnID != "turn_1" || again[0].ProviderItemID != "item_1" || again[0].Text != "edited" || !again[0].UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("updated message = %#v", again)
	}
	got, err := f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if got.LastMessageAt == nil {
		t.Fatal("appendMessage must record last_message_at")
	}
	missing := updated
	missing.ID = conversation.NewMessageID()
	if err := f.store.updateMessage(t.Context(), tx, missing); err == nil {
		t.Fatal("updateMessage(missing) must fail")
	}
}

func seqs(messages []conversationMessageRecord) []int64 {
	out := make([]int64, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Seq)
	}
	return out
}

func TestConversationStoreQuestions(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "questions", now))
	tx := f.tx(t)
	message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageStatus, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "runner"}, CreatedAt: now, UpdatedAt: now}
	if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
		t.Fatalf("appendMessage() error = %v", err)
	}
	expires := now.Add(24 * time.Hour)
	question := conversationQuestionRecord{
		Question: conversation.Question{
			ID:             conversation.NewQuestionID(),
			ConversationID: record.ID,
			MessageID:      message.ID,
			Status:         conversation.QuestionPending,
			Owner:          conversation.Owner{AttemptID: "att_1", ThreadID: "thread_1", TurnID: "turn_1"},
			Prompts:        []conversation.Prompt{{ID: "p", Question: "Q?", Options: []conversation.Option{{Label: "yes"}}, FreeText: true}},
			ExpiresAt:      &expires,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		RequestID: "req_1",
	}
	if err := f.store.upsertQuestion(t.Context(), tx, question); err != nil {
		t.Fatalf("upsertQuestion() error = %v", err)
	}
	pending, err := f.store.listPendingQuestions(t.Context(), tx, record.ID)
	if err != nil {
		t.Fatalf("listPendingQuestions() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != question.ID || pending[0].RequestID != "req_1" || pending[0].Owner.AttemptID != "att_1" || pending[0].Owner.TurnID != "turn_1" || len(pending[0].Prompts) != 1 || pending[0].ExpiresAt == nil || !pending[0].ExpiresAt.Equal(expires) {
		t.Fatalf("listPendingQuestions() = %#v", pending)
	}
	question.Status = conversation.QuestionAnswered
	question.Answers = map[string][]string{"p": {"yes"}}
	question.AnsweredBy = f.owner
	question.UpdatedAt = now.Add(time.Minute)
	if err := f.store.upsertQuestion(t.Context(), tx, question); err != nil {
		t.Fatalf("upsertQuestion(update) error = %v", err)
	}
	got, err := f.store.readQuestion(t.Context(), tx, record.ID, question.ID)
	if err != nil {
		t.Fatalf("readQuestion() error = %v", err)
	}
	if got.Status != conversation.QuestionAnswered || len(got.Answers["p"]) != 1 || got.Answers["p"][0] != "yes" || got.AnsweredBy != f.owner || !got.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("readQuestion() = %#v", got)
	}
	pending, err = f.store.listPendingQuestions(t.Context(), tx, record.ID)
	if err != nil {
		t.Fatalf("listPendingQuestions() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("answered question still pending: %#v", pending)
	}
	_, err = f.store.readQuestion(t.Context(), tx, record.ID, conversation.NewQuestionID())
	var failure *nativeError
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("readQuestion(missing) error = %v, want not_found", err)
	}
	_, err = f.store.readQuestion(t.Context(), tx, conversation.NewConversationID(), question.ID)
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("readQuestion(wrong conversation) error = %v, want not_found", err)
	}
}

func TestConversationStoreCommandsAndReceipts(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "commands", now))
	second := f.create(t, f.record(f.owner, "second", now))
	command := conversation.Command{Key: "cmd-1", Kind: conversation.CommandMessage, Text: "hello"}
	hash := conversation.RequestHash(command)

	tx := f.tx(t)
	existing, conflict, err := f.store.reserveCommand(t.Context(), tx, record.ID, command.Key, command.Kind, hash, now)
	if err != nil || existing != nil || conflict {
		t.Fatalf("reserveCommand(first) = %#v, %v, %v", existing, conflict, err)
	}
	receipt := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliveryQueued, MessageID: "msg_x", UpdatedAt: now.Add(time.Second)}
	if err := f.store.updateReceipt(t.Context(), tx, record.ID, command.Key, receipt); err != nil {
		t.Fatalf("updateReceipt() error = %v", err)
	}
	existing, conflict, err = f.store.reserveCommand(t.Context(), tx, record.ID, command.Key, command.Kind, hash, now.Add(time.Minute))
	if err != nil || conflict || existing == nil {
		t.Fatalf("reserveCommand(replay) = %#v, %v, %v", existing, conflict, err)
	}
	if existing.Status != conversation.DeliveryQueued || existing.MessageID != "msg_x" || !existing.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("replayed receipt = %#v", existing)
	}
	other := command
	other.Text = "different"
	existing, conflict, err = f.store.reserveCommand(t.Context(), tx, record.ID, command.Key, command.Kind, conversation.RequestHash(other), now)
	if err != nil || !conflict || existing == nil || existing.Key != command.Key {
		t.Fatalf("reserveCommand(conflict) = %#v, %v, %v", existing, conflict, err)
	}
	if err := f.store.updateReceipt(t.Context(), tx, record.ID, "missing", receipt); err == nil {
		t.Fatal("updateReceipt(missing) must fail")
	}
	// The same key in another conversation is independent.
	existing, conflict, err = f.store.reserveCommand(t.Context(), tx, second.ID, command.Key, command.Kind, hash, now)
	if err != nil || existing != nil || conflict {
		t.Fatalf("reserveCommand(other conversation) = %#v, %v, %v", existing, conflict, err)
	}
	f.commit(t, tx)
}

func TestConversationStoreEvents(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "events", now))

	tx := f.tx(t)
	seq, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventMessageDelta, map[string]any{"message_id": "msg_1", "seq": 1, "text": "hel"}, now)
	if err != nil || seq != 1 {
		t.Fatalf("appendEvent() = %d, %v", seq, err)
	}
	seq, err = f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventHeartbeat, map[string]any{"seq": 1}, now)
	if err != nil || seq != 2 {
		t.Fatalf("appendEvent() = %d, %v", seq, err)
	}
	got, err := f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if got.EventSeq != 2 {
		t.Fatalf("event_seq = %d, want 2", got.EventSeq)
	}
	events, err := f.store.listEvents(t.Context(), tx, record.ID, 0, 10)
	if err != nil {
		t.Fatalf("listEvents() error = %v", err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[0].Type != conversation.EventMessageDelta || events[1].Seq != 2 || string(events[0].Data) != `{"message_id":"msg_1","seq":1,"text":"hel"}` {
		t.Fatalf("listEvents() = %#v", events)
	}
	events, err = f.store.listEvents(t.Context(), tx, record.ID, 1, 10)
	if err != nil {
		t.Fatalf("listEvents(after) error = %v", err)
	}
	if len(events) != 1 || events[0].Seq != 2 {
		t.Fatalf("listEvents(after=1) = %#v", events)
	}
	if _, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventType("bogus"), nil, now); err == nil {
		t.Fatal("appendEvent(bogus type) must fail")
	}
	if _, err := f.store.appendEvent(t.Context(), tx, conversation.NewConversationID(), conversation.EventHeartbeat, nil, now); err == nil {
		t.Fatal("appendEvent(missing conversation) must fail")
	}
	// A rolled back transaction leaves neither the event nor the counter.
	_ = tx.Rollback()
	tx = f.tx(t)
	got, err = f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	events, err = f.store.listEvents(t.Context(), tx, record.ID, 0, 10)
	if err != nil {
		t.Fatalf("listEvents() error = %v", err)
	}
	if got.EventSeq != 0 || len(events) != 0 {
		t.Fatalf("rollback left event_seq=%d events=%d", got.EventSeq, len(events))
	}
}

func TestConversationStoreStartsAndAudience(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "starts", now))
	other := f.create(t, f.record(f.owner, "other", now))

	tx := f.tx(t)
	if err := f.store.recordStart(t.Context(), tx, "att_1", record.ID, now); err != nil {
		t.Fatalf("recordStart() error = %v", err)
	}
	err := f.store.recordStart(t.Context(), tx, "att_1", other.ID, now)
	var started *errConversationStarted
	if !errors.As(err, &started) || started.ConversationID != record.ID || started.AttemptID != "att_1" {
		t.Fatalf("recordStart(duplicate) error = %v, want started conflict for %q", err, record.ID)
	}
	if err := f.store.recordStart(t.Context(), tx, "att_2", record.ID, now); err != nil {
		t.Fatalf("recordStart(second attempt) error = %v", err)
	}
	if err := f.store.recordAudience(t.Context(), tx, record.ID, f.owner, conversation.VisibilityPrivate, conversation.VisibilityShared, now); err != nil {
		t.Fatalf("recordAudience() error = %v", err)
	}
	var count int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_audience_events WHERE conversation_id = ? AND from_visibility = 'private' AND to_visibility = 'shared' AND actor_principal_id = ?", record.ID, f.owner).Scan(&count); err != nil {
		t.Fatalf("count audience events: %v", err)
	}
	if count != 1 {
		t.Fatalf("audience events = %d, want 1", count)
	}
	f.commit(t, tx)
}

func TestConversationStoreNormalizeAfterRestart(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	type row struct {
		execution conversation.ExecutionStatus
		delivery  conversation.Delivery
		question  conversation.QuestionStatus
		receipt   conversation.Delivery
	}
	tests := []struct {
		name string
		in   row
		want row
	}{
		{name: "in flight becomes unknown", in: row{conversation.ExecutionRunning, conversation.DeliverySending, conversation.QuestionSending, conversation.DeliverySending}, want: row{conversation.ExecutionUnknown, conversation.DeliveryUnknown, conversation.QuestionUnknown, conversation.DeliveryUnknown}},
		{name: "pending expires and responding unknown", in: row{conversation.ExecutionWaitingInput, conversation.DeliveryResponding, conversation.QuestionPending, conversation.DeliveryResponding}, want: row{conversation.ExecutionUnknown, conversation.DeliveryUnknown, conversation.QuestionExpired, conversation.DeliveryUnknown}},
		{name: "queued and terminal untouched", in: row{conversation.ExecutionWaitingForRunner, conversation.DeliveryQueued, conversation.QuestionAnswered, conversation.DeliveryQueued}, want: row{conversation.ExecutionWaitingForRunner, conversation.DeliveryQueued, conversation.QuestionAnswered, conversation.DeliveryQueued}},
		{name: "idle and completed untouched", in: row{conversation.ExecutionIdle, conversation.DeliveryCompleted, conversation.QuestionSent, conversation.DeliveryDelivered}, want: row{conversation.ExecutionIdle, conversation.DeliveryCompleted, conversation.QuestionSent, conversation.DeliveryDelivered}},
	}
	ids := make([]string, len(tests))
	for i, test := range tests {
		record := f.record(f.owner, test.name, now)
		record.Execution = conversation.Execution{Status: test.in.execution, Owner: conversation.Owner{AttemptID: "att_" + strconv.Itoa(i)}, Capabilities: conversation.Capabilities{Steer: true, Answer: true}, UpdatedAt: now}
		record = f.create(t, record)
		ids[i] = record.ID
		tx := f.tx(t)
		message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "m", Delivery: test.in.delivery, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CommandKey: "k", CreatedAt: now, UpdatedAt: now}
		if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
			t.Fatalf("appendMessage() error = %v", err)
		}
		question := conversationQuestionRecord{Question: conversation.Question{ID: conversation.NewQuestionID(), ConversationID: record.ID, MessageID: message.ID, Status: test.in.question, Prompts: []conversation.Prompt{{ID: "p", Question: "?"}}, CreatedAt: now, UpdatedAt: now}}
		if err := f.store.upsertQuestion(t.Context(), tx, question); err != nil {
			t.Fatalf("upsertQuestion() error = %v", err)
		}
		if _, _, err := f.store.reserveCommand(t.Context(), tx, record.ID, "k", conversation.CommandMessage, "hash", now); err != nil {
			t.Fatalf("reserveCommand() error = %v", err)
		}
		if err := f.store.updateReceipt(t.Context(), tx, record.ID, "k", conversation.Receipt{Key: "k", Kind: conversation.CommandMessage, Status: test.in.receipt, MessageID: message.ID, UpdatedAt: now}); err != nil {
			t.Fatalf("updateReceipt() error = %v", err)
		}
		f.commit(t, tx)
	}
	if _, err := f.store.normalizeAfterRestart(t.Context(), now.Add(time.Hour)); err != nil {
		t.Fatalf("normalizeAfterRestart() error = %v", err)
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx := f.tx(t)
			got, err := f.store.readConversation(t.Context(), tx, f.organization, f.project.ID, ids[i])
			if err != nil {
				t.Fatalf("readConversation() error = %v", err)
			}
			if got.Execution.Status != test.want.execution {
				t.Fatalf("execution = %q, want %q", got.Execution.Status, test.want.execution)
			}
			if got.Execution.Owner.AttemptID != "att_"+strconv.Itoa(i) {
				t.Fatalf("owner must be preserved: %#v", got.Execution.Owner)
			}
			changed := test.want.execution != test.in.execution
			if changed && (got.Revision != 2 || got.Execution.Capabilities != (conversation.Capabilities{})) {
				t.Fatalf("normalized conversation = %#v, want revision 2 and no capabilities", got)
			}
			if !changed && got.Revision != 1 {
				t.Fatalf("untouched conversation revision = %d, want 1", got.Revision)
			}
			messages, err := f.store.listMessages(t.Context(), tx, ids[i], 0, 10)
			if err != nil {
				t.Fatalf("listMessages() error = %v", err)
			}
			if len(messages) != 1 || messages[0].Delivery != test.want.delivery {
				t.Fatalf("messages = %#v, want delivery %q", messages, test.want.delivery)
			}
			var status string
			if err := tx.QueryRowContext(t.Context(), "SELECT status FROM conversation_questions WHERE conversation_id = ?", ids[i]).Scan(&status); err != nil {
				t.Fatalf("read question status: %v", err)
			}
			if conversation.QuestionStatus(status) != test.want.question {
				t.Fatalf("question status = %q, want %q", status, test.want.question)
			}
			existing, _, err := f.store.reserveCommand(t.Context(), tx, ids[i], "k", conversation.CommandMessage, "hash", now)
			if err != nil || existing == nil {
				t.Fatalf("reserveCommand() = %#v, %v", existing, err)
			}
			if existing.Status != test.want.receipt {
				t.Fatalf("receipt status = %q, want %q", existing.Status, test.want.receipt)
			}
		})
	}
}

func TestConversationMigrationTables(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	if service.database.schemaVersion != supportedSchemaVersion {
		t.Fatalf("schema version = %d, want %d", service.database.schemaVersion, supportedSchemaVersion)
	}
	for _, table := range []string{"conversations", "conversation_messages", "conversation_questions", "conversation_commands", "conversation_events", "conversation_starts", "conversation_audience_events", "coordinator_items", "message_references", "conversation_attachments", "conversation_attachment_blobs", "attempt_usage", "attempt_diffs", "attempt_diff_files", "pull_request_actions"} {
		var name string
		if err := service.database.db.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
	for _, index := range []string{"conversations_work_item_idx", "conversations_owner_activity_idx", "coordinator_items_open_idx"} {
		var name string
		if err := service.database.db.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", index).Scan(&name); err != nil {
			t.Fatalf("index %s: %v", index, err)
		}
	}
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO conversations (id, organization_id, project_id, owner_principal_id, owner_subject, title, visibility, status, execution_json, created_at, updated_at) VALUES ('conv_x', 'org', 'proj', 'tok', '', 't', 'public', 'active', '{}', 'now', 'now')"); err == nil {
		t.Fatal("visibility CHECK must reject unknown values")
	}
}
