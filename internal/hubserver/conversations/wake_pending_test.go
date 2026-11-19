package conversations

import (
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type pendingWaker struct {
	available bool
	wakes     []string
}

func (w *pendingWaker) Available() bool           { return w.available }
func (*pendingWaker) Cancel(string) bool          { return false }
func (*pendingWaker) Hold(string) (func(), error) { return func() {}, nil }
func (w *pendingWaker) Wake(id string)            { w.wakes = append(w.wakes, id) }
func (*pendingWaker) Stop()                       {}

func TestConversationServiceWakesPendingWorkAfterRestart(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	for _, test := range []struct {
		name        string
		coordinator bool
	}{
		{"a coordinator wakes the saved unlinked message", true},
		{"without a coordinator the saved message awaits linking", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
			project := hubtest.Project(t, repository, org, hubtest.ProjectOptions{}).ID
			owner := hubtest.Credential(t, repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project}, Now: now}).ID
			const itemID tracker.NativeWorkItemID = "wi_linked"
			unlinked, linked, idle := conversation.NewConversationID(), conversation.NewConversationID(), conversation.NewConversationID()
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: itemID, Title: "Linked issue", Number: 1}); err != nil {
					return err
				}
				for _, seed := range []struct {
					id       string
					linked   bool
					delivery conversation.Delivery
				}{{unlinked, false, conversation.DeliverySaved}, {linked, true, conversation.DeliveryQueued}, {idle, false, conversation.DeliveryCompleted}} {
					row := conversationstore.Conversation{
						OrganizationID: org, ProjectID: project, ID: seed.id, OwnerPrincipalID: owner,
						Origin: ConversationOriginUser, Title: seed.id, Visibility: conversation.VisibilityPrivate,
						Status: conversation.StatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
					}
					if seed.linked {
						row.Visibility, row.WorkItemID, row.LinkedAt = conversation.VisibilityShared, string(itemID), &now
					}
					if _, err := w.CreateConversation(t.Context(), row); err != nil {
						return err
					}
					message := conversationstore.Message{OrganizationID: org, ConversationID: row.ID, ID: conversation.NewMessageID(), Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "hello", Data: []byte("{}"), Delivery: seed.delivery, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: owner}, CreatedAt: now, UpdatedAt: now}
					if _, err := w.CreateMessage(t.Context(), message); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			coordinator := &pendingWaker{available: test.coordinator}
			controls := &pendingWaker{}
			service := &ConversationService{
				Server: NewServer(Deps{
					Config:       func() Config { return Config{TenantRepository: repository} },
					Organization: func() string { return string(org) },
				}),
				Coordinator: coordinator,
				Controls:    controls,
				Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			if err := service.WakePending(t.Context()); err != nil {
				t.Fatal(err)
			}
			var wantCoordinator []string
			if test.coordinator {
				wantCoordinator = []string{unlinked}
			}
			if !slices.Equal(coordinator.wakes, wantCoordinator) {
				t.Fatalf("coordinator wakes = %v, want %v", coordinator.wakes, wantCoordinator)
			}
			if !slices.Equal(controls.wakes, []string{linked}) {
				t.Fatalf("worker wakes = %v, want %v", controls.wakes, []string{linked})
			}
		})
	}
}
