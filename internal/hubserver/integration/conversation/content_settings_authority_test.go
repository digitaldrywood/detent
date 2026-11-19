package conversation_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudContentSettingsAdministration(t *testing.T) {
	t.Parallel()
	h := hubfixture.OpenConversationHostedEntry(t)
	path := h.Base + "/content-settings"
	for _, role := range []string{"viewer", "member", "reporter"} {
		t.Run(role, func(t *testing.T) {
			user := h.User(t, "settings-"+role, role, role+"@example.test", "write", "")
			request := conversations.ContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: role}, ContentChange: projectsettings.ContentChange{RequireVisualAttachments: true}}
			response := h.BrowserRequest(t, user, role, http.MethodPut, path, request)
			if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
				t.Fatalf("non-administrator status=%d %s", response.Code, response.Body)
			}
		})
	}
	var revision tracker.Revision
	for _, test := range []struct {
		role    string
		require bool
	}{{"owner", true}, {"admin", false}} {
		t.Run(test.role, func(t *testing.T) {
			user := h.User(t, "settings-"+test.role, test.role, test.role+"@example.test", "write", "")
			request := conversations.ContentSettingsRequest{
				Mutation:      tracker.Mutation{IdempotencyKey: test.role},
				ContentChange: projectsettings.ContentChange{ExpectedRevision: revision, RequireVisualAttachments: test.require},
			}
			response := h.BrowserRequest(t, user, test.role, http.MethodPut, path, request)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var saved projectsettings.Content
			hubfixture.Decode(t, response, &saved)
			if saved.RequireVisualAttachments != test.require || saved.Revision != revision+1 {
				t.Fatalf("saved content settings = %+v", saved)
			}
			revision = saved.Revision
		})
	}
}
