package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorProjectActions(t *testing.T) {
	for _, outcome := range []string{"execute", "revoked", "stale", "wrong role", "foreign issue", "bad arguments"} {
		t.Run(outcome, func(t *testing.T) {
			f := newHostedSecurityFixture(t, func(cfg *Config) {
				cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir()}
			})
			u := f.user(t, "luna-owner", "owner", "luna@example.test", "write", "")
			response := f.request(t, u, http.MethodPost, f.base+"/work-items", map[string]any{"idempotency_key": "luna-issue", "title": "Original issue", "state": "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			seedCoordinatorRepository(t, f.service, f.project)
			response = f.request(t, u, http.MethodPost, f.base+"/conversations", map[string]any{"key": "luna-conversation", "first_message": map[string]any{"key": "luna-message", "text": "Please move this issue"}})
			requireNativeStatus(t, response, http.StatusCreated)
			var created struct {
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			}
			decodeHubResponse(t, response, &created)
			coordinator := f.service.conversations.coordinator.(*conversationTurnCoordinator)
			record, err := coordinator.readConversation(t.Context(), f.service.database.db, created.Conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := f.service.conversations.store.listMessages(t.Context(), f.service.database.db, record.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var user conversationMessageRecord
			for _, message := range messages {
				if message.Role == conversation.RoleUser {
					user = message
				}
			}
			tools := newCoordinatorToolset(coordinator, &coordinatorTurnState{coordinator: coordinator, conversationID: record.ID, users: []conversationMessageRecord{user}})
			arguments, _ := json.Marshal(map[string]string{"work_item_id": string(issue.WorkItemID), "state": "Done"})
			if _, err := tools.projectAction(context.Background(), record, runner.AgentToolCall{Name: operatortool.MoveItem, Arguments: arguments}); err != nil {
				t.Fatal(err)
			}
			messages, err = f.service.conversations.store.listMessages(t.Context(), f.service.database.db, record.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var proposed chat.Action
			for _, message := range messages {
				var data struct {
					Action struct {
						Action chat.Action `json:"action"`
					} `json:"operator_action"`
				}
				if json.Unmarshal(message.Data, &data) == nil && data.Action.Action.RequestID != "" {
					proposed = data.Action.Action
				}
			}
			if proposed.RequestID == "" {
				t.Fatal("missing inline call")
			}
			before := f.request(t, u, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), nil)
			var current tracker.NativeIssue
			decodeHubResponse(t, before, &current)
			if current.State != "Todo" {
				t.Fatal("proposal executed before client submission")
			}
			for _, s := range f.service.operatorChat.Conversation(proposed.ConnectionID).Actions {
				if s.Status == chat.ActionPending {
					t.Fatal("pending action stored")
				}
			}
			switch outcome {
			case "revoked":
				operatorSQL(t, f, "UPDATE hosted_sessions SET revoked_at=?", testTimestamp)
			case "wrong role":
				operatorSQL(t, f, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", u.identity.Subject)
			case "stale":
				operatorSQL(t, f, "UPDATE issues SET revision=revision+1 WHERE native_id=?", issue.WorkItemID)
			case "foreign issue":
				proposed.IssueID = "wi_foreign"
			case "bad arguments":
				proposed.Kind = "delete_organization"
			}
			response = f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", proposed)
			if outcome != "execute" {
				if response.Code < 400 {
					t.Fatalf("unauthorized call=%d %s", response.Code, response.Body.String())
				}
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			response = f.request(t, u, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), nil)
			decodeHubResponse(t, response, &current)
			if current.State != "Done" {
				t.Fatalf("client call did not execute=%+v", current)
			}
			requireNativeStatus(t, f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", proposed), http.StatusOK)
		})
	}
}

func seedCoordinatorRepository(t *testing.T, service *Service, project tracker.ProjectID) {
	t.Helper()
	db := service.database.db
	now := formatHubTime(service.config.now())
	if _, err := db.ExecContext(t.Context(), "INSERT INTO repositories(github_node_id,github_owner,github_name,created_at,updated_at) VALUES ('luna_repo','example','repo',?,?)", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "DELETE FROM projects WHERE repository_id=(SELECT id FROM repositories WHERE github_node_id='luna_repo')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE projects SET repository_id=(SELECT id FROM repositories WHERE github_node_id='luna_repo'), github_repository_enabled=0 WHERE id=?", project); err != nil {
		t.Fatal(err)
	}
}

func (f *browserHostedFixture) seedCoordinatorActions(t *testing.T) {
	t.Helper()
	seedCoordinatorRepository(t, f.service, tracker.ProjectID(f.project))
	var issue tracker.NativeIssue
	base := browserHostedOrganizationBase + "/projects/" + f.project
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/work-items", map[string]any{"idempotency_key": "luna-blocked", "title": "Landing blocked by branch rules", "body": "Enable GitHub pull-request mode and retry.", "state": "Blocked"}, http.StatusOK), &issue)
	f.workItem = string(issue.WorkItemID)
	backend := f.service.conversations.config.Backend.(*fakeCoordinatorBackend)
	backend.setRun(func(ctx context.Context, turn int, handle runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		prompt := strings.ToLower(backend.request(t, turn-1).Prompt)
		call := runner.AgentToolCall{Name: "update_project_integration", Arguments: json.RawMessage(`{"repository_enabled":true}`)}
		if strings.Contains(prompt, "disable the sprite pool") {
			call = runner.AgentToolCall{Name: "set_sprite_pool", Arguments: json.RawMessage(`{"min_runners":0,"max_runners":0}`)}
		}
		if strings.Contains(prompt, "retry the blocked issue") {
			raw, err := json.Marshal(map[string]string{"work_item_id": f.workItem, "state": "Todo"})
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			call = runner.AgentToolCall{Name: operatortool.MoveItem, Arguments: raw}
		}
		result, err := handle(ctx, call)
		if err != nil {
			return runner.AgentTurnResult{}, err
		}
		text := "Review the exact change below and confirm it to continue."
		if !result.Success {
			text = "The change was refused: " + result.Content
		}
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: text}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{}, nil
	})
}
