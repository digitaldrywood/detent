package hubserver

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type recordingWaker struct {
	mu          sync.Mutex
	woken       []string
	unavailable bool
}

func (w *recordingWaker) Available() bool             { return !w.unavailable }
func (w *recordingWaker) Cancel(string) bool          { return false }
func (w *recordingWaker) Hold(string) (func(), error) { return func() {}, nil }
func (w *recordingWaker) Wake(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woken = append(w.woken, id)
}
func (w *recordingWaker) Stop() {}

func TestConversationServiceWakesPendingWorkAfterRestart(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		coordinator     bool
		wantCoordinator bool
	}{
		{name: "a hub coordinator takes the saved unlinked message", coordinator: true, wantCoordinator: true},
		{name: "without a hub coordinator the saved message waits for a link"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConversationFixture(t)
			now := time.Now().UTC().Truncate(time.Second)
			service := &conversationService{Server: f.service.conversationServer(), Store: f.store, Broker: newNotificationBroker().Broker, Logger: f.service.config.Logger}
			coordinator := &recordingWaker{unavailable: !test.coordinator}
			controls := &recordingWaker{}
			service.Coordinator = coordinator
			service.Controls = controls

			issue := f.nativeFixture.create(t, "linked")
			unlinked := f.create(t, f.record(f.owner, "Unlinked", now))
			linked := f.record(f.owner, "Linked", now)
			linked.Visibility = conversation.VisibilityShared
			linked.WorkItemID = string(issue.WorkItemID)
			linkedAt := now
			linked.LinkedAt = &linkedAt
			linked = f.create(t, linked)
			idle := f.create(t, f.record(f.owner, "Idle", now))

			for _, seed := range []struct {
				record   conversationRecord
				delivery conversation.Delivery
			}{{unlinked, conversation.DeliverySaved}, {linked, conversation.DeliveryQueued}, {idle, conversation.DeliveryCompleted}} {
				message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: seed.record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "hello", Data: json.RawMessage("{}"), Delivery: seed.delivery, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CreatedAt: now, UpdatedAt: now}
				if err := f.appendMessage(t, t.Context(), &message); err != nil {
					t.Fatalf("appendMessage() error = %v", err)
				}
			}

			if err := service.WakePending(t.Context()); err != nil {
				t.Fatalf("wakePending() error = %v", err)
			}
			wantCoordinator := []string{}
			if test.wantCoordinator {
				wantCoordinator = []string{unlinked.ID}
			}
			if got := append([]string{}, coordinator.woken...); !slices.Equal(got, wantCoordinator) {
				t.Fatalf("coordinator woken = %v, want %v", got, wantCoordinator)
			}
			if got := controls.woken; len(got) != 1 || got[0] != linked.ID {
				t.Fatalf("controls woken = %v, want [%s]", got, linked.ID)
			}
		})
	}
}
