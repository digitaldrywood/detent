package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationBrokerNotifiesSubscribersOnce(t *testing.T) {
	broker := newConversationBroker()
	first, cancelFirst := broker.subscribe("conv_a")
	defer cancelFirst()
	other, cancelOther := broker.subscribe("conv_b")
	defer cancelOther()

	broker.notify("conv_a")
	broker.notify("conv_a")
	select {
	case <-first.wake:
	default:
		t.Fatal("expected a wake signal for conv_a")
	}
	select {
	case <-first.wake:
		t.Fatal("wake signals must coalesce to one pending signal")
	default:
	}
	select {
	case <-other.wake:
		t.Fatal("conv_b must not be woken by conv_a events")
	default:
	}
}

func TestConversationBrokerCancelAndClose(t *testing.T) {
	broker := newConversationBroker()
	subscription, cancel := broker.subscribe("conv_a")
	cancel()
	select {
	case <-subscription.closed:
	default:
		t.Fatal("cancel must close the subscription")
	}
	if len(broker.subscribers) != 0 {
		t.Fatalf("expected no subscribers after cancel, got %d", len(broker.subscribers))
	}
	broker.notify("conv_a")

	live, cancelLive := broker.subscribe("conv_a")
	defer cancelLive()
	broker.closeAll()
	select {
	case <-live.closed:
	default:
		t.Fatal("closeAll must close live subscriptions")
	}
	late, cancelLate := broker.subscribe("conv_a")
	defer cancelLate()
	select {
	case <-late.closed:
	default:
		t.Fatal("subscribing after close must return a closed subscription")
	}
}

func TestConversationAuthorizeRead(t *testing.T) {
	service := &conversationService{}
	base := conversationRecord{OrganizationID: "org_a", ProjectID: "prj_a", OwnerPrincipalID: "tok_owner", Visibility: conversation.VisibilityPrivate}
	tests := []struct {
		name    string
		scope   nativeScope
		record  conversationRecord
		wantErr bool
	}{
		{name: "owner reads private", scope: nativeScope{organization: "org_a", project: "prj_a", credential: apiCredential{ID: "tok_owner"}}, record: base},
		{name: "other principal denied private", scope: nativeScope{organization: "org_a", project: "prj_a", credential: apiCredential{ID: "tok_other"}}, record: base, wantErr: true},
		{name: "other principal reads shared", scope: nativeScope{organization: "org_a", project: "prj_a", credential: apiCredential{ID: "tok_other"}}, record: func() conversationRecord {
			record := base
			record.Visibility = conversation.VisibilityShared
			return record
		}()},
		{name: "cross project denied", scope: nativeScope{organization: "org_a", project: "prj_b", credential: apiCredential{ID: "tok_owner"}}, record: base, wantErr: true},
		{name: "cross organization denied", scope: nativeScope{organization: "org_b", project: "prj_a", credential: apiCredential{ID: "tok_owner"}}, record: base, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := service.authorizeRead(test.scope, test.record)
			if test.wantErr {
				var failure *nativeError
				if !errors.As(err, &failure) || failure.status != http.StatusNotFound {
					t.Fatalf("expected opaque not_found, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestTranslateConversationError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{name: "linked", err: &errConversationLinked{ExistingConversationID: "conv_x"}, code: "conversation_already_linked", status: http.StatusConflict},
		{name: "revision", err: &errConversationRevision{}, code: "revision_conflict", status: http.StatusConflict},
		{name: "stale", err: conversation.ErrStaleExecution, code: "stale_execution", status: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var failure *nativeError
			if !errors.As(translateConversationError(test.err), &failure) {
				t.Fatalf("expected nativeError")
			}
			if failure.Code != test.code || failure.status != test.status {
				t.Fatalf("got %s/%d, want %s/%d", failure.Code, failure.status, test.code, test.status)
			}
		})
	}
	var linkedErr *nativeError
	if !errors.As(conversationAlreadyLinked("conv_x"), &linkedErr) || linkedErr.Details["existing_conversation_id"] != "conv_x" {
		t.Fatalf("expected existing conversation in details, got %v", linkedErr)
	}
}

func TestProjectConversationShape(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	record := conversationRecord{
		ID: "conv_1", OrganizationID: tracker.OrganizationID("org_a"), ProjectID: tracker.ProjectID("prj_a"),
		OwnerPrincipalID: "tok_owner", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive,
		Execution: conversation.Execution{Owner: conversation.Owner{LeaseID: "lease_secret", FencingToken: 7, AttemptID: "attempt_1"}, UpdatedAt: now},
		Revision:  3, CreatedAt: now, UpdatedAt: now,
	}
	encoded, err := json.Marshal(projectConversation(record))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["work_item_id"] != nil {
		t.Fatalf("unlinked conversation must report null work_item_id, got %v", decoded["work_item_id"])
	}
	execution := decoded["execution"].(map[string]any)
	if execution["status"] != "idle" {
		t.Fatalf("empty execution status must project as idle, got %v", execution["status"])
	}
	if _, leaked := execution["lease_id"]; leaked {
		t.Fatal("lease and fencing token must not be exposed on the public resource")
	}
	if execution["attempt_id"] != "attempt_1" {
		t.Fatalf("attempt id missing: %v", execution)
	}
	message := projectMessage(conversationMessageRecord{ID: "msg_1", Delivery: conversation.DeliverySaved})
	if message.AttemptID != nil || message.CommandKey != nil {
		t.Fatal("empty message identifiers must be null")
	}
}

type recordingWaker struct {
	mu    sync.Mutex
	woken []string
}

func (w *recordingWaker) Wake(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woken = append(w.woken, id)
}
func (w *recordingWaker) Stop() {}

func TestConversationServiceWakesPendingWorkAfterRestart(t *testing.T) {
	f := newConversationFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	service := &conversationService{server: f.service, store: f.store, broker: newConversationBroker(), logger: f.service.config.Logger}
	controls := &recordingWaker{}
	service.controls = controls

	issue := f.nativeFixture.create(t, "linked")
	unlinked := f.create(t, f.record(f.owner, "Unlinked", now))
	linked := f.record(f.owner, "Linked", now)
	linked.Visibility = conversation.VisibilityShared
	linked.WorkItemID = string(issue.WorkItemID)
	linkedAt := now
	linked.LinkedAt = &linkedAt
	linked = f.create(t, linked)
	idle := f.create(t, f.record(f.owner, "Idle", now))

	tx := f.tx(t)
	for _, seed := range []struct {
		record   conversationRecord
		delivery conversation.Delivery
	}{{unlinked, conversation.DeliverySaved}, {linked, conversation.DeliveryQueued}, {idle, conversation.DeliveryCompleted}} {
		message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: seed.record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "hello", Data: json.RawMessage("{}"), Delivery: seed.delivery, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CreatedAt: now, UpdatedAt: now}
		if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
			t.Fatalf("appendMessage() error = %v", err)
		}
	}
	f.commit(t, tx)

	if err := service.wakePending(t.Context()); err != nil {
		t.Fatalf("wakePending() error = %v", err)
	}
	if got := controls.woken; len(got) != 1 || got[0] != linked.ID {
		t.Fatalf("controls woken = %v, want [%s]", got, linked.ID)
	}
}
