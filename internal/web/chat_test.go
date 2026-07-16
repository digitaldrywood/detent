package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestChatQuestionUsesLiveBoardToolAndPersistsSession(t *testing.T) {
	t.Parallel()
	toolCalled := false
	deps := testDeps(t)
	deps.Chat = chatpkg.ProviderFunc(func(ctx context.Context, request chatpkg.TurnRequest) (chatpkg.TurnResponse, error) {
		result, err := request.Handle(ctx, chatpkg.ToolCall{Name: "board_state", Arguments: json.RawMessage(`{"state":"Blocked"}`)})
		if err != nil {
			return chatpkg.TurnResponse{}, err
		}
		toolCalled = strings.Contains(result.Content, "dependency is not merged")
		return chatpkg.TurnResponse{ThreadID: "thread-live", Content: "Issue #1362 is blocked because its dependency is not merged."}, nil
	})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
		Blocked:     []telemetry.Blocked{{Issue: telemetry.Issue{ID: "issue-1362", Identifier: "digitaldrywood/detent#1362", ProjectID: "detent", Title: "Chat", State: "Todo"}, Error: "dependency is not merged", Source: telemetry.BlockedSourceDependency}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "detent_test_token"}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	panel := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/api/v1/chat", headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	if panel.Code != http.StatusOK {
		t.Fatalf("panel status = %d; body = %s", panel.Code, panel.Body.String())
	}
	cookie := panel.Result().Cookies()[0]
	message := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/messages", form: url.Values{"message": {"What is blocked?"}}, cookies: []*http.Cookie{cookie}, headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	if message.Code != http.StatusOK || !strings.Contains(message.Body.String(), "dependency is not merged") {
		t.Fatalf("message status/body = %d/%s", message.Code, message.Body.String())
	}
	if !toolCalled {
		t.Fatal("provider did not receive live board tool output")
	}
	reloaded := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/api/v1/chat", cookies: []*http.Cookie{cookie}, headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	if !strings.Contains(reloaded.Body.String(), "What is blocked?") || !strings.Contains(reloaded.Body.String(), "Issue #1362 is blocked") {
		t.Fatalf("reloaded conversation = %s", reloaded.Body.String())
	}
}

func TestChatMutationRequiresConfirmationAndUsesKanbanHandler(t *testing.T) {
	t.Parallel()
	backend := openWebTestStore(t)
	actionConnector := &kanbanActionConnector{name: "memory"}
	deps := testDeps(t)
	deps.Store = backend
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration}, actionConnector)
	deps.Chat = chatpkg.ProviderFunc(func(ctx context.Context, request chatpkg.TurnRequest) (chatpkg.TurnResponse, error) {
		_, err := request.Handle(ctx, chatpkg.ToolCall{Name: "propose_move_item", Arguments: json.RawMessage(`{"project_id":"detent","identifier":"digitaldrywood/detent#1362","target_state":"Todo"}`)})
		return chatpkg.TurnResponse{ThreadID: "thread-move", Content: "Confirm the proposed move below."}, err
	})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Now().UTC(),
		BoardIssues: []telemetry.Issue{{ID: "issue-1362", Identifier: "digitaldrywood/detent#1362", ProjectID: "detent", Title: "Chat", State: "Backlog"}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "detent_test_token"}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	panel := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/api/v1/chat", headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	cookie := panel.Result().Cookies()[0]
	message := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/messages", form: url.Values{"message": {"Move issue 1362 to Todo"}}, cookies: []*http.Cookie{cookie}, headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	if updates := actionConnector.stateUpdates(); len(updates) != 0 {
		t.Fatalf("state updates before confirmation = %#v", updates)
	}
	match := regexp.MustCompile(`data-chat-action="([^"]+)"`).FindStringSubmatch(message.Body.String())
	if len(match) != 2 || !strings.Contains(message.Body.String(), "Confirmation required") {
		t.Fatalf("proposal response = %s", message.Body.String())
	}

	confirmed := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/actions/" + match[1] + "/confirm", cookies: []*http.Cookie{cookie}, headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	updates := actionConnector.stateUpdates()
	if len(updates) != 1 || updates[0].issueID != "issue-1362" || updates[0].state != "Todo" {
		t.Fatalf("state updates after confirmation = %#v", updates)
	}
	if !strings.Contains(confirmed.Body.String(), "Executed") || !strings.Contains(confirmed.Body.String(), "Action source: chat") {
		t.Fatalf("confirmation response = %s", confirmed.Body.String())
	}

	activityBackend, ok := backend.(store.ActivityStore)
	if !ok {
		t.Fatalf("backend = %T, want store.ActivityStore", backend)
	}
	events, err := activityBackend.ListIssueActivity(context.Background(), store.IssueActivityQuery{ProjectID: "detent", IssueID: "issue-1362", Limit: 20})
	if err != nil {
		t.Fatalf("ListIssueActivity() error = %v", err)
	}
	found := false
	for _, event := range events {
		if event.Kind == string(store.WorkflowPhaseTypeOperatorAction) && event.Name == string(chatpkg.ActionMoveItem) && event.Reason == "chat" {
			found = true
		}
	}
	if !found {
		t.Fatalf("activity events = %#v, want chat operator action", events)
	}
}

func TestChatPriorityRequiresConfirmationAndUsesPriorityHandler(t *testing.T) {
	t.Parallel()
	backend := openWebTestStore(t)
	priorityConnector := &chatPriorityConnector{kanbanActionConnector: &kanbanActionConnector{name: "memory"}}
	deps := testDeps(t)
	deps.Store = backend
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration}, priorityConnector)
	deps.Chat = chatpkg.ProviderFunc(func(ctx context.Context, request chatpkg.TurnRequest) (chatpkg.TurnResponse, error) {
		_, err := request.Handle(ctx, chatpkg.ToolCall{Name: "propose_set_priority", Arguments: json.RawMessage(`{"project_id":"detent","identifier":"digitaldrywood/detent#1362","priority":"High"}`)})
		return chatpkg.TurnResponse{ThreadID: "thread-priority", Content: "Confirm the priority update below."}, err
	})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Now().UTC(),
		BoardIssues: []telemetry.Issue{{ID: "issue-1362", Identifier: "digitaldrywood/detent#1362", ProjectID: "detent", Title: "Chat", State: "Todo"}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "detent_test_token"}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	panel := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/api/v1/chat", headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	cookie := panel.Result().Cookies()[0]
	message := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/messages", form: url.Values{"message": {"Set issue 1362 to high priority"}}, cookies: []*http.Cookie{cookie}, headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	if updates := priorityConnector.priorityUpdates(); len(updates) != 0 {
		t.Fatalf("priority updates before confirmation = %#v", updates)
	}
	match := regexp.MustCompile(`data-chat-action="([^"]+)"`).FindStringSubmatch(message.Body.String())
	if len(match) != 2 {
		t.Fatalf("proposal response = %s", message.Body.String())
	}

	confirmed := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/actions/" + match[1] + "/confirm", cookies: []*http.Cookie{cookie}, headers: map[string]string{"Authorization": "Bearer detent_test_token"}})
	updates := priorityConnector.priorityUpdates()
	if len(updates) != 1 || updates[0].issueID != "issue-1362" || updates[0].field != "Priority" || updates[0].value != "High" {
		t.Fatalf("priority updates after confirmation = %#v", updates)
	}
	if !strings.Contains(confirmed.Body.String(), "Executed") || !strings.Contains(confirmed.Body.String(), "priority to High via chat") {
		t.Fatalf("confirmation response = %s", confirmed.Body.String())
	}
}

func TestChatConfirmationEnforcesAPIKeyProjectScope(t *testing.T) {
	t.Parallel()
	backend := openWebTestStore(t)
	actionConnector := &kanbanActionConnector{name: "memory"}
	deps := testDeps(t)
	deps.Store = backend
	mustSetKanbanProject(t, deps.Registry, "beta", workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration}, actionConnector)
	deps.Chat = chatpkg.ProviderFunc(func(ctx context.Context, request chatpkg.TurnRequest) (chatpkg.TurnResponse, error) {
		_, err := request.Handle(ctx, chatpkg.ToolCall{Name: "propose_move_item", Arguments: json.RawMessage(`{"project_id":"beta","identifier":"digitaldrywood/beta#1","target_state":"Todo"}`)})
		return chatpkg.TurnResponse{Content: "Confirm the move below."}, err
	})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Now().UTC(),
		BoardIssues: []telemetry.Issue{{ID: "issue-beta-1", Identifier: "digitaldrywood/beta#1", ProjectID: "beta", Title: "Scoped", State: "Backlog"}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	created, err := apikey.NewService(backend).Create(context.Background(), apikey.CreateRequest{
		Name:       "alpha-only",
		Scopes:     []string{string(apikey.ScopeWrite)},
		ProjectIDs: []string{"alpha"},
		ExpiresIn:  "90d",
	})
	if err != nil {
		t.Fatalf("Create() API key error = %v", err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token"}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	restrictedAuth := map[string]string{"Authorization": "Bearer " + created.Token}
	panel := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/api/v1/chat", headers: restrictedAuth})
	cookie := panel.Result().Cookies()[0]
	message := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/messages", form: url.Values{"message": {"Move beta issue 1 to Todo"}}, cookies: []*http.Cookie{cookie}, headers: restrictedAuth})
	match := regexp.MustCompile(`data-chat-action="([^"]+)"`).FindStringSubmatch(message.Body.String())
	if len(match) != 2 {
		t.Fatalf("proposal response = %s", message.Body.String())
	}

	restricted := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/actions/" + match[1] + "/confirm", cookies: []*http.Cookie{cookie}, headers: restrictedAuth})
	if restricted.Code != http.StatusForbidden || !strings.Contains(restricted.Body.String(), "not allowed for project beta") {
		t.Fatalf("restricted confirmation = %d/%s", restricted.Code, restricted.Body.String())
	}
	if updates := actionConnector.stateUpdates(); len(updates) != 0 {
		t.Fatalf("state updates after restricted confirmation = %#v", updates)
	}

	allowed := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/chat/actions/" + match[1] + "/confirm", cookies: []*http.Cookie{cookie}, headers: map[string]string{"Authorization": "Bearer detent_admin_token"}})
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed confirmation = %d/%s", allowed.Code, allowed.Body.String())
	}
	if updates := actionConnector.stateUpdates(); len(updates) != 1 || updates[0].issueID != "issue-beta-1" || updates[0].state != "Todo" {
		t.Fatalf("state updates after allowed confirmation = %#v", updates)
	}
}

type chatPriorityUpdate struct {
	issueID string
	field   string
	value   string
}

type chatPriorityConnector struct {
	*kanbanActionConnector
	priority []chatPriorityUpdate
}

func (c *chatPriorityConnector) SetField(_ context.Context, issueID string, field string, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.priority = append(c.priority, chatPriorityUpdate{issueID: issueID, field: field, value: value})
	return nil
}

func (c *chatPriorityConnector) priorityUpdates() []chatPriorityUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]chatPriorityUpdate(nil), c.priority...)
}
