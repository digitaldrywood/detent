package hubserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestCoordinatorPublicationPreservesEventSequence(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := newConversationTenantFixture(t, repository, now)
	organization, project := fixture.project.OrganizationID, fixture.project.ID
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "example_owner", Name: "Example owner", Hash: "example-owner-hash", Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &conversationTurnCoordinator{service: fixture.service.conversations}
	future := now.Add(time.Hour)
	for _, tc := range []struct {
		name              string
		lastMessageAt     *time.Time
		wantLastMessageAt time.Time
	}{{"first", nil, now}, {"second", &future, future}} {
		t.Run(tc.name, func(t *testing.T) {
			id := conversation.NewConversationID()
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: project, ID: id, OwnerPrincipalID: "example_owner", Origin: conversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now, LastMessageAt: tc.lastMessageAt})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: id, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: now, UpdatedAt: now}
			execution := conversation.Execution{Status: conversation.ExecutionRunning, UpdatedAt: now}
			if err := coordinator.write(t.Context(), id, func(ctx context.Context, w tenantstore.Writer, record *conversationRecord, now time.Time) error {
				if err := coordinator.appendMessage(ctx, w, record, &message, now); err != nil {
					return err
				}
				return coordinator.updateExecution(ctx, w, record, execution, now)
			}, func(ctx context.Context, plan *conversationPublicationPlan) error {
				if err := plan.appendMessage(ctx, &message); err != nil {
					return err
				}
				return plan.updateExecution(ctx, execution)
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
			var content conversationContent
			if err := fixture.service.readConversationObject(t.Context(), string(organization), snapshotReference, &content); err != nil {
				t.Fatal(err)
			}
			var snapshot conversationResource
			if err := json.Unmarshal(content.Data, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.MessageCount != 1 || snapshot.EventSeq != 1 {
				t.Fatalf("published snapshot count/sequence = %d/%d", snapshot.MessageCount, snapshot.EventSeq)
			}
			if snapshot.LastMessageAt == nil || !snapshot.LastMessageAt.Equal(tc.wantLastMessageAt) {
				t.Fatalf("published last message = %v, want %v", snapshot.LastMessageAt, tc.wantLastMessageAt)
			}
			if err := fixture.service.readConversationObject(t.Context(), string(organization), messageReference, &content); err != nil {
				t.Fatal(err)
			}
			var accepted conversationMessageResource
			if err := json.Unmarshal(content.Data, &accepted); err != nil {
				t.Fatal(err)
			}
			if accepted.ID != message.ID {
				t.Fatalf("published message identity = %s", accepted.ID)
			}
			response := httptest.NewRecorder()
			stream := newConversationStream(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response))
			scope := nativeScope{organization: organization, project: project, credential: apiCredential{ID: "example_owner", Hash: "example-owner-hash", Scope: apiScopeOperator}}
			if _, err := fixture.service.replayPGConversationEvents(t.Context(), scope, stream, id, 0, 3); err != nil {
				t.Fatal(err)
			}
			frame, err := readSSEFrame(bufio.NewReader(bytes.NewReader(response.Body.Bytes())))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(frame.Data), &accepted); err != nil {
				t.Fatal(err)
			}
			if accepted.ID != message.ID || accepted.Seq != 1 {
				t.Fatalf("replayed message identity/sequence = %s/%d", accepted.ID, accepted.Seq)
			}
		})
	}
}
