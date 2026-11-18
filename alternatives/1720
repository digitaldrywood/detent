package hubserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f conversationFixture) readTenant(ctx context.Context, read func(tenantstore.Reader) error) error {
	return f.service.config.TenantRepository.ReadTenant(ctx, f.organization, corestore.Serving, read)
}

func (f conversationFixture) readRecord(ctx context.Context, project tracker.ProjectID, id string) (record conversationRecord, err error) {
	err = f.readTenant(ctx, func(r tenantstore.Reader) error {
		value, err := r.Conversation(ctx, project, id)
		record = conversationRecordFromStore(value)
		return err
	})
	return
}

func (f conversationFixture) recordChat(organization tracker.OrganizationID) *conversationService {
	return f.recordServer(organization).conversations
}

func (f conversationFixture) recordServer(organization tracker.OrganizationID) *Service {
	server := &Service{config: f.service.config, database: f.service.database, localOrganization: string(organization)}
	server.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(organization)}
	chat := &conversationService{Server: server.conversationServer(), Store: f.store, Broker: newNotificationBroker().Broker, Coordinator: &recordingWaker{}, Controls: &recordingWaker{}}
	server.conversations = chat
	return server
}

func (f conversationFixture) publishedRecordMessages(ctx context.Context, record conversationRecord) ([]conversationMessageRecord, error) {
	server := f.recordServer(record.OrganizationID)
	return (&conversationTurnCoordinator{server: server, service: server.conversations}).coordinatorMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100})
}

func (f conversationFixture) publishedRecordEvents(ctx context.Context, record conversationRecord, after int64) ([]conversation.Event, error) {
	chat := f.recordChat(record.OrganizationID)
	response := httptest.NewRecorder()
	stream := newConversationStream(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response))
	var credential accessstore.Credential
	if err := f.service.config.TenantRepository.ReadTenant(ctx, record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		credential, err = r.AccessCredential(ctx, record.OwnerPrincipalID)
		return err
	}); err != nil {
		return nil, err
	}
	scope := hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: hubapi.APICredential{ID: credential.ID, Hash: credential.Hash, Scope: hubapi.APIScopeOperator}}
	current, err := chat.Server.ReadPGConversation(ctx, conversationInput(scope), record.ID)
	if err != nil {
		return nil, err
	}
	if _, err := chat.Server.ReplayPGConversationEvents(ctx, conversationInput(scope), stream, record.ID, after, current.EventSeq); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(response.Body.Bytes()))
	var events []conversation.Event
	for {
		frame, err := readSSEFrame(reader)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return nil, err
		}
		seq, err := strconv.ParseInt(frame.ID, 10, 64)
		if err != nil {
			return nil, err
		}
		events = append(events, conversation.Event{Seq: seq, Type: conversation.EventType(frame.Event), Data: json.RawMessage(frame.Data)})
	}
}

func conversationFixturePrincipal(t *testing.T, f nativeFixture, name string) hubapi.TokenResponse {
	t.Helper()
	token := hubapi.TokenResponse{ID: hubapi.NewID("credential"), Token: hubapi.NewID("token")}
	now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
	err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: token.ID, Name: name, Hash: apikey.HashToken(token.Token), Scope: "operator", Kind: "service", NativeOnly: true, ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: token.ID, ProjectID: f.project.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}
