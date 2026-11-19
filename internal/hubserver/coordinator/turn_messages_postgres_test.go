package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorPostgresReadsPendingMessages(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	id := conversation.NewConversationID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_coordinator_first", "org_coordinator_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: org, Now: now})
		project := hubtest.Project(t, repository, org, hubtest.ProjectOptions{}).ID
		owner := hubtest.Credential(t, repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project}, Now: now}).ID
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{
				OrganizationID: org, ProjectID: project, ID: id, OwnerPrincipalID: owner, Origin: conversations.ConversationOriginUser,
				Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1,
				Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
			for _, delivery := range []conversation.Delivery{conversation.DeliveryDelivered, conversation.DeliverySaved, conversation.DeliveryQueued} {
				if _, err := w.CreateMessage(t.Context(), conversationstore.Message{
					OrganizationID: org, ConversationID: id, ID: conversation.NewMessageID(), Role: conversation.RoleUser,
					Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: owner}, Kind: conversation.MessageText,
					Text: string(org) + ":" + string(delivery), Delivery: delivery, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_coordinator_first", "org_coordinator_second"} {
		t.Run(string(org), func(t *testing.T) {
			service := &conversations.ConversationService{Server: conversations.NewServer(conversations.Deps{
				Config:       func() conversations.Config { return conversations.Config{TenantRepository: repository} },
				Organization: func() string { return string(org) },
			})}
			records := RecordDeps{
				Service: service,
				ReadTenant: func(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
					return repository.ReadTenant(ctx, organization, corestore.Serving, read)
				},
			}
			record, err := service.ReadPublicationConversation(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if record.OrganizationID != org || record.ID != id {
				t.Fatalf("wrong conversation: %+v", record)
			}
			pending, err := records.PendingMessages(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{string(org) + ":saved", string(org) + ":queued"}
			if len(pending) != len(want) || pending[0].Text != want[0] || pending[1].Text != want[1] {
				t.Fatalf("pending messages for %s = %+v, want %v", org, pending, want)
			}
		})
	}
}
