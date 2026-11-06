package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresConversationArchiveAuthorization(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, now: time.Now, Logger: discardLogger()}}
	service.conversations = &conversationService{server: service, store: &conversationStore{}}
	organization := nativeOrganization{ID: "org_archive", Name: "Example organization", Slug: "example-org"}
	seedPostgresHubAdmin(t, repository, service, organization)
	now := service.config.now().UTC()
	cases := []struct {
		name   string
		owner  string
		id     string
		status int
	}{
		{"first", "test_hub_admin", conversation.NewConversationID(), http.StatusOK},
		{"second", "test_hub_admin", conversation.NewConversationID(), http.StatusOK},
		{"private", "other_owner", conversation.NewConversationID(), http.StatusNotFound},
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization.ID, ID: "project_archive", Name: "Example project", Slug: "example-project", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{
			OrganizationID: organization.ID, ID: "other_owner", Name: "Other conversation owner",
			Hash: apikey.HashToken("example-other-owner-token"), Scope: "admin", KeyScope: "admin",
			ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		for _, tc := range cases {
			_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization.ID, ProjectID: "project_archive", ID: tc.id, OwnerPrincipalID: tc.owner, Origin: conversationOriginUser, Title: tc.name, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now})
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := fmt.Sprintf("/api/v2/organizations/%s/projects/project_archive/conversations/%s", organization.ID, tc.id)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+testHubAdminToken)
			req.Header.Set("X-Detent-Archive-Read", "1")
			response := httptest.NewRecorder()
			c := echo.New().NewContext(req, response)
			c.SetParamNames("conversation")
			c.SetParamValues(tc.id)
			c.Set("native_scope", nativeScope{organization: organization.ID, project: "project_archive", credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}})
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("archive authorization panicked: %v", value)
				}
			}()
			if err := service.getConversation(c); err != nil {
				t.Fatalf("archive conversation read: %v", err)
			}
			if response.Code != tc.status {
				t.Fatalf("archive conversation status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
			}
			if tc.status == http.StatusOK {
				var snapshot conversationSnapshot
				if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.Conversation.ID != tc.id || snapshot.Conversation.Title != tc.name {
					t.Fatalf("conversation snapshot=%+v", snapshot.Conversation)
				}
			}
		})
	}
}
