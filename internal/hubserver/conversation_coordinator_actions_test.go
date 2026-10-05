package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorProjectActions(t *testing.T) {
	for _, tool := range []string{"update_project_integration", operatortool.MoveItem, operatortool.EditItem, operatortool.AddComment, "set_sprite_pool", "scale_up_sprite_pool"} {
		outcomes := []string{"approve", "reject", "unauthorized", "revoked", "stale", "model approval", "foreign issue", "expired session", "wrong role", "no write grant", "bad arguments"}
		if tool == "update_project_integration" {
			outcomes = append(outcomes, "transport unavailable")
		}
		if coordinatorSpriteMutation(tool) {
			outcomes = append(outcomes, "no runner grant", "runner grant revoked")
		}
		for _, outcome := range outcomes {
			t.Run(tool+"/"+outcome, func(t *testing.T) {
				f := newHostedSecurityFixture(t, func(cfg *Config) {
					cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir()}
					if coordinatorSpriteMutation(tool) {
						cfg.SecretKeys = secretTestKeys(t, "1", "1")
						cfg.Version = "v0.117.99"
						cfg.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
							if r.Method == http.MethodPost {
								return spritesTestResponse(spritesSecretSentinel, http.StatusPaymentRequired), nil
							}
							return spritesTestResponse(`{"sprites":[]}`, http.StatusOK), nil
						})}
					}
					if tool == "update_project_integration" && outcome != "transport unavailable" {
						cfg.GitHubDisabled = false
						cfg.ReconcileBackend = &scriptedReconcileBackend{}
					}
				})
				u := f.user(t, "luna-owner", "owner", "luna@example.test", "write", "")
				if coordinatorSpriteMutation(tool) {
					f.grant(t, u, true, outcome != "no runner grant")
				}
				response := f.request(t, u, http.MethodPost, f.base+"/work-items", map[string]any{"idempotency_key": "luna-issue", "title": "Original issue", "body": "Original body", "state": "Todo", "labels": []string{"original"}})
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				id := issue.WorkItemID
				db := f.service.database.db
				seedCoordinatorRepository(t, f.service, f.project)
				response = f.request(t, u, http.MethodPost, f.base+"/conversations", map[string]any{"key": "luna-conversation", "first_message": map[string]any{"key": "luna-message", "text": "Please change this project"}})
				requireNativeStatus(t, response, http.StatusCreated)
				var created struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
				}
				decodeHubResponse(t, response, &created)
				c := f.service.conversations.coordinator.(*conversationTurnCoordinator)
				record, err := c.readConversation(t.Context(), db, created.Conversation.ID)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := f.service.conversations.store.listMessages(t.Context(), db, record.ID, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				var user conversationMessageRecord
				for _, message := range messages {
					if message.Role == conversation.RoleUser {
						user = message
					}
				}
				state := &coordinatorTurnState{coordinator: c, conversationID: record.ID, users: []conversationMessageRecord{user}}
				tools := newCoordinatorToolset(c, state)
				if coordinatorSpriteMutation(tool) {
					envelope, err := f.service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), secretAAD("org_security", string(f.project), flySpritesToken))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `INSERT INTO project_secrets(organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES('org_security',?,?,'detent-test',?,?,?,?,?)`, f.project, flySpritesToken, envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, formatHubTime(f.service.config.now())); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `INSERT INTO project_sprite_pools(organization_id,project_id,min_runners,max_runners,idle_seconds,bootstrap,configured_by) SELECT 'org_security',?,0,1,300,'private-provider-secret',principal_id FROM hosted_members WHERE user_id=?`, f.project, u.identity.Subject); err != nil {
						t.Fatal(err)
					}
					if outcome == "approve" {
						for _, name := range []string{"get_sprite_pool", "set_sprites_token", "get_sprite_bootstrap_log"} {
							result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
							if err != nil || !result.Success || strings.Contains(result.Content, spritesSecretSentinel) || strings.Contains(result.Content, "private-provider-secret") {
								t.Fatalf("unsafe or unavailable Sprite read %s: %+v, %v", name, result, err)
							}
						}
					}
				}
				arguments := map[string]any{"work_item_id": id}
				switch tool {
				case "update_project_integration":
					arguments = map[string]any{"repository_enabled": true}
				case operatortool.MoveItem:
					arguments["state"] = "Done"
				case operatortool.EditItem:
					arguments["title"], arguments["body"], arguments["labels"], arguments["priority"] = "Edited by Luna", "", []string{"approved"}, 2
				case operatortool.AddComment:
					arguments["body"] = "Comment approved by the owner"
				case "set_sprite_pool":
					arguments = map[string]any{"min_runners": 0, "max_runners": 0}
				case "scale_up_sprite_pool":
					arguments = map[string]any{}
				}
				if outcome == "bad arguments" {
					arguments["approve"] = true
				}
				if outcome == "wrong role" {
					if _, err := db.ExecContext(t.Context(), "UPDATE hosted_members SET role='member' WHERE user_id=?", u.identity.Subject); err != nil {
						t.Fatal(err)
					}
				}
				if outcome == "no write grant" {
					f.grant(t, u, false, false)
				}
				if outcome == "foreign issue" {
					arguments["work_item_id"] = "wi_foreign"
				}
				if outcome == "unauthorized" {
					if _, err := db.ExecContext(t.Context(), "UPDATE hosted_members SET role='viewer' WHERE user_id=?", u.identity.Subject); err != nil {
						t.Fatal(err)
					}
				}
				if outcome == "expired session" {
					f.provider.mu.Lock()
					delete(f.provider.sessions, u.identity.Hosted.SessionID)
					f.provider.mu.Unlock()
				}
				raw, err := json.Marshal(arguments)
				if err != nil {
					t.Fatal(err)
				}
				result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				if outcome == "transport unavailable" {
					if result.Success || !strings.Contains(result.Content, "runner's repository policy") {
						t.Fatalf("unavailable integration result: %+v", result)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					return
				}
				if outcome == "unauthorized" || outcome == "foreign issue" || outcome == "expired session" || outcome == "bad arguments" || outcome == "no write grant" || outcome == "no runner grant" || outcome == "wrong role" && (tool == "update_project_integration" || coordinatorSpriteMutation(tool)) {
					if result.Success || !strings.Contains(result.Content, "error") {
						t.Fatalf("unauthorized result: %+v", result)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					return
				}
				if !result.Success {
					t.Fatalf("preview: %s", result.Content)
				}
				if coordinatorSpriteMutation(tool) && strings.Contains(result.Content, "private-provider-secret") {
					t.Fatal("preview disclosed the stored customer bootstrap")
				}
				var preview struct {
					ActionID string            `json:"action_id"`
					Status   chat.ActionStatus `json:"status"`
				}
				if err := json.Unmarshal([]byte(result.Content), &preview); err != nil || preview.Status != chat.ActionPending {
					t.Fatalf("preview=%s error=%v", result.Content, err)
				}
				ctx, err := tools.actionContext(t.Context(), record)
				if err != nil {
					t.Fatal(err)
				}
				connectionID := operatortool.CurrentConnection(ctx).ID
				action, ok := f.service.operatorChat.Action(connectionID, preview.ActionID)
				if !ok {
					t.Fatal("preview action missing")
				}
				assertCoordinatorEffect(t, f, id, tool, false)
				if outcome == "model approval" {
					if _, err := f.service.operatorChat.Confirm(ctx, connectionID, action.ID); err == nil {
						t.Fatal("model approved its own action")
					}
					if err := f.service.operatorChat.SetConnectionMode(chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx)), connectionID, chat.YOLOMode); err == nil {
						t.Fatal("chat bypassed approval with YOLO")
					}
					return
				}
				page := f.request(t, u, http.MethodGet, "/chat/approval?connection_id="+connectionID, nil)
				requireNativeStatus(t, page, http.StatusOK)
				tokens := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
				if len(tokens) == 0 {
					t.Fatalf("missing approval form: %s", page.Body.String())
				}
				decision := "confirm"
				if outcome == "reject" {
					decision = "reject"
				}
				if outcome == "revoked" {
					f.grant(t, u, false, false)
				}
				if outcome == "runner grant revoked" {
					f.grant(t, u, true, false)
				}
				if outcome == "stale" {
					if coordinatorSpriteMutation(tool) {
						_, err = db.ExecContext(t.Context(), "UPDATE project_sprite_pools SET revision=revision+1 WHERE project_id=?", f.project)
					} else if tool == "update_project_integration" {
						_, err = db.ExecContext(t.Context(), "UPDATE projects SET integration_revision=integration_revision+1 WHERE id=?", f.project)
					} else if tool != operatortool.AddComment {
						_, err = db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", id)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				form := url.Values{"connection_id": {connectionID}, "action_id": {action.ID}, "decision": {decision}, "form_token": {tokens[len(tokens)-1][1]}}
				response = f.request(t, u, http.MethodPost, "/chat/approval", form)
				want := http.StatusSeeOther
				if outcome == "revoked" {
					want = http.StatusForbidden
				}
				if outcome == "stale" && tool != operatortool.AddComment {
					want = http.StatusConflict
				}
				if outcome == "runner grant revoked" {
					want = http.StatusConflict
				}
				requireNativeStatus(t, response, want)
				f.service.spriteWakeWork.Wait()
				changed := outcome == "approve" || outcome == "wrong role" || outcome == "stale" && tool == operatortool.AddComment
				assertCoordinatorEffect(t, f, id, tool, changed)
				if tool == "scale_up_sprite_pool" && changed {
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "get_sprite_bootstrap_log", Arguments: json.RawMessage(`{}`)})
					if err != nil || !result.Success || !strings.Contains(result.Content, "billing enabled") || !strings.Contains(result.Content, "scale_up_sprite_pool") || strings.Contains(result.Content, spritesSecretSentinel) {
						t.Fatalf("unsafe or missing bootstrap failure/retry: %+v, %v", result, err)
					}
				}
				resolved, ok := f.service.operatorChat.Action(connectionID, action.ID)
				if !ok {
					t.Fatal("decision receipt lost")
				}
				if outcome == "reject" && resolved.Status != chat.ActionRejected || changed && resolved.Status != chat.ActionSucceeded {
					t.Fatalf("decision=%+v", resolved)
				}
				if outcome != "revoked" {
					messages, err = f.service.conversations.store.listMessages(t.Context(), db, record.ID, 0, 100)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, message := range messages {
						if strings.Contains(message.Text, string(resolved.Status)+".") {
							found = true
						}
					}
					if !found {
						t.Fatalf("decision was not posted in chat: %+v", messages)
					}
				}
			})
		}
	}
}

func assertCoordinatorEffect(t *testing.T, f hostedSecurityFixture, id tracker.NativeWorkItemID, tool string, changed bool) {
	t.Helper()
	var actual bool
	var err error
	switch tool {
	case "set_sprite_pool":
		err = f.service.database.db.QueryRowContext(t.Context(), "SELECT max_runners=0 FROM project_sprite_pools WHERE project_id=?", f.project).Scan(&actual)
	case "scale_up_sprite_pool":
		err = f.service.database.db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM project_sprite_members WHERE project_id=?)", f.project).Scan(&actual)
	case "update_project_integration":
		err = f.service.database.db.QueryRowContext(t.Context(), "SELECT github_repository_enabled FROM projects WHERE id=?", f.project).Scan(&actual)
	case operatortool.MoveItem:
		issue, _, readErr := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project}, string(id))
		err, actual = readErr, issue.State == "Done"
	case operatortool.EditItem:
		issue, _, readErr := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: "org_security", project: f.project}, string(id))
		err, actual = readErr, issue.Title == "Edited by Luna" && issue.Body == "" && issue.Priority != nil && *issue.Priority == 2 && len(issue.Labels) == 1 && issue.Labels[0] == "approved"
		if !changed && (issue.Title != "Original issue" || issue.Body != "Original body" || issue.Priority != nil || len(issue.Labels) != 1 || issue.Labels[0] != "original") {
			t.Fatalf("unapproved edit changed the issue: %+v", issue)
		}
	case operatortool.AddComment:
		err = f.service.database.db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM native_comments WHERE work_item_id=? AND body='Comment approved by the owner')", id).Scan(&actual)
	}
	if err != nil {
		t.Fatal(err)
	}
	if actual != changed {
		t.Fatalf("effect changed=%t, want %t", actual, changed)
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
