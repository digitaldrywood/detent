package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationFixture struct {
	nativeFixture
	store        *conversationStore
	organization tracker.OrganizationID
	owner        string
	ownerToken   string
	other        string
}

func newConversationFixture(t *testing.T) conversationFixture {
	t.Helper()
	clock := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{Conversation: &ConversationConfig{Enabled: true, ArchiveContents: &conversationContentFixture{}}, Clock: func() time.Time { return clock }})
	f.service.registerConversationRoutes(f.service.echo)
	owner := conversationFixturePrincipal(t, f, "owner")
	other := conversationFixturePrincipal(t, f, "other")
	return conversationFixture{nativeFixture: f, store: f.service.conversations.Store, organization: f.project.OrganizationID, owner: owner.ID, ownerToken: owner.Token, other: other.ID}
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
	if err := f.createRecord(t.Context(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func prepareRecordTestPayload(t *testing.T, f conversationFixture, record conversationRecord, values ...[]byte) context.Context {
	t.Helper()
	contents := make([][]byte, 0, len(values))
	for _, raw := range values {
		content, err := messageContent(conversationMessageRecord{Data: raw})
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, content)
	}
	ctx, err := f.store.PreparePayloads(t.Context(), string(record.OrganizationID), contents...)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func prepareTestConversationRecord(t *testing.T, store *conversationStore, record conversationRecord) context.Context {
	t.Helper()
	raw, err := recordPayload(record)
	if err != nil {
		t.Fatal(err)
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.PreparePayloads(t.Context(), string(record.OrganizationID), content)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
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
	issue := f.nativeFixture.create(t, "linked user chat")
	shared.WorkItemID, shared.LinkedAt = string(issue.WorkItemID), &base
	if err := f.updateConversation(t, t.Context(), &shared, shared.Revision); err != nil {
		t.Fatal(err)
	}
	worker := f.record(f.owner, "worker session", base.Add(time.Hour))
	worker.Origin, worker.Visibility = conversationOriginWorker, conversation.VisibilityShared
	workerIssue := f.nativeFixture.create(t, "runner issue")
	worker.WorkItemID, worker.LinkedAt = string(workerIssue.WorkItemID), &base
	worker = f.create(t, worker)
	privateWorker := f.record(f.owner, "private worker session", base.Add(2*time.Hour))
	privateWorker.Origin = conversationOriginWorker
	privateWorker = f.create(t, privateWorker)
	settled := f.record(f.owner, "settled", base.Add(4*time.Minute))
	settled.Status = conversation.StatusSettled
	settledAt := base.Add(4 * time.Minute)
	settled.SettledAt = &settledAt
	settled = f.create(t, settled)
	for _, scope := range []struct {
		name    string
		project tracker.ProjectID
	}{
		{"project", f.project.ID},
		{"organization", ""},
	} {
		t.Run(scope.name+" lists only user origins", func(t *testing.T) {
			got, _, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: scope.project, Principal: f.owner, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				record conversationRecord
				listed bool
			}{
				{newer, true}, {shared, true}, {worker, false}, {privateWorker, false},
			} {
				t.Run(test.record.Title, func(t *testing.T) {
					listed := false
					for _, record := range got {
						listed = listed || record.ID == test.record.ID
					}
					if listed != test.listed {
						t.Fatalf("listed = %v, want %v", listed, test.listed)
					}
					read, err := f.readConversation(t.Context(), f.project.ID, test.record.ID)
					if err != nil || read.Origin != test.record.Origin {
						t.Fatalf("direct read = %#v, %v", read, err)
					}
				})
			}
		})
	}

	t.Run("read", func(t *testing.T) {
		got, err := f.readConversation(t.Context(), f.project.ID, newer.ID)
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
		_, err := f.readConversation(t.Context(), f.project.ID, conversation.NewConversationID())
		var failure *hubapi.NativeError
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			t.Fatalf("readConversation() error = %v, want not_found", err)
		}
	})
	t.Run("read across project is not found", func(t *testing.T) {
		_, err := f.readConversation(t.Context(), tracker.ProjectID("proj_other"), newer.ID)
		var failure *hubapi.NativeError
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			t.Fatalf("readConversation() error = %v, want not_found", err)
		}
	})
	t.Run("list for owner", func(t *testing.T) {
		got, next, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if next != "" {
			t.Fatalf("next cursor = %q, want empty", next)
		}
		// Settled sorts after every active conversation, however recent its
		// own activity is.
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
		got, _, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.other, Limit: 10})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(got) != 2 || got[0].ID != shared.ID || got[1].ID != foreign.ID {
			t.Fatalf("listConversations() = %#v", got)
		}
	})
	t.Run("list filters by settled", func(t *testing.T) {
		value := true
		got, _, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10, Settled: &value})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(got) != 1 || got[0].ID != settled.ID {
			t.Fatalf("listConversations(settled) = %#v", got)
		}
		value = false
		got, _, err = f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10, Settled: &value})
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
		first, next, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 3})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(first) != 3 || next == "" {
			t.Fatalf("first page = %#v next = %q", first, next)
		}
		second, last, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 3, Cursor: next})
		if err != nil {
			t.Fatalf("listConversations(cursor) error = %v", err)
		}
		if len(second) != 1 || second[0].ID != settled.ID || last != "" {
			t.Fatalf("second page = %#v last = %q", second, last)
		}
	})
	t.Run("list across projects", func(t *testing.T) {
		got, _, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Principal: f.owner, Limit: 10})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("listConversations() returned %d conversations, want 4", len(got))
		}
	})
	t.Run("list pages with cursor", func(t *testing.T) {
		first, next, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 2})
		if err != nil {
			t.Fatalf("listConversations() error = %v", err)
		}
		if len(first) != 2 || next == "" {
			t.Fatalf("first page = %d rows, cursor %q", len(first), next)
		}
		second, last, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Cursor: next, Limit: 2})
		if err != nil {
			t.Fatalf("listConversations(cursor) error = %v", err)
		}
		if len(second) != 2 || second[0].ID != older.ID || second[1].ID != settled.ID || last != "" {
			t.Fatalf("second page = %#v, cursor %q", second, last)
		}
		if _, _, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Cursor: "not-a-cursor", Limit: 2}); err == nil {
			t.Fatal("invalid cursor must be rejected")
		}
	})
	t.Run("last message orders before updated", func(t *testing.T) {
		message := conversationMessageRecord{ConversationID: older.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "bump", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)}
		if err := f.appendMessage(t, t.Context(), &message); err != nil {
			t.Fatalf("appendMessage() error = %v", err)
		}
		got, _, err := f.listConversations(t.Context(), conversationListQuery{Organization: f.organization, Project: f.project.ID, Principal: f.owner, Limit: 10})
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

	record.Title = "renamed"
	record.UpdatedAt = now.Add(time.Second)
	if err := f.updateConversation(t, t.Context(), &record, 1); err != nil {
		t.Fatalf("updateConversation() error = %v", err)
	}
	if record.Revision != 2 {
		t.Fatalf("revision = %d, want 2", record.Revision)
	}
	got, err := f.readConversation(t.Context(), f.project.ID, record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if got.Title != "renamed" || got.Revision != 2 {
		t.Fatalf("readConversation() = %#v", got)
	}
	stale := got
	stale.Title = "stale"
	err = f.updateConversation(t, t.Context(), &stale, 1)
	current, readErr := f.readConversation(t.Context(), f.project.ID, record.ID)
	if !errors.Is(err, corestore.ErrRevisionConflict) || readErr != nil || current.Revision != 2 {
		t.Fatalf("updateConversation(stale) error = %v, want revision conflict at 2", err)
	}
	missing := got
	missing.ID = conversation.NewConversationID()
	err = f.updateConversation(t, t.Context(), &missing, 2)
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("updateConversation(missing) error=%v, want absent CAS target", err)
	}
	if _, readErr := f.readConversation(t.Context(), f.project.ID, missing.ID); readErr == nil {
		t.Fatal("missing conversation became readable after refused update")
	}
	response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/conversations/"+missing.ID, f.ownerToken, map[string]any{"title": "missing"})
	requireNativeStatus(t, response, http.StatusNotFound)
	var missingResponse hubapi.APIErrorResponse
	decodeHubResponse(t, response, &missingResponse)
	if missingResponse.Code != "not_found" {
		t.Fatalf("updateConversation(missing) code=%s, want not_found", missingResponse.Code)
	}

}

func TestConversationStoreLinkedIssueIsUnique(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	issue := f.nativeFixture.create(t, "linked")
	first := f.create(t, f.record(f.owner, "first", now))
	second := f.create(t, f.record(f.owner, "second", now))

	first.WorkItemID = string(issue.WorkItemID)
	linkedAt := now.Add(time.Second)
	first.LinkedAt = &linkedAt
	first.Visibility = conversation.VisibilityShared
	if err := f.updateConversation(t, t.Context(), &first, 1); err != nil {
		t.Fatalf("updateConversation(link) error = %v", err)
	}
	second.WorkItemID = string(issue.WorkItemID)
	second.LinkedAt = &linkedAt
	err := f.updateConversation(t, t.Context(), &second, 1)
	var linked conversationstore.Conversation
	readLinked := func() error {
		return f.readTenant(t.Context(), func(r tenantstore.Reader) error {
			var err error
			linked, err = r.ConversationByWorkItem(t.Context(), f.project.ID, string(issue.WorkItemID))
			return err
		})
	}
	if !errors.Is(err, corestore.ErrUniqueViolation) || readLinked() != nil || linked.ID != first.ID {
		t.Fatalf("updateConversation(second link) error = %v, want conversation_already_linked with %q", err, first.ID)
	}
	fresh := f.record(f.owner, "fresh", now)
	fresh.WorkItemID = string(issue.WorkItemID)
	fresh.LinkedAt = &linkedAt
	err = f.createRecord(t.Context(), &fresh)
	if !errors.Is(err, corestore.ErrUniqueViolation) || readLinked() != nil || linked.ID != first.ID {
		t.Fatalf("createConversation(linked) error = %v, want conversation_already_linked", err)
	}
	unknown := f.record(f.owner, "unknown issue", now)
	unknown.WorkItemID = "wi_missing"
	unknown.LinkedAt = &linkedAt
	err = f.createRecord(t.Context(), &unknown)
	var foreignKey interface{ SQLState() string }
	if !errors.As(err, &foreignKey) || foreignKey.SQLState() != "23503" {
		t.Fatalf("createConversation(unknown issue) error = %v, want not_found", err)
	}

	got, err := f.readConversation(t.Context(), f.project.ID, first.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if got.WorkItemID != string(issue.WorkItemID) || got.LinkedAt == nil || got.Visibility != conversation.VisibilityShared {
		t.Fatalf("readConversation() = %#v", got)
	}
	// The partial unique index is a backstop for direct writes.
	if _, err := nativeAttemptTestDatabase(t, f.nativeFixture).ExecContext(t.Context(), "UPDATE conversations SET work_item_id=$1 WHERE organization_id=$2 AND project_id=$3 AND id=$4", issue.WorkItemID, f.organization, f.project.ID, second.ID); err == nil {
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
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "message " + strconv.Itoa(i), Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CommandKey: "key-" + strconv.Itoa(i), CreatedAt: now.Add(time.Duration(i) * time.Millisecond), UpdatedAt: now}
			if err := f.appendMessage(t, ctx, &message); err != nil {
				failures <- err
				return
			}
			if message.ID == "" || message.Seq <= 0 {
				failures <- errors.New("appendMessage must allocate id and seq")
				return
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent appendMessage error = %v", err)
	}
	messages, err := f.listMessages(t.Context(), record.ID, 0, 100)
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
	page, err := f.listMessages(t.Context(), record.ID, 11, 5)
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
	if err := f.updateMessage(t, t.Context(), updated); err != nil {
		t.Fatalf("updateMessage() error = %v", err)
	}
	again, err := f.listMessages(t.Context(), record.ID, 2, 1)
	if err != nil {
		t.Fatalf("listMessages() error = %v", err)
	}
	if len(again) != 1 || again[0].Delivery != conversation.DeliveryDelivered || again[0].AttemptID != "att_1" || again[0].TurnID != "turn_1" || again[0].ProviderItemID != "item_1" || again[0].Text != "edited" || !again[0].UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("updated message = %#v", again)
	}
	got, err := f.readConversation(t.Context(), f.project.ID, record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	if got.LastMessageAt == nil {
		t.Fatal("appendMessage must record last_message_at")
	}
	missing := updated
	missing.ID = conversation.NewMessageID()
	if err := f.updateMessage(t, t.Context(), missing); err == nil {
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
	message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageStatus, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "runner"}, CreatedAt: now, UpdatedAt: now}
	if err := f.appendMessage(t, t.Context(), &message); err != nil {
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
	if err := f.upsertQuestion(t, t.Context(), question); err != nil {
		t.Fatalf("upsertQuestion() error = %v", err)
	}
	pending, err := f.pendingQuestions(t.Context(), record.ID)
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
	if err := f.upsertQuestion(t, t.Context(), question); err != nil {
		t.Fatalf("upsertQuestion(update) error = %v", err)
	}
	got, err := f.readQuestion(t.Context(), record.ID, question.ID)
	if err != nil {
		t.Fatalf("readQuestion() error = %v", err)
	}
	if got.Status != conversation.QuestionAnswered || len(got.Answers["p"]) != 1 || got.Answers["p"][0] != "yes" || got.AnsweredBy != f.owner || !got.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("readQuestion() = %#v", got)
	}
	pending, err = f.pendingQuestions(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("listPendingQuestions() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("answered question still pending: %#v", pending)
	}
	_, err = f.readQuestion(t.Context(), record.ID, conversation.NewQuestionID())
	var failure *hubapi.NativeError
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("readQuestion(missing) error = %v, want not_found", err)
	}
	_, err = f.readQuestion(t.Context(), conversation.NewConversationID(), question.ID)
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

	existing, conflict, err := f.reserveCommand(t, t.Context(), record.ID, command.Key, command.Kind, hash, now)
	if err != nil || existing != nil || conflict {
		t.Fatalf("reserveCommand(first) = %#v, %v, %v", existing, conflict, err)
	}
	receipt := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliveryQueued, MessageID: "msg_x", UpdatedAt: now.Add(time.Second)}
	if err := f.updateReceipt(t, t.Context(), record.ID, command.Key, receipt); err != nil {
		t.Fatalf("updateReceipt() error = %v", err)
	}
	existing, conflict, err = f.reserveCommand(t, t.Context(), record.ID, command.Key, command.Kind, hash, now.Add(time.Minute))
	if err != nil || conflict || existing == nil {
		t.Fatalf("reserveCommand(replay) = %#v, %v, %v", existing, conflict, err)
	}
	if existing.Status != conversation.DeliveryQueued || existing.MessageID != "msg_x" || !existing.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("replayed receipt = %#v", existing)
	}
	other := command
	other.Text = "different"
	existing, conflict, err = f.reserveCommand(t, t.Context(), record.ID, command.Key, command.Kind, conversation.RequestHash(other), now)
	if err != nil || !conflict || existing == nil || existing.Key != command.Key {
		t.Fatalf("reserveCommand(conflict) = %#v, %v, %v", existing, conflict, err)
	}
	if err := f.updateReceipt(t, t.Context(), record.ID, "missing", receipt); err == nil {
		t.Fatal("updateReceipt(missing) must fail")
	}
	// The same key in another conversation is independent.
	existing, conflict, err = f.reserveCommand(t, t.Context(), second.ID, command.Key, command.Kind, hash, now)
	if err != nil || existing != nil || conflict {
		t.Fatalf("reserveCommand(other conversation) = %#v, %v, %v", existing, conflict, err)
	}
}

func TestConversationStoreEvents(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "events", now))

	bodies := []any{map[string]any{"message_id": "msg_1", "seq": 1, "text": "hel"}, map[string]any{"seq": 1}}
	kinds := []conversation.EventType{conversation.EventMessageDelta, conversation.EventHeartbeat}
	plan, err := f.publication(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		if err := preparePGWorkerEvent(t.Context(), plan, body); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.WithValue(t.Context(), pgWorkerPublicationKey{}, plan)
	rollback := errors.New("deliberate conversation rollback")
	err = f.writeTenant(ctx, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, now); err != nil {
			return err
		}
		for index, body := range bodies {
			if err := appendPGConversationEvent(ctx, w, f.organization, record.ID, kinds[index], body, now); err != nil {
				return err
			}
			got, err := w.Conversation(ctx, f.project.ID, record.ID)
			if err != nil || got.EventSequence != int64(index+1) {
				t.Fatalf("appendEvent() sequence=%d, %v", got.EventSequence, err)
			}
		}
		page, err := w.ConversationEvents(ctx, record.ID, 0, 10)
		if err != nil {
			return err
		}
		var events []conversation.Event
		for _, row := range page.Items {
			content, ok := plan.Reads.Load(string(f.organization) + "/" + row.ContentReference)
			if !ok {
				t.Fatal("published uncommitted event content is absent")
			}
			var payload conversationContent
			if err := json.Unmarshal(content, &payload); err != nil {
				return err
			}
			event := row.Event
			event.Data = payload.Data
			events = append(events, event)
		}
		if len(events) != 2 || events[0].Seq != 1 || events[0].Type != conversation.EventMessageDelta || events[1].Seq != 2 || string(events[0].Data) != `{"message_id":"msg_1","seq":1,"text":"hel"}` {
			t.Fatalf("listEvents() = %#v", events)
		}
		after, err := w.ConversationEvents(ctx, record.ID, 1, 10)
		if err != nil || len(after.Items) != 1 || after.Items[0].Seq != 2 {
			t.Fatalf("listEvents(after=1) = %#v, %v", after.Items, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback error = %v", err)
	}
	if _, err := f.appendEvent(t, t.Context(), record.ID, conversation.EventType("bogus"), nil, now); err == nil {
		t.Fatal("appendEvent(bogus type) must fail")
	}
	if _, err := f.appendEvent(t, t.Context(), conversation.NewConversationID(), conversation.EventHeartbeat, nil, now); err == nil {
		t.Fatal("appendEvent(missing conversation) must fail")
	}
	got, err := f.readConversation(t.Context(), f.project.ID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.listEvents(t.Context(), record.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.EventSeq != 0 || len(events) != 0 {
		t.Fatalf("rollback left event_seq=%d events=%d", got.EventSeq, len(events))
	}
	t.Run("external content stays isolated across conversations and organizations", func(t *testing.T) {
		objects := &conversationContentFixture{}
		f.store.Contents = objects
		response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Second organization"})
		requireNativeStatus(t, response, http.StatusCreated)
		var organization nativeOrganization
		decodeHubResponse(t, response, &organization)
		foreign := newNativeFixture(t, f.service, organization.ID, "second-org-project")
		var owner accessstore.Credential
		if err := f.readTenant(t.Context(), func(r tenantstore.Reader) error {
			var err error
			owner, err = r.AccessCredential(t.Context(), f.owner)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		owner.OrganizationID = organization.ID
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), owner); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: f.owner, ProjectID: foreign.project.ID})
		}); err != nil {
			t.Fatal(err)
		}
		scoped := func(record conversationRecord) conversationFixture {
			value := f
			if record.OrganizationID == organization.ID {
				value.organization, value.nativeFixture = organization.ID, foreign
			}
			return value
		}

		records := []conversationRecord{}
		text := strings.Repeat("shared tool output ", 200)
		for index := range 4 {
			record := f.record(f.owner, "content", now)
			if index >= 2 {
				record.OrganizationID = organization.ID
				record.ProjectID = foreign.project.ID
			}
			record = f.create(t, record)
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleSystem, Kind: conversation.MessageTool, Delivery: conversation.DeliveryCompleted, Text: text, Data: []byte("{}"), Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: f.owner}, CreatedAt: now, UpdatedAt: now}
			ctx := t.Context()
			if err := scoped(record).appendMessage(t, ctx, &message); err != nil {
				t.Fatal(err)
			}
			if _, err := scoped(record).appendEvent(t, ctx, record.ID, conversation.EventMessageAccepted, projectMessage(message), now); err != nil {
				t.Fatal(err)
			}
			records = append(records, record)
		}
		var ids []string
		for _, record := range records {
			messages, err := scoped(record).listMessages(t.Context(), record.ID, 0, 50)
			if err != nil || len(messages) != 1 || messages[0].Text != text {
				t.Fatalf("content read for %s: %v", record.ID, err)
			}
			ids = append(ids, messages[0].ContentID)
			replay, err := f.publishedRecordEvents(t.Context(), record, 0)
			if err != nil || len(replay) != 1 {
				t.Fatalf("replay for %s: %v", record.ID, err)
			}
			var got conversationMessageResource
			if err := json.Unmarshal(replay[0].Data, &got); err != nil {
				t.Fatal(err)
			}
			if got.ConversationID != record.ID || got.Text != text {
				t.Fatal("replay crossed a conversation boundary")
			}
		}
		if ids[0] != ids[1] || ids[2] != ids[3] || ids[0] == ids[2] || len(objects.content) != 8 {
			t.Fatalf("org deduplication=%v objects=%d", ids, len(objects.content))
		}
		if _, err := f.readConversation(t.Context(), f.project.ID, records[2].ID); err == nil {
			t.Fatal("foreign conversation was readable")
		}
		f.store.Contents = objects
		for index, record := range records {
			tool := "tool-" + strconv.Itoa(index)
			data, err := json.Marshal(map[string]string{"tool": tool, "item_id": record.ID, "status": "running"})
			if err != nil {
				t.Fatal(err)
			}
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleSystem, Kind: conversation.MessageTool, Text: "Running " + tool, Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: now, UpdatedAt: now}
			if err := scoped(record).appendMessage(t, t.Context(), &message); err != nil {
				t.Fatal(err)
			}
			if _, err := scoped(record).appendEvent(t, t.Context(), record.ID, conversation.EventMessageAccepted, projectMessage(message), now); err != nil {
				t.Fatal(err)
			}
			for _, status := range []string{"completed", "saved"} {
				message.Text = status + " " + tool
				message.Data, err = json.Marshal(map[string]string{"tool": tool, "item_id": record.ID, "status": status})
				if err != nil {
					t.Fatal(err)
				}
				if err := scoped(record).updateChatMessage(t, t.Context(), message, now); err != nil {
					t.Fatal(err)
				}
			}
			replay, err := f.publishedRecordEvents(t.Context(), record, 1)
			if err != nil || len(replay) != 3 {
				t.Fatalf("inline tool replay: events=%d error=%v", len(replay), err)
			}
			for revision, status := range []string{"running", "completed", "saved"} {
				var got struct {
					Text string
					Data map[string]string
				}
				if err := json.Unmarshal(replay[revision].Data, &got); err != nil {
					t.Fatal(err)
				}
				wantText := status + " " + tool
				if revision == 0 {
					wantText = "Running " + tool
				}
				if got.Text != wantText || got.Data["tool"] != tool || got.Data["item_id"] != record.ID || got.Data["status"] != status {
					t.Fatalf("inline revision crossed a boundary: %+v", got)
				}
			}
			rows, err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryContext(t.Context(), "SELECT body_json FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2", record.OrganizationID, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			var oversized int
			for rows.Next() {
				var body []byte
				if err := rows.Scan(&body); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				if len(body) > 2048 || fields["text"] != nil || fields["data"] != nil {
					oversized++
				}
			}
			err = errors.Join(rows.Err(), rows.Close())
			if err != nil || oversized != 0 {
				t.Fatalf("noncompact events=%d error=%v", oversized, err)
			}
		}
		f.store.Contents = objects
		if result, err := nativeAttemptTestDatabase(t, f.nativeFixture).ExecContext(t.Context(), "UPDATE conversation_messages SET content_attachment_id=$1 WHERE organization_id=$2 AND conversation_id=$3", ids[2], f.organization, records[0].ID); err != nil {
			t.Fatal(err)
		} else if count, err := result.RowsAffected(); err != nil || count != 2 {
			t.Fatalf("scoped corruption rows=%d error=%v", count, err)
		}
		if _, err := f.listMessages(t.Context(), records[0].ID, 0, 50); err == nil {
			t.Fatal("foreign object reference was hydrated")
		}
	})

}

func TestConversationStoreStartsAndAudience(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	record := f.create(t, f.record(f.owner, "starts", now))
	other := f.create(t, f.record(f.owner, "other", now))

	if err := f.writeTenant(t.Context(), func(w tenantstore.Writer) error { return w.RecordStart(t.Context(), "att_1", record.ID, now) }); err != nil {
		t.Fatalf("recordStart() error = %v", err)
	}
	err := f.writeTenant(t.Context(), func(w tenantstore.Writer) error { return w.RecordStart(t.Context(), "att_1", other.ID, now) })
	var existing string
	readErr := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), "SELECT conversation_id FROM conversation_starts WHERE organization_id=$1 AND attempt_id='att_1'", f.organization).Scan(&existing)
	if !errors.Is(err, corestore.ErrUniqueViolation) || readErr != nil || existing != record.ID {
		t.Fatalf("recordStart(duplicate) error = %v, want started conflict for %q", err, record.ID)
	}
	if err := f.writeTenant(t.Context(), func(w tenantstore.Writer) error { return w.RecordStart(t.Context(), "att_2", record.ID, now) }); err != nil {
		t.Fatalf("recordStart(second attempt) error = %v", err)
	}
	if err := f.writeTenant(t.Context(), func(w tenantstore.Writer) error {
		return w.RecordAudience(t.Context(), record.ID, f.owner, conversation.VisibilityPrivate, conversation.VisibilityShared, now)
	}); err != nil {
		t.Fatalf("recordAudience() error = %v", err)
	}
	var count int
	if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_audience_events WHERE organization_id=$1 AND conversation_id=$2 AND from_visibility='private' AND to_visibility='shared' AND actor_principal_id=$3", f.organization, record.ID, f.owner).Scan(&count); err != nil {
		t.Fatalf("count audience events: %v", err)
	}
	if count != 1 {
		t.Fatalf("audience events = %d, want 1", count)
	}
}

func (f conversationFixture) writeTenant(ctx context.Context, write func(tenantstore.Writer) error) error {
	return f.service.config.TenantRepository.WriteTenant(ctx, f.organization, corestore.Serving, write)
}

func (f conversationFixture) readConversation(ctx context.Context, project tracker.ProjectID, id string) (conversationRecord, error) {
	record, err := f.readRecord(ctx, project, id)
	return record, translateConversationError(err)
}

func (f conversationFixture) listConversations(ctx context.Context, query conversationListQuery) (records []conversationRecord, next string, err error) {
	err = f.readTenant(ctx, func(r tenantstore.Reader) error {
		page, err := r.Conversations(ctx, conversationstore.ConversationQuery{ProjectID: query.Project, PrincipalID: query.Principal, Cursor: query.Cursor, Limit: query.Limit, Settled: query.Settled})
		for _, record := range page.Items {
			records = append(records, conversationRecordFromStore(record))
		}
		next = page.Next
		return err
	})
	return
}

func (f conversationFixture) publication(ctx context.Context, id string) (*conversationPublicationPlan, error) {
	var record conversationRecord
	err := f.readTenant(ctx, func(r tenantstore.Reader) error {
		row, err := r.ConversationByID(ctx, id)
		record = conversationRecordFromStore(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &conversationPublicationPlan{Service: f.recordChat(record.OrganizationID), Record: record, Now: record.UpdatedAt}, nil
}

func (f conversationFixture) createRecord(ctx context.Context, record *conversationRecord) error {
	if err := validateConversationRecord(record); err != nil {
		return err
	}
	plan := &conversationPublicationPlan{Service: f.recordChat(record.OrganizationID), Record: *record, Now: record.UpdatedAt}
	raw, err := recordPayload(*record)
	if err != nil {
		return err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	ref, err := plan.Prepare(ctx, content)
	if err != nil {
		return err
	}
	record.ContentID = contentArchiveID(string(record.OrganizationID), ref)
	return f.service.config.TenantRepository.WriteTenant(ctx, record.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, record.OrganizationID, plan.Prepared, record.CreatedAt); err != nil {
			return err
		}
		row, err := w.CreateConversation(ctx, conversationRecordToStore(*record))
		if err == nil {
			*record = conversationRecordFromStore(row)
		}
		return err
	})
}

func (f conversationFixture) updateConversation(t *testing.T, ctx context.Context, record *conversationRecord, expected int64) error {
	t.Helper()
	plan := &conversationPublicationPlan{Service: f.recordChat(record.OrganizationID), Record: *record, Now: record.UpdatedAt}
	raw, err := recordPayload(*record)
	if err != nil {
		return err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	ref, err := plan.Prepare(ctx, content)
	if err != nil {
		return err
	}
	record.ContentID = contentArchiveID(string(record.OrganizationID), ref)
	return f.writeTenant(ctx, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, record.UpdatedAt); err != nil {
			return err
		}
		row, err := w.UpdateConversation(ctx, conversationRecordToStore(*record), expected)
		if err == nil {
			*record = conversationRecordFromStore(row)
		}
		return err
	})
}

func (f conversationFixture) appendMessage(t *testing.T, ctx context.Context, message *conversationMessageRecord) error {
	t.Helper()
	if err := validateConversationMessage(message); err != nil {
		return err
	}
	plan, err := f.publication(ctx, message.ConversationID)
	if err != nil {
		return err
	}
	if err := preparePGConversationMessage(ctx, plan, message); err != nil {
		return err
	}
	return f.writeTenant(ctx, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, message.CreatedAt); err != nil {
			return err
		}
		row, err := w.CreateMessage(ctx, conversationMessageToStore(f.organization, *message))
		if err == nil {
			message.ID, message.Seq, message.Revision = row.ID, row.Sequence, row.Revision
		}
		return err
	})
}

func (f conversationFixture) listMessages(ctx context.Context, id string, before int64, limit int) ([]conversationMessageRecord, error) {
	server := f.recordServer(f.organization)
	return (&conversationTurnCoordinator{server: server, service: server.conversations}).coordinatorMessages(ctx, conversationstore.MessageFilter{ConversationID: id, BeforeSequence: before, Limit: limit})
}

func (f conversationFixture) updateMessage(t *testing.T, ctx context.Context, message conversationMessageRecord) error {
	t.Helper()
	plan, err := f.publication(ctx, message.ConversationID)
	if err != nil {
		return err
	}
	message, err = plan.Message(ctx, message)
	if err != nil {
		return err
	}
	return f.writeTenant(ctx, func(w tenantstore.Writer) error {
		stored, err := w.Message(ctx, message.ConversationID, message.ID)
		if err != nil {
			return err
		}
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, message.UpdatedAt); err != nil {
			return err
		}
		_, err = w.UpdateMessage(ctx, conversationMessageToStore(f.organization, message), stored.Revision)
		return err
	})
}

func (f conversationFixture) updateChatMessage(t *testing.T, ctx context.Context, message conversationMessageRecord, now time.Time) error {
	t.Helper()
	plan, err := f.publication(ctx, message.ConversationID)
	if err != nil {
		return err
	}
	var stored conversationstore.Message
	if err := f.readTenant(ctx, func(r tenantstore.Reader) error {
		var err error
		stored, err = r.Message(ctx, message.ConversationID, message.ID)
		return err
	}); err != nil {
		return err
	}
	message.Revision, message.UpdatedAt = stored.Revision+1, now
	message, err = plan.Message(ctx, message)
	if err != nil {
		return err
	}
	body := projectMessage(message)
	if err := preparePGWorkerEvent(ctx, plan, body); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, pgWorkerPublicationKey{}, plan)
	return f.writeTenant(ctx, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, now); err != nil {
			return err
		}
		if _, err := w.UpdateMessage(ctx, conversationMessageToStore(f.organization, message), stored.Revision); err != nil {
			return err
		}
		return appendPGConversationEvent(ctx, w, f.organization, message.ConversationID, conversation.EventMessageUpdated, body, now)
	})
}

func (f conversationFixture) appendEvent(t *testing.T, ctx context.Context, id string, kind conversation.EventType, body any, now time.Time) (seq int64, err error) {
	t.Helper()
	plan, err := f.publication(ctx, id)
	if err != nil {
		return 0, err
	}
	if body == nil {
		body = map[string]any{}
	}
	if err := preparePGWorkerEvent(ctx, plan, body); err != nil {
		return 0, err
	}
	ctx = context.WithValue(ctx, pgWorkerPublicationKey{}, plan)
	err = f.writeTenant(ctx, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, now); err != nil {
			return err
		}
		if err := appendPGConversationEvent(ctx, w, f.organization, id, kind, body, now); err != nil {
			return err
		}
		row, err := w.ConversationByID(ctx, id)
		seq = row.EventSequence
		return err
	})
	return
}

func (f conversationFixture) listEvents(ctx context.Context, id string, after int64, limit int) ([]conversation.Event, error) {
	record, err := f.readRecord(ctx, f.project.ID, id)
	if err != nil {
		return nil, err
	}
	events, err := f.publishedRecordEvents(ctx, record, after)
	if len(events) > limit {
		events = events[:limit]
	}
	return events, err
}

func (f conversationFixture) upsertQuestion(t *testing.T, ctx context.Context, question conversationQuestionRecord) error {
	t.Helper()
	plan, err := f.publication(ctx, question.ConversationID)
	if err != nil {
		return err
	}
	if err := plan.PrepareQuestion(ctx, question); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, pgWorkerPublicationKey{}, plan)
	stored, err := preparedPGConversationQuestion(ctx, conversationstore.Question{OrganizationID: f.organization, ConversationID: question.ConversationID, Question: question.Question, RequestID: question.RequestID})
	if err != nil {
		return err
	}
	return f.writeTenant(ctx, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, question.UpdatedAt); err != nil {
			return err
		}
		return w.UpsertQuestion(ctx, stored)
	})
}

func (f conversationFixture) readQuestion(ctx context.Context, id, questionID string) (question conversationQuestionRecord, err error) {
	var row conversationstore.Question
	err = f.readTenant(ctx, func(r tenantstore.Reader) error {
		var err error
		row, err = r.Question(ctx, id, questionID)
		return err
	})
	if err != nil {
		return question, translateConversationError(err)
	}
	row, err = f.recordChat(f.organization).Server.HydratePGConversationQuestion(ctx, f.organization, row)
	return conversationQuestionRecord{Question: row.Question, RequestID: row.RequestID, ContentID: row.ContentReference}, err
}

func (f conversationFixture) pendingQuestions(ctx context.Context, id string) (questions []conversationQuestionRecord, err error) {
	var rows []conversationstore.Question
	err = f.readTenant(ctx, func(r tenantstore.Reader) error { var err error; rows, err = r.Questions(ctx, id, "", 100); return err })
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Status != conversation.QuestionPending {
			continue
		}
		question, err := f.readQuestion(ctx, id, row.ID)
		if err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	return
}

func (f conversationFixture) reserveCommand(t *testing.T, ctx context.Context, id, key string, kind conversation.CommandKind, hash string, now time.Time) (existing *conversation.Receipt, conflict bool, err error) {
	t.Helper()
	receipt := conversation.Receipt{Key: key, Kind: kind, Status: conversation.DeliverySaved, UpdatedAt: now.UTC()}
	plan, err := f.publication(ctx, id)
	if err != nil {
		return nil, false, err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return nil, false, err
	}
	if err := plan.PrepareValue(ctx, raw); err != nil {
		return nil, false, err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return nil, false, err
	}
	ref := plan.Prepared[string(f.organization)+"/"+artifact.Digest(content)]
	err = f.writeTenant(ctx, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, now); err != nil {
			return err
		}
		row, inserted, err := w.ReserveCommand(ctx, conversationstore.Command{OrganizationID: f.organization, ConversationID: id, Key: key, Kind: kind, RequestHash: hash, Receipt: receipt, CreatedAt: now, UpdatedAt: now, ContentReference: contentArchiveID(string(f.organization), ref)})
		if errors.Is(err, corestore.ErrUniqueViolation) {
			existing, conflict = &row.Receipt, true
			return nil
		}
		if err == nil && !inserted {
			existing = &row.Receipt
		}
		return err
	})
	return
}

func (f conversationFixture) updateReceipt(t *testing.T, ctx context.Context, id, key string, receipt conversation.Receipt) error {
	t.Helper()
	if receipt.Key == "" {
		receipt.Key = key
	}
	plan, err := f.publication(ctx, id)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := plan.PrepareValue(ctx, raw); err != nil {
		return err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	ref := plan.Prepared[string(f.organization)+"/"+artifact.Digest(content)]
	return f.writeTenant(ctx, func(w tenantstore.Writer) error {
		row, found, err := w.Command(ctx, id, key)
		if err != nil {
			return err
		}
		if !found {
			return corestore.ErrNotFound
		}
		if err := registerPreparedConversationObjects(ctx, w, f.organization, plan.Prepared, receipt.UpdatedAt); err != nil {
			return err
		}
		row.Receipt, row.UpdatedAt, row.ContentReference = receipt, receipt.UpdatedAt, contentArchiveID(string(f.organization), ref)
		return w.UpdateCommand(ctx, row)
	})
}
