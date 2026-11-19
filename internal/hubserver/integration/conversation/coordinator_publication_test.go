package conversation_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/coordinator"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestCoordinatorPublicationPreservesEventSequence(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	p := hubfixture.NewNative(t, h, h.Primary, "publication project")
	now := h.Now().UTC().Truncate(time.Microsecond)
	repository, organization, project := h.Repository, p.Organization(), p.Project.ID
	owner, err := p.Credential(t, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	service := &conversations.ConversationService{
		Store:       &conversations.ConversationStore{Contents: conversations.ArchiveStore{Call: h.Config.Content}},
		Broker:      eventbroker.New("publication"),
		Coordinator: quietPublicationCoordinator{},
	}
	service.Server = conversations.NewServer(conversations.Deps{
		Config:        func() conversations.Config { return conversations.Config{TenantRepository: repository} },
		Organization:  func() string { return string(organization) },
		Conversations: func() *conversations.ConversationService { return service },
	})
	writer := coordinator.TurnWriteDeps{
		Service: service,
		Now:     func() time.Time { return now },
		WriteTenant: func(ctx context.Context, org tracker.OrganizationID, write func(tenantstore.Writer) error) error {
			return repository.WriteTenant(ctx, org, corestore.Serving, write)
		},
	}
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	future := now.Add(time.Hour)
	for _, tc := range []struct {
		name              string
		lastMessageAt     *time.Time
		wantLastMessageAt time.Time
	}{{"first", nil, now}, {"second", &future, future}} {
		t.Run(tc.name, func(t *testing.T) {
			id := conversation.NewConversationID()
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: project, ID: id, OwnerPrincipalID: owner.ID, Origin: conversations.ConversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now, LastMessageAt: tc.lastMessageAt})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			message := conversations.ConversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: id, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: now, UpdatedAt: now}
			execution := conversation.Execution{Status: conversation.ExecutionRunning, UpdatedAt: now}
			if err := writer.Write(t.Context(), id, func(ctx context.Context, w tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error {
				if err := coordinator.AppendMessage(ctx, w, record, &message, now); err != nil {
					return err
				}
				return coordinator.UpdateExecution(ctx, w, record, execution, now)
			}, func(ctx context.Context, plan *conversations.ConversationPublicationPlan) error {
				if err := plan.AppendMessage(ctx, &message); err != nil {
					return err
				}
				return plan.UpdateExecution(ctx, execution)
			}); err != nil {
				t.Fatalf("publish message and execution: %v", err)
			}
			var snapshotReference, messageReference attachment.ArchiveReference
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.ConversationByID(t.Context(), id)
				if err != nil {
					return err
				}
				if stored.EventSequence != 3 || stored.Execution.Status != conversation.ExecutionRunning {
					t.Fatalf("publication sequence/status = %d/%s", stored.EventSequence, stored.Execution.Status)
				}
				if stored.LastMessageAt == nil || !stored.LastMessageAt.Equal(tc.wantLastMessageAt) {
					t.Fatalf("stored last message = %v, want %v", stored.LastMessageAt, tc.wantLastMessageAt)
				}
				events, err := r.ConversationEvents(t.Context(), id, 0, 10)
				if err != nil {
					return err
				}
				if len(events.Items) != 3 || events.Items[1].Type != conversation.EventConversationUpdated {
					t.Fatalf("publication events = %+v", events.Items)
				}
				content, err := r.ConversationContent(t.Context(), events.Items[1].ContentReference)
				if err != nil {
					return err
				}
				snapshotReference = content.Reference
				content, err = r.ConversationContent(t.Context(), events.Items[0].ContentReference)
				if err != nil {
					return err
				}
				messageReference = content.Reference
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var content conversations.ConversationContent
			if err := service.Server.ReadConversationObject(t.Context(), string(organization), snapshotReference, &content); err != nil {
				t.Fatal(err)
			}
			var snapshot conversations.ConversationResource
			if err := json.Unmarshal(content.Data, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.MessageCount != 1 || snapshot.EventSeq != 1 {
				t.Fatalf("published snapshot count/sequence = %d/%d", snapshot.MessageCount, snapshot.EventSeq)
			}
			if snapshot.LastMessageAt == nil || !snapshot.LastMessageAt.Equal(tc.wantLastMessageAt) {
				t.Fatalf("published last message = %v, want %v", snapshot.LastMessageAt, tc.wantLastMessageAt)
			}
			if err := service.Server.ReadConversationObject(t.Context(), string(organization), messageReference, &content); err != nil {
				t.Fatal(err)
			}
			var accepted conversations.ConversationMessageResource
			if err := json.Unmarshal(content.Data, &accepted); err != nil {
				t.Fatal(err)
			}
			if accepted.ID != message.ID {
				t.Fatalf("published message identity = %s", accepted.ID)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+p.Base+"/conversations/"+id+"/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+p.Token)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("replay status = %d", response.StatusCode)
			}
			accepted = conversations.ConversationMessageResource{}
			scanner := bufio.NewScanner(response.Body)
			for scanner.Scan() {
				data, ok := strings.CutPrefix(scanner.Text(), "data: ")
				if !ok {
					continue
				}
				if err := json.Unmarshal([]byte(data), &accepted); err != nil {
					t.Fatal(err)
				}
				break
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if accepted.ID != message.ID || accepted.Seq != 1 {
				t.Fatalf("replayed message identity/sequence = %s/%d", accepted.ID, accepted.Seq)
			}
		})
	}
}

type quietPublicationCoordinator struct{}

func (quietPublicationCoordinator) Available() bool             { return false }
func (quietPublicationCoordinator) Cancel(string) bool          { return false }
func (quietPublicationCoordinator) Hold(string) (func(), error) { return func() {}, nil }
func (quietPublicationCoordinator) Wake(string)                 {}
func (quietPublicationCoordinator) Stop()                       {}
