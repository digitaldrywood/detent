package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorProjectActions(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	for _, tool := range []string{"update_project_integration", operatortool.MoveItem, operatortool.EditItem, operatortool.AddComment, string(chat.ActionIssueSplit), string(chat.ActionArchiveItems), "set_sprite_pool", "scale_up_sprite_pool"} {
		outcomes := []string{"execute", "reject", "unauthorized", "revoked", "stale", "foreign issue", "expired session", "wrong role", "no write grant", "bad arguments"}
		if tool == "update_project_integration" {
			outcomes = append(outcomes, "transport unavailable")
		}
		if tool == string(chat.ActionIssueSplit) {
			outcomes = append(outcomes, "child failure", "edge failure", "comment failure", "invalid state", "cycle", "validation then valid")
		}
		if tool == string(chat.ActionArchiveItems) {
			outcomes = append(outcomes, "running", "merging", "became running", "became merging", "archive failure", "history failure", "already archived", "duplicate", "empty", "outside project", "number resolution")
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
					if _, err := f.service.database.approvePolicy(t.Context(), "org_security/"+string(f.project), "test", policy.Change{Policy: hubTestPolicy()}); err != nil {
						t.Fatal(err)
					}
				}
				if outcome == "number resolution" {
					seedArchiveIssues(t, f.service, nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}, 18, "Todo")
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
					if _, err := db.ExecContext(t.Context(), `INSERT INTO project_sprite_pools(organization_id,project_id,min_runners,max_runners,idle_seconds,bootstrap,configured_by) SELECT 'org_security',?,1,1,300,'private-provider-secret',principal_id FROM hosted_members WHERE user_id=?`, f.project, u.identity.Subject); err != nil {
						t.Fatal(err)
					}
					if outcome == "execute" {
						for _, name := range []string{"get_sprite_pool", "set_sprites_token", "get_sprite_bootstrap_log"} {
							result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
							if err != nil || !result.Success || strings.Contains(result.Content, spritesSecretSentinel) || strings.Contains(result.Content, "private-provider-secret") {
								t.Fatalf("unsafe or unavailable Sprite read %s: %+v, %v", name, result, err)
							}
						}
					}
				}
				archiveIDs := []string{string(id)}
				if tool == string(chat.ActionArchiveItems) {
					now := formatHubTime(f.service.config.now())
					if _, err := db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,created_at,updated_at) VALUES (?,'Merging','Merging',?,?)", f.project, now, now); err != nil {
						t.Fatal(err)
					}
					scope := nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
					children := 4
					if outcome == "number resolution" {
						if _, err := db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,created_at,updated_at) VALUES (?,'Blocked','Blocked',?,?)", f.project, now, now); err != nil {
							t.Fatal(err)
						}
						seedArchiveIssues(t, f.service, scope, 4, "Todo")
						children = 6
					}
					for _, item := range seedArchiveIssues(t, f.service, scope, children, "Todo") {
						archiveIDs = append(archiveIDs, string(item.WorkItemID))
					}
					if outcome == "number resolution" {
						resolvedIDs := make([]string, 0, len(archiveIDs))
						for index, workItemID := range archiveIDs {
							number, state := 23+index, "Blocked"
							if index == 0 {
								number, state = 19, "Done"
							}
							if _, err := db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) WHERE native_id=?", f.project, state, workItemID); err != nil {
								t.Fatal(err)
							}
							raw := json.RawMessage(fmt.Sprintf(`{"work_item_id":"#%d"}`, number))
							result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: coordinatorToolExplainIssue, Arguments: raw})
							if err != nil || !result.Success {
								t.Fatalf("resolve #%d: %+v, %v", number, result, err)
							}
							var explained coordinatorIssue
							decodeToolResult(t, result, &explained)
							if explained.WorkItemID != workItemID || explained.Number != int64(number) || explained.State != state {
								t.Fatalf("resolved #%d: %+v", number, explained)
							}
							resolvedIDs = append(resolvedIDs, explained.WorkItemID)
						}
						archiveIDs = resolvedIDs
					}
					if outcome == "running" {
						seedCoordinatorArchiveLease(t, f.service, archiveIDs[4])
					}
					if outcome == "merging" {
						if _, err := db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Merging') WHERE native_id=?", f.project, archiveIDs[4]); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "already archived" {
						if _, err := db.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE native_id=?", archiveIDs[4]); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "outside project" {
						response := f.request(t, u, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "archive-other-project", "name": "Other project", "grant_access": true})
						requireNativeStatus(t, response, http.StatusCreated)
						var created struct {
							ProjectID tracker.ProjectID `json:"project_id"`
						}
						decodeHubResponse(t, response, &created)
						scope.project = created.ProjectID
						foreign := seedArchiveIssues(t, f.service, scope, 1, "Todo")[0]
						archiveIDs[4] = string(foreign.WorkItemID)
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
				case string(chat.ActionArchiveItems):
					arguments = map[string]any{"work_item_ids": archiveIDs}
					if outcome == "duplicate" {
						arguments["work_item_ids"] = []string{string(id), string(id)}
					}
					if outcome == "empty" {
						arguments["work_item_ids"] = []string{}
					}
				case string(chat.ActionIssueSplit):
					priority := 1
					arguments = map[string]any{"parent_work_item_id": id, "children": []chat.IssueSplitChild{
						{Title: "Split storage", Description: "Create storage", Priority: &priority, State: "Todo"},
						{Title: "Split API", Description: "Use storage", Priority: &priority, State: "Todo"},
						{Title: "Split UI", Description: "Build UI", Priority: &priority, State: "Todo"},
					}, "edges": []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}}}
				case "set_sprite_pool":
					arguments = map[string]any{"min_runners": 0, "max_runners": 0}
				case "scale_up_sprite_pool":
					arguments = map[string]any{}
				}
				if tool == string(chat.ActionIssueSplit) {
					if outcome == "invalid state" {
						children := arguments["children"].([]chat.IssueSplitChild)
						children[0].State = "Missing"
					}
					if outcome == "cycle" {
						arguments["edges"] = []chat.IssueSplitEdge{{Dependent: 1, Blocker: 2}, {Dependent: 2, Blocker: 1}}
					}
				}
				if outcome == "bad arguments" {
					arguments["execute"] = true
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
					if tool == string(chat.ActionArchiveItems) {
						arguments["work_item_ids"] = []string{string(id), "wi_foreign"}
					} else {
						arguments["work_item_id"] = "wi_foreign"
					}
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
				if outcome == "validation then valid" {
					var invalid map[string]any
					if err := json.Unmarshal(raw, &invalid); err != nil {
						t.Fatal(err)
					}
					delete(invalid["children"].([]any)[1].(map[string]any), "state")
					arguments, err := json.Marshal(invalid)
					if err != nil {
						t.Fatal(err)
					}
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: arguments})
					if err != nil || result.Success || !strings.Contains(result.Content, "Each child requires a bounded title, description and target state") {
						t.Fatalf("validation feedback = %+v, error=%v", result, err)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
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
				if outcome == "running" || outcome == "merging" || outcome == "already archived" || outcome == "duplicate" || outcome == "empty" || outcome == "outside project" || outcome == "unauthorized" || outcome == "foreign issue" || outcome == "expired session" || outcome == "bad arguments" || outcome == "invalid state" || outcome == "cycle" || outcome == "no write grant" || outcome == "no runner grant" || outcome == "wrong role" && (tool == "update_project_integration" || coordinatorSpriteMutation(tool)) {
					if result.Success || !strings.Contains(result.Content, "error") {
						t.Fatalf("unauthorized result: %+v", result)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					return
				}
				if (tool == string(chat.ActionIssueSplit) || tool == string(chat.ActionArchiveItems)) && result.Success && outcome != "number resolution" {
					replay, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: raw})
					if err != nil || replay.Content != result.Content {
						t.Fatalf("proposal replay=%+v error=%v, want %s", replay, err, result.Content)
					}
				}
				if !result.Success {
					t.Fatalf("preview: %s", result.Content)
				}
				if coordinatorSpriteMutation(tool) && strings.Contains(result.Content, "private-provider-secret") {
					t.Fatal("preview disclosed the stored customer bootstrap")
				}
				var preview struct {
					ActionID string `json:"action_id"`
					Status   string `json:"status"`
				}
				if err := json.Unmarshal([]byte(result.Content), &preview); err != nil || preview.Status != "proposed" {
					t.Fatalf("preview=%s error=%v", result.Content, err)
				}
				ctx, err := tools.actionContext(t.Context(), record)
				if err != nil {
					t.Fatal(err)
				}
				connectionID := operatortool.CurrentConnection(ctx).ID
				messages, err = f.service.conversations.store.listMessages(t.Context(), db, record.ID, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				var action chat.Action
				for _, message := range messages {
					var data struct {
						Proposal struct {
							Action chat.Action `json:"action"`
						} `json:"operator_action"`
					}
					if json.Unmarshal(message.Data, &data) == nil && data.Proposal.Action.ID == preview.ActionID {
						action = data.Proposal.Action
					}
				}
				if action.RequestID == "" {
					t.Fatal("inline proposal missing")
				}
				if outcome == "number resolution" {
					var archive chat.IssueArchive
					if err := json.Unmarshal(action.Arguments, &archive); err != nil {
						t.Fatal(err)
					}
					if len(archive.Items) != 7 {
						t.Fatalf("archive has %d items, want seven", len(archive.Items))
					}
					for index, item := range archive.Items {
						if item.WorkItemID != archiveIDs[index] {
							t.Fatalf("archive item %d = %+v", index, item)
						}
					}
					var proposals, archived int
					for _, message := range messages {
						var data map[string]json.RawMessage
						if json.Unmarshal(message.Data, &data) == nil && data["operator_action"] != nil {
							proposals++
						}
					}
					if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=? AND archived=1", f.project).Scan(&archived); err != nil {
						t.Fatal(err)
					}
					if proposals != 1 || archived != 0 {
						t.Fatalf("proposals=%d archived=%d, want one unsubmitted proposal", proposals, archived)
					}
					return
				}
				for _, stored := range f.service.operatorChat.Conversation(connectionID).Actions {
					if stored.Status == chat.ActionPending {
						t.Fatal("pending action stored")
					}
				}
				assertCoordinatorEffect(t, f, id, tool, false)
				if outcome == "reject" {
					return
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
						staleID := string(id)
						if tool == string(chat.ActionArchiveItems) {
							staleID = archiveIDs[4]
						}
						_, err = db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", staleID)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if tool == string(chat.ActionIssueSplit) {
					trigger := ""
					switch outcome {
					case "child failure":
						trigger = "CREATE TRIGGER split_failure BEFORE INSERT ON issues WHEN NEW.title='Split API' BEGIN SELECT RAISE(ABORT, 'child failed'); END"
					case "edge failure":
						trigger = "CREATE TRIGGER split_failure BEFORE INSERT ON issue_dependencies BEGIN SELECT RAISE(ABORT, 'edge failed'); END"
					case "comment failure":
						trigger = "CREATE TRIGGER split_failure BEFORE INSERT ON native_comments BEGIN SELECT RAISE(ABORT, 'comment failed'); END"
					}
					if trigger != "" {
						if _, err := db.ExecContext(t.Context(), trigger); err != nil {
							t.Fatal(err)
						}
					}
				}
				if tool == string(chat.ActionArchiveItems) {
					if outcome == "became running" {
						seedCoordinatorArchiveLease(t, f.service, archiveIDs[4])
					}
					if outcome == "became merging" {
						if _, err := db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Merging') WHERE native_id=?", f.project, archiveIDs[4]); err != nil {
							t.Fatal(err)
						}
					}
					trigger := ""
					if outcome == "archive failure" {
						trigger = "CREATE TRIGGER archive_failure BEFORE UPDATE OF archived ON issues WHEN NEW.archived=1 AND NEW.title='Seed 3' BEGIN SELECT RAISE(ABORT, 'archive failed'); END"
					}
					if outcome == "history failure" {
						trigger = "CREATE TRIGGER archive_failure BEFORE INSERT ON collaboration_events WHEN NEW.type='issue.edited' AND NEW.work_item_id='" + archiveIDs[4] + "' BEGIN SELECT RAISE(ABORT, 'history failed'); END"
					}
					if trigger != "" {
						if _, err := db.ExecContext(t.Context(), trigger); err != nil {
							t.Fatal(err)
						}
					}
				}
				response = f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
				changed := outcome == "execute" || outcome == "validation then valid" || outcome == "wrong role" || outcome == "stale" && tool == operatortool.AddComment
				if changed {
					requireNativeStatus(t, response, http.StatusOK)
				} else if response.Code < 400 {
					t.Fatalf("refused call=%d %s", response.Code, response.Body.String())
				}
				f.service.spriteWakeWork.Wait()
				assertCoordinatorEffect(t, f, id, tool, changed)
				if tool == "scale_up_sprite_pool" && changed {
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "get_sprite_bootstrap_log", Arguments: json.RawMessage(`{}`)})
					if err != nil || !result.Success || !strings.Contains(result.Content, "billing enabled") || !strings.Contains(result.Content, "scale_up_sprite_pool") || strings.Contains(result.Content, spritesSecretSentinel) {
						t.Fatalf("unsafe or missing bootstrap failure/retry: %+v, %v", result, err)
					}
				}
				if changed && tool == string(chat.ActionIssueSplit) {
					requireNativeStatus(t, f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
					var count int
					if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE title LIKE 'Split %'").Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 3 {
						t.Fatalf("confirmation replay created %d children", count)
					}
				}
				if changed && tool == string(chat.ActionArchiveItems) {
					requireNativeStatus(t, f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
					assertCoordinatorArchiveEffect(t, f, true)
					for _, archived := range []string{"", "true"} {
						result, err := (operatorWorkReads{service: f.service, scope: nativeScope{organization: "org_security", project: f.project}}).ReadWork(ctx, operatortool.WorkList, operatortool.WorkReadRequest{ProjectID: string(f.project), Archived: archived, Limit: 100})
						if err != nil {
							t.Fatal(err)
						}
						var listing operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]
						if err := json.Unmarshal(result.Content, &listing); err != nil {
							t.Fatal(err)
						}
						want := 0
						if archived == "true" {
							want = 5
						}
						if len(listing.Data.Items) != want {
							t.Fatalf("work_list archived=%q: %s", archived, result.Content)
						}
					}
				}
				if changed {
					messages, err = f.service.conversations.store.listMessages(t.Context(), db, record.ID, 0, 100)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, message := range messages {
						if strings.Contains(message.Text, string(chat.ActionSucceeded)+".") {
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
	case string(chat.ActionArchiveItems):
		assertCoordinatorArchiveEffect(t, f, changed)
		return
	case string(chat.ActionIssueSplit):
		assertCoordinatorSplitEffect(t, f, id, changed)
		return
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
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project), credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	archiveIssues := seedArchiveIssues(t, f.service, scope, 5, "Todo")
	blockedArchive := seedArchiveIssues(t, f.service, scope, 1, "Merging")[0]
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title='Running cleanup issue' WHERE native_id=?", blockedArchive.WorkItemID); err != nil {
		t.Fatal(err)
	}
	seedCoordinatorArchiveLease(t, f.service, string(blockedArchive.WorkItemID))
	backend := f.service.conversations.config.Backend.(*fakeCoordinatorBackend)
	backend.setRun(func(ctx context.Context, turn int, handle runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		prompt := strings.ToLower(backend.request(t, turn-1).Prompt)
		if _, current, ok := strings.Cut(prompt, "</transcript>"); ok {
			prompt = current
		}
		call := runner.AgentToolCall{Name: "update_project_integration", Arguments: json.RawMessage(`{"repository_enabled":true}`)}
		if strings.Contains(prompt, "split this issue") || strings.Contains(prompt, "cyclic split") {
			result, err := f.proposeBrowserIssueSplit(ctx, handle, strings.Contains(prompt, "cyclic split"), strings.Contains(prompt, "six children"))
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			text := "Review the exact change below and confirm it to continue."
			if !result.Success {
				text = "The change was refused: " + result.Content
			}
			return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: text})
		}
		if strings.Contains(prompt, "archive the test issues") || strings.Contains(prompt, "archive the running issue") {
			ids := make([]string, 0, len(archiveIssues))
			for _, item := range archiveIssues {
				ids = append(ids, string(item.WorkItemID))
			}
			if strings.Contains(prompt, "archive the running issue") {
				ids = append(ids, string(blockedArchive.WorkItemID))
			}
			raw, err := json.Marshal(map[string]any{"work_item_ids": ids})
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			call = runner.AgentToolCall{Name: string(chat.ActionArchiveItems), Arguments: raw}
		}
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

func (f *browserHostedFixture) proposeBrowserIssueSplit(ctx context.Context, handle runner.AgentToolHandler, cyclic, sixChildren bool) (runner.AgentToolResult, error) {
	children := []chat.IssueSplitChild{{Title: "Split storage", Description: "Create the **storage layer**.\n\n- Preserve existing records.\n- Add the migration.\n\n```detent-agent\nschema: 1\neffort: high\n```", State: "Todo"}, {Title: "Split API", Description: "Use the storage layer.", State: "Todo"}, {Title: "Split UI", Description: "Render the approved UI.", State: "Todo"}}
	edges := []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}, {Dependent: 0, Blocker: 1}, {Dependent: 0, Blocker: 2}, {Dependent: 0, Blocker: 3}}
	if sixChildren {
		children = append(children, chat.IssueSplitChild{Title: "Split runner", Description: "Run the approved work.", State: "Todo"}, chat.IssueSplitChild{Title: "Split reporting", Description: "Report the saved results.", State: "Todo"}, chat.IssueSplitChild{Title: "Split integration", Description: "Integrate the completed children.", State: "Todo"})
		edges = append(edges, chat.IssueSplitEdge{Dependent: 3, Blocker: 1}, chat.IssueSplitEdge{Dependent: 3, Blocker: 2}, chat.IssueSplitEdge{Dependent: 5, Blocker: 4}, chat.IssueSplitEdge{Dependent: 6, Blocker: 3}, chat.IssueSplitEdge{Dependent: 6, Blocker: 5}, chat.IssueSplitEdge{Dependent: 0, Blocker: 4}, chat.IssueSplitEdge{Dependent: 0, Blocker: 5}, chat.IssueSplitEdge{Dependent: 0, Blocker: 6})
	}
	if cyclic {
		edges = append(edges, chat.IssueSplitEdge{Dependent: 1, Blocker: 2})
	}
	raw, err := json.Marshal(chat.IssueSplit{ParentID: f.workItem, Children: children, Edges: edges})
	if err != nil {
		return runner.AgentToolResult{}, err
	}
	loaded, err := handle(ctx, runner.AgentToolCall{Name: "load_split_issue_skill", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		return runner.AgentToolResult{}, err
	}
	if !loaded.Success {
		return runner.AgentToolResult{}, fmt.Errorf("load split skill: %s", loaded.Content)
	}
	return handle(ctx, runner.AgentToolCall{Name: string(chat.ActionIssueSplit), Arguments: raw})
}

func assertCoordinatorSplitEffect(t *testing.T, f hostedSecurityFixture, parent tracker.NativeWorkItemID, changed bool) {
	t.Helper()
	db := f.service.database.db
	var children, edges, comments int
	for query, target := range map[string]*int{
		"SELECT count(*) FROM issues WHERE title LIKE 'Split %'":    &children,
		"SELECT count(*) FROM issue_dependencies":                   &edges,
		"SELECT count(*) FROM native_comments WHERE work_item_id=?": &comments,
	} {
		var args []any
		if strings.Contains(query, "?") {
			args = []any{parent}
		}
		if err := db.QueryRowContext(t.Context(), query, args...).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if !changed {
		if children != 0 || edges != 0 || comments != 0 {
			t.Fatalf("partial split survived: children=%d edges=%d comments=%d", children, edges, comments)
		}
		var residue int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type='issue.created' AND work_item_id<>?", parent).Scan(&residue); err != nil {
			t.Fatal(err)
		}
		if residue != 0 {
			t.Fatalf("rolled back split left %d history events", residue)
		}
		return
	}
	if children != 3 || edges != 1 || comments != 1 {
		t.Fatalf("split: children=%d edges=%d comments=%d", children, edges, comments)
	}
	var correctlyFiled int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues i JOIN workflow_states w ON w.id=i.workflow_state_id JOIN queue_entries q ON q.issue_id=i.id WHERE i.title LIKE 'Split %' AND w.detent_state='Todo' AND q.priority_override=1").Scan(&correctlyFiled); err != nil {
		t.Fatal(err)
	}
	if correctlyFiled != 3 {
		t.Fatalf("only %d children have the approved state and priority", correctlyFiled)
	}
	scope := nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	var blocker, dependent tracker.WorkItemID
	var blockerNative string
	if err := db.QueryRowContext(t.Context(), "SELECT id,native_id FROM issues WHERE title='Split storage'").Scan(&blocker, &blockerNative); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE title='Split API'").Scan(&dependent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE projects SET require_dependencies=1 WHERE id=?", f.project); err != nil {
		t.Fatal(err)
	}
	for _, terminal := range []bool{false, true} {
		if terminal {
			if _, err := db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Done') WHERE id=?", f.project, blocker); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{NativeScope: &scope, Scope: string(f.project)}, nil, nil, nil, nil, nil, nil, nil, nil)
		_ = tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(ids, dependent) != terminal {
			t.Fatalf("dependent dispatch=%v while blocker terminal=%v", slices.Contains(ids, dependent), terminal)
		}
	}
}

func seedCoordinatorArchiveLease(t *testing.T, service *Service, id string) {
	t.Helper()
	db := service.database.db
	now := formatHubTime(service.config.now())
	if _, err := db.ExecContext(t.Context(), "INSERT INTO machines (id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id) VALUES ('archive-machine','archive-host',1,'test',?,?,?,?)", now, now, now, service.config.Hosted.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO leases (lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) SELECT 'archive-lease',id,'archive-machine','archive-session',?,?,?,?,? FROM issues WHERE native_id=?", formatHubTime(service.config.now().Add(time.Hour)), now, now, now, now, id); err != nil {
		t.Fatal(err)
	}
}

func assertCoordinatorArchiveEffect(t *testing.T, f hostedSecurityFixture, changed bool) {
	t.Helper()
	db := f.service.database.db
	var archived, history int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=? AND archived=1 AND revision>1 AND (title='Original issue' OR title LIKE 'Seed %')", f.project).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE project_id=? AND json_extract(data_json,'$.operation')='archive'", f.project).Scan(&history); err != nil {
		t.Fatal(err)
	}
	want := 0
	if changed {
		want = 5
	}
	if history != want || archived != want {
		t.Fatalf("archive effect: archived=%d history=%d want=%d", archived, history, want)
	}
	if changed {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{NativeScope: &nativeScope{organization: "org_security", project: f.project}, Scope: string(f.project)}, nil, nil, nil, nil, nil, nil, nil, nil)
		_ = tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Fatalf("archived issues remain dispatchable: %v", ids)
		}
	}
}
