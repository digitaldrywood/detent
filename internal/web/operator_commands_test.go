package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

const modernOperatorMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"portable-client","version":"1"},"yolo":true,"principal_id":"forged-admin","organization_id":"forged-org","connection_id":"forged-session"}`

func hasMutationAudit(logs, outcome string) bool {
	for _, line := range strings.Split(logs, "\n") {
		var log struct {
			Audit string `json:"audit"`
		}
		if json.Unmarshal([]byte(line), &log) != nil {
			continue
		}
		var audit struct {
			Principal   string `json:"principal_id"`
			Correlation string `json:"correlation_id"`
			Source      string `json:"source"`
			Outcome     string `json:"outcome"`
		}
		if json.Unmarshal([]byte(log.Audit), &audit) != nil {
			continue
		}
		if audit.Principal != "" && audit.Correlation != "" && audit.Source == "mcp" && audit.Outcome == outcome {
			return true
		}
	}
	return false
}

// Catches duplicate provider turns across reconnects, unrelated browser history
// selection, and a nested chat model approving or proposing operator mutations.
func TestMCPOperatorChatRetry(t *testing.T) {
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now().UTC(), BoardIssues: []telemetry.Issue{
		{ID: "visible", ProjectID: "detent", Title: "Visible issue"},
		{ID: "foreign", ProjectID: "other", Title: "foreign-project-sentinel"},
	}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	deps.Chat = chatpkg.ProviderFunc(func(ctx context.Context, request chatpkg.TurnRequest) (chatpkg.TurnResponse, error) {
		calls++
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "propose_") {
				t.Fatal("nested MCP chat advertised operator proposals")
			}
		}
		if _, err := request.Handle(ctx, chatpkg.ToolCall{Name: "propose_file_issue", Arguments: json.RawMessage(`{"project_id":"detent"}`)}); err == nil {
			t.Fatal("nested chat proposed an operator mutation")
		}
		read, err := request.Handle(ctx, chatpkg.ToolCall{Name: "board_state", Arguments: json.RawMessage(`{}`)})
		if err != nil || strings.Contains(read.Content, "foreign-project-sentinel") || !strings.Contains(read.Content, "Visible issue") {
			t.Fatalf("nested read escaped project grants: %s %v", read.Content, err)
		}
		if _, err := request.Handle(ctx, chatpkg.ToolCall{Name: "board_state", Arguments: json.RawMessage(`{"project_id":"other"}`)}); err == nil {
			t.Fatal("nested chat read a foreign project")
		}
		return chatpkg.TurnResponse{Content: "This connection's answer."}, nil
	})
	server, err := web.NewServer(web.Config{ServerAddress: "127.0.0.1:0", GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token"}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	token, _ := createRemoteMCPKey(t, server, "Chat authority", []string{"write"}, []string{"detent"})
	headers := map[string]string{"Authorization": "Bearer " + token}
	connect := func() {
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, headers)
		var setup struct {
			ID string `json:"connection_id"`
		}
		if json.Unmarshal(response.Body.Bytes(), &setup) != nil || setup.ID == "" {
			t.Fatalf("setup=%s", response.Body.String())
		}
		headers["X-Detent-Connection-ID"] = setup.ID
	}
	connect()
	args := `{"project_id":"detent","request_id":"turn","message":"What is happening?"}`
	first := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/post_operator_chat", args, headers)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "This connection's answer.") {
		t.Fatalf("post=%d %s", first.Code, first.Body.String())
	}
	connect()
	replay := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/post_operator_chat", args, headers)
	if replay.Code != http.StatusOK || calls != 1 {
		t.Fatalf("replay=%d %s provider calls=%d", replay.Code, replay.Body.String(), calls)
	}
	changed := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/post_operator_chat", strings.Replace(args, "What is happening?", "Changed", 1), headers)
	if changed.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("changed=%d %s", changed.Code, changed.Body.String())
	}
	history := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/get_operator_chat", `{"session_id":"foreign-browser"}`, headers)
	if history.Code != http.StatusBadRequest {
		t.Fatalf("foreign history=%d %s", history.Code, history.Body.String())
	}
	history = performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/get_operator_chat", `{}`, headers)
	if history.Code != http.StatusOK || strings.Contains(history.Body.String(), "This connection's answer.") {
		t.Fatalf("reconnected history=%d %s", history.Code, history.Body.String())
	}
}

// These implement the dashboard's existing tracker boundaries and record effects.
type mcpWorkConnector struct {
	*kanbanActionConnector
	created    []connector.Issue
	priorities []string
}

func (c *mcpWorkConnector) UpsertIssues(_ context.Context, issues []connector.Issue) error {
	c.created = append(c.created, issues...)
	return nil
}
func (c *mcpWorkConnector) SetField(_ context.Context, id, field, value string) error {
	c.priorities = append(c.priorities, id+" "+field+" "+value)
	return nil
}
