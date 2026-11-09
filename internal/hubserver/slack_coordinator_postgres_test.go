package hubserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSlackCoordinatorPostgresScopeAndReopen(t *testing.T) {
	repository := projectRecordPostgres(t)
	type source struct {
		fixture    hostedSecurityFixture
		owner      hostedSecurityUser
		records    []string
		connection json.RawMessage
		channel    string
	}
	sources := []source{}
	bind := func(source *source) {
		t.Helper()
		service := source.fixture.service
		path := "/api/v2/organizations/:organization/slack-coordinator-context"
		service.echo.GET(path, func(c echo.Context) error {
			ctx, err := service.bindCoordinatorConnection(c.Request().Context(), "slack-coordinator-test")
			if err != nil {
				return err
			}
			source.connection = coordinatorConnectionData(ctx)
			return c.NoContent(http.StatusOK)
		}, service.operatorAuthority)
		path = "/api/v2/organizations/" + service.config.Hosted.OrganizationID + "/slack-coordinator-context"
		requireNativeStatus(t, source.fixture.request(t, source.owner, http.MethodGet, path, nil), http.StatusOK)
		if len(source.connection) == 0 {
			t.Fatal("missing authenticated coordinator connection")
		}
	}
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f, owner := newChangePolicyPostgresTenantFixture(t, repository, org, &memoryRunDetails{})
		cfg := f.service.config
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir(), ArchiveContents: &conversationContentFixture{}}
		cfg.runMaintenance = waitForMaintenanceFixtureShutdown
		logs := &conversationLogSink{}
		cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		if err := f.service.Close(); err != nil {
			t.Fatal(err)
		}
		service, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.Close() })
		f.service = service
		current := source{fixture: f, owner: owner, channel: []string{"#first", "#second"}[index]}
		base := "/api/v2/organizations/" + string(org) + "/integrations/slack"
		requireNativeStatus(t, f.request(t, owner, http.MethodPut, base, map[string]string{"webhook": slackTestWebhook, "channel_name": current.channel}), http.StatusOK)
		for _, key := range []string{"first", "second"} {
			response := f.request(t, owner, http.MethodPost, f.base+"/conversations", map[string]string{"key": key})
			if response.Code != http.StatusCreated {
				t.Logf("conversation diagnostics: %s", logs.text())
			}
			requireNativeStatus(t, response, http.StatusCreated)
			var created struct {
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			}
			decodeHubResponse(t, response, &created)
			current.records = append(current.records, created.Conversation.ID)
		}
		sources = append(sources, current)
	}
	for i := range sources {
		bind(&sources[i])
	}
	check := func(index int) {
		t.Helper()
		source := &sources[index]
		coordinator := source.fixture.service.conversations.coordinator.(*conversationTurnCoordinator)
		for _, id := range source.records {
			state := &coordinatorTurnState{coordinator: coordinator, conversationID: id}
			tools := newCoordinatorToolset(coordinator, state)
			call := runner.AgentToolCall{Name: "get_slack_integration", Arguments: json.RawMessage(`{}`)}
			result, err := tools.handle(t.Context(), call)
			if err != nil || result.Success {
				t.Fatalf("unauthenticated tool result=%+v err=%v", result, err)
			}
			state.users = []conversationMessageRecord{{Role: conversation.RoleUser, Text: "Read Slack settings", Data: source.connection}}
			result, err = tools.handle(t.Context(), call)
			if err != nil || !result.Success {
				t.Fatalf("Slack tool result=%+v err=%v", result, err)
			}
			var status slackIntegrationStatus
			if err := json.Unmarshal([]byte(result.Content), &status); err != nil {
				t.Fatal(err)
			}
			if status.ChannelName != source.channel || status.Webhook == "" || strings.Contains(result.Content, slackTestWebhook) || strings.Contains(result.Content, "ciphertext") {
				t.Fatalf("wrong or exposed Slack settings: %+v", status)
			}
			state.users[0].Data = sources[1-index].connection
			result, err = tools.handle(t.Context(), call)
			if err != nil || result.Success {
				t.Fatalf("foreign tool result=%+v err=%v", result, err)
			}
		}
	}
	for index := range sources {
		check(index)
	}
	for index := range sources {
		source := &sources[index]
		cfg := source.fixture.service.config
		if err := source.fixture.service.Close(); err != nil {
			t.Fatal(err)
		}
		service, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.Close() })
		source.fixture.service = service
		check(index)
	}
}
