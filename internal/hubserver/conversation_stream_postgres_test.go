package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationPostgresStreamReauthorizesAgainstTenantProject(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		Conversation: &ConversationConfig{Enabled: true},
	})
	organization := nativeOrganization{ID: "org_stream", Name: "Example organization", Slug: "example-org"}
	seedPostgresHubAdmin(t, repository, service, organization)
	projectID := tracker.ProjectID("project_stream")
	now := service.config.now().UTC()
	conversationID := conversation.NewConversationID()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(writer tenantstore.Writer) error {
		if _, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{
			Core:   corestore.Project{OrganizationID: organization.ID, ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native", CreatedAt: now},
			States: nativeFixtureStates(),
		}); err != nil {
			return fmt.Errorf("create project record: %w", err)
		}
		_, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID:   organization.ID,
			ID:               conversationID,
			ProjectID:        projectID,
			OwnerPrincipalID: "test_hub_admin",
			Origin:           conversationOriginUser,
			Title:            "Example conversation",
			Visibility:       conversation.VisibilityShared,
			Status:           conversation.StatusActive,
			Revision:         1,
			Execution:        conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now},
			Preferences:      conversation.DefaultPreferences(),
			CreatedAt:        now,
			UpdatedAt:        now,
		})
		return err
	}); err != nil {
		t.Fatalf("seed PostgreSQL conversation: %v", err)
	}

	database := service.database
	db, reader, authReader, claimReader, postgres := database.db, database.reader, database.authReader, database.claimReader, database.postgres
	database.db, database.reader, database.authReader, database.claimReader, database.postgres = nil, nil, nil, nil, repository
	t.Cleanup(func() {
		database.db, database.reader, database.authReader, database.claimReader, database.postgres = db, reader, authReader, claimReader, postgres
	})
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	path := fmt.Sprintf("%s/api/v2/organizations/%s/projects/%s/conversations/%s/events?after=0", server.URL, url.PathEscape(string(organization.ID)), url.PathEscape(string(projectID)), url.PathEscape(conversationID))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
	transport := &http.Transport{}
	client := &http.Client{Transport: transport}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("open PostgreSQL event stream: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	stream := &sseStream{response: response, reader: bufio.NewReader(response.Body), cancel: cancel}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(writer tenantstore.Writer) error {
		_, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{
			OrganizationID: organization.ID,
			ConversationID: conversationID,
			Event:          conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{"status":"active"}`), CreatedAt: service.config.now().UTC()},
		})
		return err
	}); err != nil {
		t.Fatalf("append PostgreSQL conversation event: %v", err)
	}
	service.conversations.committed(conversationRecord{ID: conversationID})
	frame, err := readSSEFrame(stream.reader)
	if err != nil {
		t.Fatalf("read PostgreSQL conversation event after HTTP 200: %v", err)
	}
	if frame.ID != "1" || frame.Event != string(conversation.EventConversationUpdated) {
		t.Fatalf("first event frame = %#v, want conversation.updated seq 1", frame)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(writer tenantstore.Writer) error {
		return writer.RevokeAccessCredential(t.Context(), "test_hub_admin", service.config.now().UTC())
	}); err != nil {
		t.Fatalf("revoke stream credential: %v", err)
	}
	service.conversations.broker.notify(conversationID)
	for {
		frame, err = readSSEFrame(stream.reader)
		if err != nil {
			t.Fatalf("read stream close after PostgreSQL credential revocation: %v", err)
		}
		if frame.Event == string(conversation.EventClosed) {
			break
		}
	}
	requireClosed(t, frame, conversationClosedAccessRevoked)
}
