package conversation_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresConversationArchiveAuthorization(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewNative(t, openConversationTestHub(t), "", "Archive privacy")
	owner := hubtest.Credential(t, f.Hub.Repository, f.Organization(), hubtest.CredentialOptions{Scope: "admin", AllProject: true})
	other := hubtest.Credential(t, f.Hub.Repository, f.Organization(), hubtest.CredentialOptions{Scope: "admin", AllProject: true})
	now := f.Hub.Now().UTC()
	cases := []struct {
		name   string
		owner  string
		id     string
		status int
	}{
		{"owned archive", owner.ID, conversation.NewConversationID(), http.StatusOK},
		{"foreign private archive", other.ID, conversation.NewConversationID(), http.StatusNotFound},
	}
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Organization(), corestore.Serving, func(w tenantstore.Writer) error {
		for _, test := range cases {
			if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{
				OrganizationID: f.Organization(), ProjectID: f.Project.ID, ID: test.id, OwnerPrincipalID: test.owner,
				Origin: conversations.ConversationOriginUser, Title: test.name, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1,
				Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := hubfixture.NewRequest(t, http.MethodGet, f.Base+"/conversations/"+test.id, owner.Token, nil)
			request.Header.Set("X-Detent-Archive-Read", "1")
			response := httptest.NewRecorder()
			f.Hub.Handler().ServeHTTP(response, request)
			hubfixture.RequireStatus(t, response, test.status)
			if test.status == http.StatusOK {
				var snapshot conversations.ConversationSnapshot
				hubfixture.Decode(t, response, &snapshot)
				if snapshot.Conversation.ID != test.id || snapshot.Conversation.Title != test.name {
					t.Fatalf("archive returned a different conversation: %+v", snapshot.Conversation)
				}
			}
		})
	}
}
