package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestConversationRemovalUsesAudienceAndArchivedContent(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	objects := &conversationContentFixture{}
	service := &Service{database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, Logger: discardLogger(), now: func() time.Time { return now }}}
	service.conversations = &conversationService{server: service, store: &conversationStore{contents: objects}, broker: newNotificationBroker(), coordinator: &recordingWaker{}}
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org_removal", Name: "Example", Slug: "example"})
	scope := nativeScope{organization: "org_removal", project: "prj_removal", credential: apiCredential{ID: "test_hub_admin", Scope: apiScopeAdmin, Hash: apikey.HashToken(testHubAdminToken)}}
	payload, err := json.Marshal(conversationRecordContent{Execution: conversation.Execution{Status: conversation.ExecutionIdle}, Preferences: conversation.Preferences{Model: "example-model", ReasoningEffort: "auto", Access: "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := messageContent(conversationMessageRecord{Data: payload})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := service.conversations.store.publishPayload(t.Context(), string(scope.organization), wrapped)
	if err != nil {
		t.Fatal(err)
	}
	contentID := contentArchiveID(string(scope.organization), ref)
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: scope.project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, Settings: projectstore.Settings{OrganizationID: scope.organization, ProjectID: scope.project}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		allowed bool
		running bool
	}{{"matching audience", true, false}, {"different audience", false, false}, {"typed running overrides archived idle", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			id := conversation.NewConversationID()
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				audience, err := conversationReadAudienceRecord(t.Context(), w, scope.organization, scope.credential)
				if err != nil {
					return err
				}
				if !test.allowed {
					audience = "different"
				}
				if err := w.PutConversationContent(t.Context(), conversationstore.ContentObject{OrganizationID: scope.organization, ID: contentID, Reference: ref, CreatedAt: now}); err != nil {
					return err
				}
				execution := conversation.Execution{Status: conversation.ExecutionIdle}
				if test.running {
					execution.Status = conversation.ExecutionRunning
				}
				_, err = w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: scope.organization, ProjectID: scope.project, ID: id, OwnerPrincipalID: scope.credential.ID, Origin: conversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: execution, Preferences: conversation.Preferences{Model: "current-model", ReasoningEffort: "auto", Access: "auto"}, ReadAudience: audience, ContentReference: contentID, CreatedAt: now, UpdatedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, read := range []struct {
				name string
				call func() (conversationRecord, error)
			}{
				{"record", func() (conversationRecord, error) { return service.readPGConversation(t.Context(), scope, id) }},
				{"page", func() (conversationRecord, error) {
					page, err := service.readPGConversationPage(t.Context(), scope, id, 0, 10)
					return page.record, err
				}},
			} {
				record, err := read.call()
				if !test.allowed {
					var denied *nativeError
					if !errors.As(err, &denied) || denied.status != http.StatusNotFound {
						t.Fatalf("%s different audience error = %v", read.name, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s matching audience: %v", read.name, err)
				}
				wantStatus := conversation.ExecutionIdle
				if test.running {
					wantStatus = conversation.ExecutionRunning
				}
				if record.Execution.Status != wantStatus || record.Preferences.Model != "current-model" {
					t.Fatalf("%s current state = %+v", read.name, record)
				}
			}
			call := func(method string, handler echo.HandlerFunc) *httptest.ResponseRecorder {
				t.Helper()
				response := httptest.NewRecorder()
				ctx := echo.New().NewContext(httptest.NewRequest(method, "/conversations/"+id, nil), response)
				ctx.Set("native_scope", scope)
				ctx.SetParamNames("conversation")
				ctx.SetParamValues(id)
				if err := handler(ctx); err != nil {
					t.Fatal(err)
				}
				return response
			}
			if !test.allowed {
				_, err := service.commandPatchConversationPG(t.Context(), scope, id, conversationPatchRequest{}, "Updated")
				var denied *nativeError
				if !errors.As(err, &denied) || denied.status != http.StatusNotFound {
					t.Fatalf("patch different audience error = %v", err)
				}
				stream := &conversationStream{response: echo.NewResponse(httptest.NewRecorder(), echo.New())}
				if _, err := service.replayPGConversationEvents(t.Context(), scope, stream, id, 0, 1); !errors.As(err, &denied) || denied.status != http.StatusNotFound {
					t.Fatalf("replay different audience error = %v", err)
				}
			}
			if !test.allowed {
				requireNativeStatus(t, call(http.MethodPost, service.archiveConversation), http.StatusNotFound)
				requireNativeStatus(t, call(http.MethodDelete, service.deleteConversation), http.StatusNotFound)
				return
			}
			if test.running {
				requireNativeStatus(t, call(http.MethodPost, service.archiveConversation), http.StatusConflict)
				requireNativeStatus(t, call(http.MethodDelete, service.deleteConversation), http.StatusConflict)
				return
			}
			response := call(http.MethodPost, service.archiveConversation)
			requireNativeStatus(t, response, http.StatusOK)
			var record conversationResource
			decodeHubResponse(t, response, &record)
			if !record.Archived || record.Revision != 2 || record.Preferences.Model != "current-model" {
				t.Fatalf("archived record = %+v", record)
			}
			if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.ConversationEvents(t.Context(), id, 0, 10)
				if err != nil {
					return err
				}
				if len(page.Items) != 1 || page.Items[0].ContentReference == "" || string(page.Items[0].Data) != "{}" {
					t.Fatalf("archive event lacks external payload: %+v", page.Items)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.commandPatchConversationPG(t.Context(), scope, id, conversationPatchRequest{}, "Updated"); err != nil {
				t.Fatalf("patch matching audience: %v", err)
			}
			output := httptest.NewRecorder()
			stream := &conversationStream{response: echo.NewResponse(output, echo.New())}
			if cursor, err := service.replayPGConversationEvents(t.Context(), scope, stream, id, 0, 2); err != nil || cursor != 2 {
				t.Fatalf("replay matching audience cursor=%d error=%v", cursor, err)
			}
			requireNativeStatus(t, call(http.MethodDelete, service.deleteConversation), http.StatusNoContent)
			requireNativeStatus(t, call(http.MethodDelete, service.deleteConversation), http.StatusNotFound)
		})
	}
}
