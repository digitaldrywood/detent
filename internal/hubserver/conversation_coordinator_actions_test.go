package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorProjectActions(t *testing.T) {
	for _, tool := range []string{"propose_stop_run", "propose_file_issue", "propose_maintenance_issue", "propose_move_item", "propose_set_priority", "propose_backlog_admission", "update_project_integration", operatortool.MoveItem, operatortool.EditItem, operatortool.AddComment, string(chat.ActionIssueSplit), string(chat.ActionArchiveItems), "set_sprite_pool", "scale_up_sprite_pool"} {
		outcomes := []string{"execute", "reject", "unauthorized", "revoked", "stale", "foreign issue", "expired session", "wrong role", "no write grant", "bad arguments"}
		if tool == operatortool.MoveItem {
			outcomes = append(outcomes, "human answer", "human empty answer", "human comment failure", "human transition failure", "human park recorded on submit", "dependency park")
		}
		if tool == "propose_stop_run" {
			outcomes = append(outcomes, "replaced attempt", "expired attempt", "release failure", "routing failure", "no active attempt")
			outcomes = append(outcomes, "stored key preview", "shared entry preview", "shared entry expired", "shared entry rotated", "shared entry absent channel", "shared entry mismatched principal", "shared entry revoked", "shared entry changed issuer", "shared entry changed generation", "shared entry no write grant", "shared entry foreign issue")
		}
		if tool == "propose_file_issue" || tool == "propose_maintenance_issue" {
			outcomes = []string{"execute", "reject", "unauthorized", "revoked", "no write grant", "bad arguments"}
		}
		if tool == "update_project_integration" {
			outcomes = append(outcomes, "transport unavailable", "execute with manual import")
		}
		if tool == string(chat.ActionIssueSplit) {
			outcomes = append(outcomes, "child failure", "edge failure", "comment failure", "invalid state", "cycle", "validation then valid")
		}
		if tool == string(chat.ActionArchiveItems) {
			outcomes = append(outcomes, "running", "merging", "became running", "became merging", "archive failure", "history failure", "already archived", "duplicate", "empty", "outside project", "number resolution", "concurrent replay", "completion without entitlement")
		}
		if coordinatorSpriteMutation(tool) {
			outcomes = append(outcomes, "no runner grant", "runner grant revoked", "grantless project")
		}
		if tool == "set_sprite_pool" {
			outcomes = append(outcomes, "intake preview", "sandbox access", "full access", "preserve access")
		}
		for _, outcome := range outcomes {
			t.Run(tool+"/"+outcome, func(t *testing.T) {
				sharedEntry := strings.HasPrefix(outcome, "shared entry")
				mcpPost := sharedEntry || outcome == "stored key preview"
				var clock atomic.Int64
				clock.Store(time.Now().UnixNano())
				configure := func(cfg *Config) {
					cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir()}
					if sharedEntry || outcome == "expired attempt" {
						cfg.now = func() time.Time { return time.Unix(0, clock.Load()) }
					}
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
						cfg.ReconcileBackend = &scriptedReconcileBackend{}
					}
				}
				var f coordinatorActionFixture
				var shared operatorSharedFixture
				if sharedEntry {
					shared = newOperatorSharedFixture(t, configure)
					f = coordinatorActionFixture{shared.operatorSecurityFixture}
				} else {
					f = coordinatorActionFixture{newOperatorSecurityFixture(t, configure)}
				}
				if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
					t.Fatal(err)
				}
				f.service.tokenUseWork.Wait()
				role := "owner"
				if outcome == "wrong role" {
					role = "member"
				}
				u := f.user(t, "luna-owner", role, "luna@example.test", "write", "")
				var currentEntryClaims cloudassert.Claims
				var keyExpired bool
				if sharedEntry {
					f.service.config.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
						if keyExpired || currentEntryClaims.Key == nil || origin.KeyID != currentEntryClaims.Key.ID || origin.CredentialBinding != currentEntryClaims.Key.CredentialBinding {
							return cloudassert.Claims{}, cloudassert.ErrInvalid
						}
						fresh := currentEntryClaims
						fresh.IssuedAt = f.service.config.now()
						fresh.ExpiresAt = fresh.IssuedAt.Add(cloudassert.MaxLifetime)
						return fresh, nil
					}
				}
				var storedToken string
				coordinatorActionProject(t, f, func(p *projectstore.Record) { p.Settings.DuplicateChallenge = false })
				request := f.request
				if sharedEntry {
					request = func(t *testing.T, user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						return shared.serve(t, hostedSharedRequest{user: &user, method: method, target: path, body: string(raw), csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
					}
				}
				if outcome == "stored key preview" {
					credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: u.identity.Hosted, Email: u.identity.Email}, apikey.HashToken(u.token))
					if err != nil {
						t.Fatal(err)
					}
					expires := time.Now().Add(time.Hour)
					key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "coordinator", Scope: apiScopeOperator, Issuer: &credential, KeyScope: apikey.ScopeWrite, ExpiresAt: &expires, ProjectIDs: []string{string(f.project)}, ProjectAccess: hostedProjectsSelected})
					if err != nil {
						t.Fatal(err)
					}
					storedToken = key.Token
					request = func(t *testing.T, _ hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
						r.Header.Set("Content-Type", "application/json")
						r.Header.Set("Authorization", "Bearer "+storedToken)
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, r)
						return response
					}
				}
				if coordinatorSpriteMutation(tool) {
					f.grant(t, u, true, outcome != "no runner grant")
					if outcome == "grantless project" {
						f.writeTenant(t, func(w tenantstore.Writer) error {
							_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: "prj_grantless", Name: "Example project", Slug: "grantless", Profile: "native"}, States: nativeFixtureStates()})
							return err
						})
					}
					if _, err := f.service.approveTestTenantPolicy(t.Context(), "org_security/"+string(f.project), "test", policy.Change{Policy: hubTestPolicy()}); err != nil {
						t.Fatal(err)
					}
				}
				if outcome == "number resolution" {
					seedCoordinatorActionIssues(t, f.service, nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}, 18, "Todo")
				}
				response := createIndependentNativeIssue(t, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "luna-issue"}, Title: "Original issue", Body: nativeContractTestBody, State: "Todo", Labels: []string{"original"}}, func(draft tracker.CreateIssue) *httptest.ResponseRecorder {
					return request(t, u, http.MethodPost, f.base+"/work-items", draft)
				})
				requireNativeStatus(t, response, http.StatusOK)
				if coordinatorSpriteMutation(tool) {
					f.service.spriteWakeWork.Wait()
				}
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				id := issue.WorkItemID
				db := f.admin
				humanPark := tool == operatortool.MoveItem && strings.HasPrefix(outcome, "human ")
				parkedMove := humanPark || tool == operatortool.MoveItem && outcome == "dependency park"
				if parkedMove {
					coordinatorActionState(t, f, "Blocked", []string{"Done"})
					coordinatorActionItem(t, f, string(id), func(row *corestore.WorkItem) { row.WorkflowStateName = new("Blocked") })
					coordinatorActionPark(t, f, string(id), humanPark && outcome != "human park recorded on submit")
				}
				if tool == "propose_stop_run" {
					coordinatorActionState(t, f, "Blocked", []string{"Todo"})
					if outcome == "expired attempt" {
						coordinatorActionLease(t, f.service, string(id), time.Minute)
					} else if outcome != "no active attempt" {
						seedCoordinatorArchiveLease(t, f.service, string(id))
					}
				}
				if tool == "propose_backlog_admission" {
					coordinatorActionState(t, f, "Backlog", []string{"Todo"})
					coordinatorActionItem(t, f, string(id), func(row *corestore.WorkItem) { row.WorkflowStateName = new("Backlog") })
				}
				seedCoordinatorRepository(t, f.service, f.project)
				if outcome == "execute with manual import" {
					coordinatorActionProject(t, f, func(p *projectstore.Record) { p.Settings.Intake = "manual" })
				}
				text := "Please change this project"
				if outcome == "intake preview" {
					text = "no extra steps, 2 and 2"
				}
				create := map[string]any{"key": "luna-conversation"}
				if !mcpPost {
					create["first_message"] = map[string]any{"key": "luna-message", "text": text}
				}
				response = request(t, u, http.MethodPost, f.base+"/conversations", create)
				requireNativeStatus(t, response, http.StatusCreated)
				var created struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
				}
				decodeHubResponse(t, response, &created)
				if mcpPost {
					call := func(name string, args map[string]any) operatortool.Result {
						body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}}
						if sharedEntry {
							raw, err := json.Marshal(body)
							if err != nil {
								t.Fatal(err)
							}
							response = shared.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/api/v2/organizations/org_security/mcp", body: string(raw), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": name}, mutate: func(claims *cloudassert.Claims) {
								claims.Subject, claims.ProviderOrganization, claims.Role = u.identity.Subject, f.service.config.Hosted.WorkOSOrganizationID, "owner"
								claims.Key = &apikey.KeyAuthority{ID: "key_coordinator", CredentialBinding: strings.Repeat("a", 64), Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{string(f.project)}}}
								currentEntryClaims = *claims
							}})
						} else {
							response = performHubWorkCall(t, f.service, "/api/v2/organizations/org_security/mcp", storedToken, name, body)
						}
						requireNativeStatus(t, response, http.StatusOK)
						var reply hostedContextReply
						decodeHubResponse(t, response, &reply)
						return operatortool.Result{Content: hostedContextData(t, reply, false)}
					}
					for _, name := range []string{"work_item", "work_runs"} {
						result := call(name, map[string]any{"project_id": f.project, "reference": id})
						if !strings.Contains(string(result.Content), string(id)) {
							t.Fatalf("direct MCP %s=%s", name, result.Content)
						}
					}
					args := map[string]any{"project_id": f.project, "conversation_id": created.Conversation.ID, "request_id": "mcp-message", "input": map[string]any{"kind": "message", "text": text}}
					result := call("post_conversation_command", args)
					if workspaceOperatorAction(t, result).Status != chat.ActionSucceeded {
						t.Fatalf("MCP post=%s", result.Content)
					}
				}
				c := f.service.conversations.coordinator.(*conversationTurnCoordinator)
				record, err := c.readConversation(t.Context(), nil, created.Conversation.ID)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100, Oldest: true})
				if err != nil {
					t.Fatal(err)
				}
				var user conversationMessageRecord
				for _, message := range messages {
					if message.Role == conversation.RoleUser {
						user = message
					}
				}
				if mcpPost {
					if sharedEntry {
						clock.Add(int64(time.Minute))
					}
					f.service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{f.service}, chat.WithClock(f.service.config.now), chat.WithSessionStore(operatorChatStore{f.service.database}, f.service.resolveOperatorChatAuthority))
				}
				state := &coordinatorTurnState{coordinator: c, conversationID: record.ID, users: []conversationMessageRecord{user}}
				tools := newCoordinatorToolset(c, state)
				if mcpPost {
					if record.OwnerSubject != u.identity.Subject || record.Visibility != conversation.VisibilityPrivate || record.WorkItemID != "" {
						t.Fatalf("MCP coordinator=%+v", record)
					}
					ctx, err := tools.actionContext(t.Context(), record)
					if err != nil {
						t.Fatalf("MCP origin binding: %v", err)
					}
					if sharedEntry {
						var persisted accessstore.OperatorSession
						f.readTenant(t, func(r tenantstore.Reader) error {
							var err error
							persisted, err = r.OperatorSession(t.Context(), operatortool.CurrentConnection(ctx).ID, f.service.config.now().Add(-time.Hour))
							return err
						})
						var origin map[string]any
						if err := json.Unmarshal(coordinatorActionJSON(t, persisted.Origin), &origin); err != nil {
							t.Fatal(err)
						}
						if origin["credential_binding"] != strings.Repeat("a", 64) || origin["key_id"] != "key_coordinator" || origin["exp"] != nil || origin["iat"] != nil || origin["token"] != nil || origin["token_hash"] != nil {
							t.Fatalf("persisted request credentials or missing stable origin: %v", origin)
						}
					}
					identity := operatortool.ConnectionIdentity(ctx)
					var principal string
					f.readTenant(t, func(r tenantstore.Reader) error {
						member, err := r.AccessMember(t.Context(), u.identity.Subject)
						principal = member.PrincipalID
						return err
					})
					if identity.SessionID != "" || identity.CredentialID == "" || identity.PrincipalID != principal {
						t.Fatalf("MCP origin identity=%+v", identity)
					}
					for _, section := range []string{"work_item", "work_runs"} {
						raw := json.RawMessage(fmt.Sprintf(`{"work_item_id":%q,"section":%q}`, id, section))
						result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "read_issue_history", Arguments: raw})
						if err != nil || !result.Success || !strings.Contains(result.Content, string(id)) {
							t.Fatalf("MCP coordinator %s=%+v error=%v", section, result, err)
						}
					}
					switch outcome {
					case "shared entry expired":
						keyExpired = true
					case "shared entry rotated":
						currentEntryClaims.Key.CredentialBinding = strings.Repeat("b", 64)
					case "shared entry absent channel":
						f.service.config.Hosted.SharedEntry.ResolveKey = nil
					case "shared entry mismatched principal":
						user.Data = json.RawMessage(fmt.Sprintf(`{"operator_connection":%q,"operator_principal":"foreign"}`, operatortool.CurrentConnection(ctx).ID))
						state.users = []conversationMessageRecord{user}
					case "shared entry revoked":
						coordinatorActionMember(t, f, u.identity.Subject, "owner", false)
					case "shared entry changed issuer":
						f.service.config.Hosted.SharedEntry.Issuer = "other-entry"
					case "shared entry changed generation":
						f.service.config.Hosted.SharedEntry.Generation++
					case "shared entry no write grant":
						f.grant(t, u, false, false)
					}
					if sharedEntry {
						f.service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{f.service}, chat.WithClock(f.service.config.now), chat.WithSessionStore(operatorChatStore{f.service.database}, f.service.resolveOperatorChatAuthority))
					}
					if outcome == "shared entry rotated" || outcome == "shared entry absent channel" || outcome == "shared entry expired" || outcome == "shared entry mismatched principal" || outcome == "shared entry revoked" || outcome == "shared entry changed issuer" || outcome == "shared entry changed generation" || outcome == "shared entry no write grant" {
						for _, section := range []string{"work_item", "work_runs"} {
							raw := json.RawMessage(fmt.Sprintf(`{"work_item_id":%q,"section":%q}`, id, section))
							result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "read_issue_history", Arguments: raw})
							if err != nil || result.Success {
								t.Fatalf("changed MCP authority %s=%+v error=%v", section, result, err)
							}
						}
					}
				}
				if coordinatorSpriteMutation(tool) {
					f.service.spriteWakeWork.Wait()
					envelope, err := f.service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), secretAAD("org_security", "", flySpritesToken))
					if err != nil {
						t.Fatal(err)
					}
					f.writeTenant(t, func(w tenantstore.Writer) error {
						return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_security", Kind: flySpritesToken, OrganizationSlug: "example-org", Envelope: envelope, UpdatedAt: f.service.config.now()}, accessstore.SecretAudit{OrganizationID: "org_security", Actor: u.identity.Subject, Kind: flySpritesToken, Event: "create", KeyVersion: envelope.Version, RecordedAt: f.service.config.now()})
					})
					setup := f.request(t, u, http.MethodPut, "/api/v2/organizations/org_security/sprite-pool", map[string]any{"idempotency_key": "actions-pool", "min_runners": 1, "max_runners": 1, "idle_seconds": 300, "bootstrap": "private-provider-secret"})
					requireNativeStatus(t, setup, http.StatusOK)
					if outcome == "execute" {
						for _, name := range []string{"get_sprite_pool", "set_sprites_token", "get_sprite_bootstrap_log"} {
							result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
							if err != nil || !result.Success || strings.Contains(result.Content, spritesSecretSentinel) || strings.Contains(result.Content, "private-provider-secret") {
								t.Fatalf("unsafe or unavailable Sprite read %s: %+v, %v", name, result, err)
							}
						}
					}
					if outcome == "intake preview" {
						result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "get_sprite_pool", Arguments: json.RawMessage(`{}`)})
						if err != nil || !result.Success {
							t.Fatalf("read intake state: %+v, %v", result, err)
						}
						var status struct {
							Token struct {
								Present bool `json:"present"`
							} `json:"token"`
							MinRunners          int   `json:"min_runners"`
							MaxRunners          int   `json:"max_runners"`
							BootstrapConfigured bool  `json:"bootstrap_configured"`
							ConnectedRunners    *int  `json:"connected_runners"`
							Members             []any `json:"members"`
						}
						decodeToolResult(t, result, &status)
						if !status.Token.Present || status.MinRunners != 1 || status.MaxRunners != 1 || !status.BootstrapConfigured || status.ConnectedRunners == nil || status.Members == nil {
							t.Fatalf("intake read must supply setup facts: %s", result.Content)
						}
					}
				}
				archiveIDs := []string{string(id)}
				if tool == string(chat.ActionArchiveItems) {
					coordinatorActionState(t, f, "Merging", []string{"Done"})
					scope := nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
					children := 4
					if outcome == "number resolution" {
						coordinatorActionState(t, f, "Blocked", []string{"Todo"})
						seedCoordinatorActionIssues(t, f.service, scope, 4, "Todo")
						children = 6
					}
					for _, item := range seedCoordinatorActionIssues(t, f.service, scope, children, "Todo", outcome == "already archived") {
						archiveIDs = append(archiveIDs, string(item.WorkItemID))
					}
					if outcome == "number resolution" {
						resolvedIDs := make([]string, 0, len(archiveIDs))
						for index, workItemID := range archiveIDs {
							number, state := 23+index, "Blocked"
							if index == 0 {
								number, state = 19, "Done"
							}
							coordinatorActionItem(t, f, workItemID, func(row *corestore.WorkItem) { row.WorkflowStateName = &state })
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
						coordinatorActionItem(t, f, archiveIDs[4], func(row *corestore.WorkItem) { row.WorkflowStateName = new("Merging") })
					}
					if outcome == "outside project" {
						response := f.request(t, u, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "archive-other-project", "name": "Other project", "grant_access": true})
						requireNativeStatus(t, response, http.StatusCreated)
						var created struct {
							ProjectID tracker.ProjectID `json:"project_id"`
						}
						decodeHubResponse(t, response, &created)
						scope.project = created.ProjectID
						foreign := seedCoordinatorActionIssues(t, f.service, scope, 1, "Todo")[0]
						archiveIDs[4] = string(foreign.WorkItemID)
					}
				}
				arguments := map[string]any{"work_item_id": id}
				switch tool {
				case "propose_stop_run":
					arguments["destination"], arguments["priority"] = "Blocked", 2
				case "update_project_integration":
					arguments = map[string]any{"repository_enabled": true}
				case operatortool.MoveItem, "propose_move_item":
					arguments["state"] = "Done"
					if humanPark && outcome != "human empty answer" && outcome != "human park recorded on submit" {
						arguments["reason_detail"] = "Use the recommended compatibility guarantee for default and existing configurations."
					} else if outcome == "human empty answer" {
						arguments["reason_detail"] = " \n "
					}
				case "propose_set_priority":
					arguments["priority"] = 2
				case "propose_file_issue", "propose_maintenance_issue":
					arguments = map[string]any{"title": "Slash issue", "description": "A proposed change" + issueContractTestSections, "state": "Todo"}
				case operatortool.EditItem:
					arguments["title"], arguments["body"], arguments["labels"], arguments["priority"] = "Edited by Luna", nativeContractTestBody, []string{"approved"}, 2
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
						{Title: "Split storage", Description: "Create storage" + issueContractTestSections, Priority: &priority, State: "Todo"},
						{Title: "Split API", Description: "Use storage" + issueContractTestSections, Priority: &priority, State: "Todo"},
						{Title: "Split UI", Description: "Build UI" + issueContractTestSections, Priority: &priority, State: "Todo"},
					}, "edges": []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}}}
				case "set_sprite_pool":
					arguments = map[string]any{"min_runners": 0, "max_runners": 0}
					if outcome == "sandbox access" {
						arguments["isolation_tier"] = "sandbox"
					}
					if outcome == "full access" {
						arguments["isolation_tier"] = "native-trusted"
					}
					if outcome == "preserve access" {
						if _, err := db.ExecContext(t.Context(), "UPDATE organization_sprite_pools SET isolation_tier='sandbox' WHERE organization_id='org_security'"); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "intake preview" {
						arguments = map[string]any{"min_runners": 2, "max_runners": 2}
					}
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
				if outcome == "no write grant" {
					f.grant(t, u, false, false)
				}
				if outcome == "foreign issue" || outcome == "shared entry foreign issue" {
					if tool == string(chat.ActionArchiveItems) {
						arguments["work_item_ids"] = []string{string(id), "wi_foreign"}
					} else {
						arguments["work_item_id"] = "wi_foreign"
					}
				}
				if outcome == "unauthorized" {
					coordinatorActionMember(t, f, u.identity.Subject, "viewer", true)
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
				if outcome == "sandbox access" {
					if result.Success || !strings.Contains(result.Content, "Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support.") {
						t.Fatalf("sandbox preview refusal = %+v", result)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					return
				}
				if sharedEntry && outcome != "shared entry preview" {
					if result.Success || !strings.Contains(result.Content, "error") {
						t.Fatalf("MCP authority denial=%+v", result)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					return
				}
				if outcome == "transport unavailable" {
					if result.Success || !strings.Contains(result.Content, "runner's repository policy") {
						t.Fatalf("unavailable integration result: %+v", result)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					return
				}
				if outcome == "human empty answer" {
					if result.Success || !strings.Contains(result.Content, "human's answer") {
						t.Fatalf("missing answer was not refused: %+v", result)
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					return
				}
				if outcome == "running" || outcome == "merging" || outcome == "already archived" || outcome == "duplicate" || outcome == "empty" || outcome == "outside project" || outcome == "unauthorized" || outcome == "foreign issue" || outcome == "expired session" || outcome == "bad arguments" || outcome == "invalid state" || outcome == "cycle" || outcome == "no write grant" || outcome == "no active attempt" || outcome == "wrong role" && (tool == "update_project_integration" || coordinatorSpriteMutation(tool)) {
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
				messages, err = c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100, Oldest: true})
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
				if strings.HasPrefix(tool, "propose_") && !action.RequiresConfirmation {
					t.Fatal("slash proposal must require explicit confirmation")
				}
				if action.RequestID == "" {
					t.Fatal("inline proposal missing")
				}
				if mcpPost {
					assertCoordinatorEffect(t, f, id, tool, false)
					if action.Mutation.Confirmation != "" || len(f.service.operatorChat.Conversation(connectionID).Actions) != 0 {
						t.Fatalf("MCP preview executed or retained an executable action: %+v", action)
					}
					return
				}
				if outcome == "intake preview" {
					var change coordinatorSpriteChange
					if err := json.Unmarshal(action.Arguments, &change); err != nil {
						t.Fatal(err)
					}
					if change.Input.MinRunners == nil || *change.Input.MinRunners != 2 || change.Input.MaxRunners == nil || *change.Input.MaxRunners != 2 || change.Input.Bootstrap != nil {
						t.Fatalf("combined answer preview must preserve bootstrap: %s", action.Arguments)
					}
					var proposals int
					for _, message := range messages {
						var data map[string]json.RawMessage
						if json.Unmarshal(message.Data, &data) == nil && data["operator_action"] != nil {
							proposals++
						}
					}
					assertCoordinatorEffect(t, f, id, tool, false)
					if proposals != 1 {
						t.Fatalf("intake produced %d previews, want one", proposals)
					}
					response = f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
					requireNativeStatus(t, response, http.StatusOK)
					var floor, ceiling int
					var bootstrap string
					if err := db.QueryRowContext(t.Context(), "SELECT min_runners,max_runners,bootstrap FROM organization_sprite_pools WHERE organization_id='org_security'").Scan(&floor, &ceiling, &bootstrap); err != nil {
						t.Fatal(err)
					}
					if floor != 2 || ceiling != 2 || bootstrap != "private-provider-secret" {
						t.Fatalf("approved intake saved floor=%d ceiling=%d bootstrap=%q", floor, ceiling, bootstrap)
					}
					return
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
					if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2 AND archived", "org_security", f.project).Scan(&archived); err != nil {
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
						_, err = db.ExecContext(t.Context(), "UPDATE organization_sprite_pools SET revision=revision+1 WHERE organization_id='org_security'")
					} else if tool == "update_project_integration" {
						coordinatorActionProject(t, f, func(*projectstore.Record) {})
					} else if tool != operatortool.AddComment {
						staleID := string(id)
						if tool == string(chat.ActionArchiveItems) {
							staleID = archiveIDs[4]
						}
						coordinatorActionItem(t, f, staleID, func(*corestore.WorkItem) {})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if tool == string(chat.ActionIssueSplit) {
					switch outcome {
					case "child failure":
						coordinatorActionFault(t, f, "issues", "INSERT", " AND NEW.title='Split API'", "child failed")
					case "edge failure":
						coordinatorActionFault(t, f, "issue_dependencies", "INSERT", "", "edge failed")
					case "comment failure":
						coordinatorActionFault(t, f, "native_comments", "INSERT", "", "comment failed")
					}
				}
				if tool == string(chat.ActionArchiveItems) {
					if outcome == "became running" {
						seedCoordinatorArchiveLease(t, f.service, archiveIDs[4])
					}
					if outcome == "became merging" {
						coordinatorActionItem(t, f, archiveIDs[4], func(row *corestore.WorkItem) { row.WorkflowStateName = new("Merging") })
					}
					if outcome == "archive failure" {
						coordinatorActionFault(t, f, "issues", "UPDATE OF archived", " AND NEW.archived AND NEW.title='Seed 3'", "archive failed")
					}
					if outcome == "history failure" {
						coordinatorActionFault(t, f, "collaboration_events", "INSERT", " AND NEW.type='issue.edited' AND NEW.work_item_id="+pgtransport.QuoteLiteral(archiveIDs[4]), "history failed")
					}
				}
				if tool == "propose_stop_run" {
					switch outcome {
					case "replaced attempt":
						coordinatorActionReplaceLease(t, f)
					case "expired attempt":
						f.readTenant(t, func(r tenantstore.Reader) error {
							lease, err := r.Lease(t.Context(), "archive-lease")
							clock.Store(lease.ExpiresAt.Add(time.Second).UnixNano())
							return err
						})
					case "release failure":
						coordinatorActionFault(t, f, "leases", "UPDATE OF released_at", "", "release failed")
					case "routing failure":
						coordinatorActionFault(t, f, "issues", "UPDATE OF workflow_state_name", "", "route failed")
					}
				}
				if outcome == "human comment failure" {
					coordinatorActionFault(t, f, "native_comments", "INSERT", "", "comment failed")
				}
				if outcome == "human transition failure" {
					coordinatorActionFault(t, f, "issues", "UPDATE OF workflow_state_name", "", "transition failed")
				}
				if outcome == "human park recorded on submit" {
					coordinatorActionHumanPark(t, f, string(id))
				}
				if outcome == "completion without entitlement" {
					coordinatorActionRemoveCollaborationEntitlement(t, f)
				}
				var restoreArchiveRepository func()
				if outcome == "concurrent replay" {
					f.service.tokenUseWork.Wait()
					original := f.service.config.TenantRepository
					observed := &coordinatorArchiveReplayRepository{Repository: original, operation: nativeOperation(nativeScope{organization: "org_security", project: f.project}, "POST", "/work-items/archive")}
					observed.afterReceipt = func(ctx context.Context) {
						credential, err := currentHubOperator(ctx)
						if err != nil {
							t.Fatal(err)
						}
						var archive chat.IssueArchive
						if err := json.Unmarshal(action.Arguments, &archive); err != nil {
							t.Fatal(err)
						}
						scope := nativeScope{organization: "org_security", project: f.project, credential: credential}
						if _, err := f.service.executeCoordinatorArchiveTenant(ctx, scope, action, archive); err != nil {
							t.Fatal(err)
						}
					}

					f.service.config.TenantRepository = observed
					restoreArchiveRepository = func() {
						f.service.config.TenantRepository = original
						observed.mu.Lock()
						pending := observed.afterReceipt != nil
						observed.mu.Unlock()
						if pending {
							t.Fatal("archive replay did not interleave after the missing receipt")
						}
					}
					t.Cleanup(func() { f.service.config.TenantRepository = original })
				}
				response = f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
				if restoreArchiveRepository != nil {
					restoreArchiveRepository()
				}
				changed := outcome == "completion without entitlement" || outcome == "concurrent replay" || outcome == "human answer" || outcome == "dependency park" || outcome == "full access" || outcome == "preserve access" || outcome == "execute" || outcome == "execute with manual import" || outcome == "validation then valid" || outcome == "wrong role" || outcome == "no runner grant" || outcome == "grantless project" || outcome == "stale" && tool == operatortool.AddComment
				if changed {
					requireNativeStatus(t, response, http.StatusOK)
				} else if outcome == "runner grant revoked" {
					requireNativeStatus(t, response, http.StatusNotFound)
				} else if response.Code < 400 {
					t.Fatalf("refused call=%d %s", response.Code, response.Body.String())
				}
				if outcome == "human park recorded on submit" {
					requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				}
				if outcome == "human park recorded on submit" && !strings.Contains(response.Body.String(), "human's answer") {
					t.Fatalf("submission did not ask for the answer: %s", response.Body.String())
				}
				f.service.spriteWakeWork.Wait()
				assertCoordinatorEffect(t, f, id, tool, changed)
				if parkedMove {
					if changed {
						requireNativeStatus(t, f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
					}
					var comments, transitions int
					var reason string
					savedComments := coordinatorActionComments(t, f, id)
					comments = len(savedComments)
					for _, event := range coordinatorActionEvents(t, f, id) {
						if event.Type == "workflow.transitioned" {
							transitions++
							if event.Data.ReasonDetail > reason {
								reason = event.Data.ReasonDetail
							}
						}
					}
					wantTransitions, wantComments := 0, 0
					if changed {
						wantTransitions = 1
					}
					if changed && humanPark {
						wantComments = 1
						answer := arguments["reason_detail"].(string)
						if reason != answer || !strings.Contains(response.Body.String(), "Recorded the human's answer") {
							t.Fatalf("answer missing: reason=%q result=%s", reason, response.Body.String())
						}
						var comment tracker.NativeComment
						if len(savedComments) > 0 {
							comment = savedComments[0]
						}
						if err != nil || comment.Body != answer || comment.Actor.Kind != "human" || comment.Actor.PrincipalID != operatortool.ConnectionIdentity(ctx).PrincipalID || comment.Provenance == nil || comment.Provenance.Provider != "chat" || comment.Provenance.AuthorID != operatortool.ConnectionIdentity(ctx).PrincipalID || !strings.HasPrefix(comment.Provenance.ExternalID, record.ID+":") {
							t.Fatalf("answer comment=%+v error=%v", comment, err)
						}
					} else if reason != "" {
						t.Fatalf("unexpected transition answer %q", reason)
					}
					if comments != wantComments || transitions != wantTransitions {
						t.Fatalf("comments=%d transitions=%d, want %d and %d", comments, transitions, wantComments, wantTransitions)
					}
				}

				if outcome == "full access" || outcome == "preserve access" {
					want := "sandbox"
					if outcome == "full access" {
						want = "native-trusted"
					}
					var got string
					if err := db.QueryRowContext(t.Context(), "SELECT isolation_tier FROM organization_sprite_pools WHERE organization_id='org_security'").Scan(&got); err != nil || got != want {
						t.Fatalf("saved tier=%q want=%q err=%v", got, want, err)
					}
				}
				if tool == "update_project_integration" {
					wantIntake := "disabled"
					if outcome == "execute with manual import" {
						wantIntake = "manual"
					}
					var intake string
					f.readTenant(t, func(r tenantstore.Reader) error {
						p, err := r.ProjectRecord(t.Context(), f.project)
						intake = p.Settings.Intake
						return err
					})
					if intake != wantIntake {
						t.Fatalf("stored intake = %q, want %q", intake, wantIntake)
					}
				}
				if tool == "scale_up_sprite_pool" && changed {
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "get_sprite_bootstrap_log", Arguments: json.RawMessage(`{}`)})
					if err != nil || !result.Success || !strings.Contains(result.Content, "billing enabled") || !strings.Contains(result.Content, "scale_up_sprite_pool") || strings.Contains(result.Content, spritesSecretSentinel) {
						t.Fatalf("unsafe or missing bootstrap failure/retry: %+v, %v", result, err)
					}
				}
				if changed && tool == string(chat.ActionIssueSplit) {
					requireNativeStatus(t, f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
					var count int
					if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id='org_security' AND project_id=$1 AND title LIKE 'Split %'", f.project).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 3 {
						t.Fatalf("confirmation replay created %d children", count)
					}
				}
				if changed && tool == string(chat.ActionArchiveItems) {
					requireNativeStatus(t, f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
					assertCoordinatorArchiveEffect(t, f, true)
					for _, id := range archiveIDs {
						if err := f.service.refreshBoardRecord(t.Context(), "org_security", viewstore.BoardCandidate{ProjectID: f.project, WorkItemID: tracker.NativeWorkItemID(id)}, f.service.config.now()); err != nil {
							t.Fatal(err)
						}
					}
					credential, err := currentHubOperator(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, archived := range []string{"", "true"} {
						result, err := (operatorWorkReads{service: f.service, scope: nativeScope{organization: "org_security", project: f.project, credential: credential}}).ReadWork(ctx, operatortool.WorkList, operatortool.WorkReadRequest{ProjectID: string(f.project), Archived: archived, Limit: 100})
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
					messages, err = c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100, Oldest: true})
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

func assertCoordinatorEffect(t *testing.T, f coordinatorActionFixture, id tracker.NativeWorkItemID, tool string, changed bool) {
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
		err = f.admin.QueryRowContext(t.Context(), "SELECT max_runners=0 FROM organization_sprite_pools WHERE organization_id='org_security'").Scan(&actual)
	case "scale_up_sprite_pool":
		err = f.admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM organization_sprite_members WHERE organization_id='org_security')").Scan(&actual)
	case "update_project_integration":
		f.readTenant(t, func(r tenantstore.Reader) error {
			p, err := r.ProjectRecord(t.Context(), f.project)
			actual = p.Settings.RepositoryEnabled
			return err
		})
	case operatortool.MoveItem, "propose_move_item":
		issue := coordinatorActionIssue(t, f, id)
		actual = issue.State == "Done"
	case "propose_stop_run":
		issue := coordinatorActionIssue(t, f, id)
		actual = issue.State == "Blocked" && issue.Priority != nil && *issue.Priority == 1
		var released bool
		f.readTenant(t, func(r tenantstore.Reader) error {
			lease, err := r.Lease(t.Context(), "archive-lease")
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			released = lease.ReleasedAt != nil && lease.ReleaseReason == "cancelled"
			return err
		})
		if released != changed {
			t.Fatalf("stop released=%v, want %v", released, changed)
		}
	case "propose_file_issue", "propose_maintenance_issue":
		err = f.admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM issues WHERE organization_id=$1 AND project_id=$2 AND title='Slash issue')", "org_security", f.project).Scan(&actual)
	case "propose_backlog_admission":
		issue := coordinatorActionIssue(t, f, id)
		actual = issue.State == "Todo"
	case "propose_set_priority":
		issue := coordinatorActionIssue(t, f, id)
		actual = issue.Priority != nil && *issue.Priority == 1
	case operatortool.EditItem:
		issue := coordinatorActionIssue(t, f, id)
		actual = issue.Title == "Edited by Luna" && issue.Body == nativeContractTestBody && issue.Priority != nil && *issue.Priority == 2 && len(issue.Labels) == 1 && issue.Labels[0] == "approved"
		if !changed && (issue.Title != "Original issue" || issue.Body != nativeContractTestBody || issue.Priority != nil || len(issue.Labels) != 1 || issue.Labels[0] != "original") {
			t.Fatalf("unapproved edit changed the issue: %+v", issue)
		}
	case operatortool.AddComment:
		for _, comment := range coordinatorActionComments(t, f, id) {
			if comment.Body == "Comment approved by the owner" {
				actual = true
			}
		}
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
	organization := tracker.OrganizationID(service.appOrganizationID())
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), project)
		if err != nil {
			return err
		}
		record.Settings.Repository = "example/repo"
		record.Settings.RepositoryEnabled = false
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *browserHostedFixture) seedCoordinatorActions(t *testing.T) {
	t.Helper()
	seedCoordinatorRepository(t, f.service, tracker.ProjectID(f.project))
	var issue tracker.NativeIssue
	base := browserHostedOrganizationBase + "/projects/" + f.project
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/work-items", map[string]any{"idempotency_key": "luna-blocked", "title": "Landing blocked by branch rules", "body": "Enable GitHub pull-request mode and retry." + issueContractTestSections, "state": "Blocked"}, http.StatusOK), &issue)
	f.workItem = string(issue.WorkItemID)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project), credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	archiveIssues := seedCoordinatorActionIssues(t, f.service, scope, 5, "Todo")
	blockedArchive := seedCoordinatorActionIssues(t, f.service, scope, 1, "Merging")[0]
	coordinatorActionChange(t, f.service, "org_browser_preview", tracker.ProjectID(f.project), string(blockedArchive.WorkItemID), func(row *corestore.WorkItem) { row.Title = "Running cleanup issue" })
	seedCoordinatorArchiveLease(t, f.service, string(blockedArchive.WorkItemID))
	backend := f.service.conversations.config.Backend.(*fakeCoordinatorBackend)
	backend.setRun(func(ctx context.Context, turn int, handle runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		prompt := strings.ToLower(backend.request(t, turn-1).Prompt)
		if _, current, ok := strings.Cut(prompt, "</transcript>"); ok {
			prompt = current
		}
		call := runner.AgentToolCall{Name: "update_project_integration", Arguments: json.RawMessage(`{"repository_enabled":true}`)}
		if strings.Contains(prompt, "split this issue") || strings.Contains(prompt, "cyclic split") {
			result, err := f.proposeBrowserIssueSplit(ctx, handle, strings.Contains(prompt, "cyclic split"), strings.Contains(prompt, "six children"), "Todo")
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			text := "All 6 children will be created in Todo, as requested; work starts as soon as a runner is free."
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

func (f *browserHostedFixture) proposeBrowserIssueSplit(ctx context.Context, handle runner.AgentToolHandler, cyclic, sixChildren bool, childState string) (runner.AgentToolResult, error) {
	children := []chat.IssueSplitChild{{Title: "Split storage", Description: "Create the **storage layer**.\n\n- Preserve existing records.\n- Add the migration.\n\n```detent-agent\nschema: 1\neffort: high\n```", State: childState}, {Title: "Split API", Description: "Use the storage layer.", State: childState}, {Title: "Split UI", Description: "Render the approved UI.", State: childState}}
	edges := []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}, {Dependent: 0, Blocker: 1}, {Dependent: 0, Blocker: 2}, {Dependent: 0, Blocker: 3}}
	if sixChildren {
		children = append(children, chat.IssueSplitChild{Title: "Split runner", Description: "Run the approved work.", State: childState}, chat.IssueSplitChild{Title: "Split reporting", Description: "Report the saved results.", State: childState}, chat.IssueSplitChild{Title: "Split integration", Description: "Integrate the completed children.", State: childState})
		edges = append(edges, chat.IssueSplitEdge{Dependent: 3, Blocker: 1}, chat.IssueSplitEdge{Dependent: 3, Blocker: 2}, chat.IssueSplitEdge{Dependent: 5, Blocker: 4}, chat.IssueSplitEdge{Dependent: 6, Blocker: 3}, chat.IssueSplitEdge{Dependent: 6, Blocker: 5}, chat.IssueSplitEdge{Dependent: 0, Blocker: 4}, chat.IssueSplitEdge{Dependent: 0, Blocker: 5}, chat.IssueSplitEdge{Dependent: 0, Blocker: 6})
	}
	if cyclic {
		edges = append(edges, chat.IssueSplitEdge{Dependent: 1, Blocker: 2})
	}
	for i := range children {
		children[i].Description += issueContractTestSections
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

func assertCoordinatorSplitEffect(t *testing.T, f coordinatorActionFixture, parent tracker.NativeWorkItemID, changed bool) {
	t.Helper()
	db := f.admin
	var rows []corestore.WorkItem
	f.readTenant(t, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.WorkItems(t.Context(), f.project, 0, 200)
		return err
	})
	var children, edges, correctlyFiled int
	var blocker tracker.NativeWorkItemID
	var dependent tracker.WorkItemID
	for _, row := range rows {
		if strings.HasPrefix(row.Title, "Split ") {
			children++
			if valueOrEmpty(row.WorkflowStateName) == "Todo" {
				f.readTenant(t, func(r tenantstore.Reader) error {
					entry, err := r.QueueEntry(t.Context(), f.project, row.NativeID, string(f.project))
					if err == nil && entry.Priority != nil && *entry.Priority == 1 {
						correctlyFiled++
					}
					return err
				})
			}
		}
		if row.Title == "Split storage" {
			blocker = row.NativeID
		}
		if row.Title == "Split API" {
			dependent = tracker.WorkItemID(row.ID)
		}
	}
	comments := len(coordinatorActionComments(t, f, parent))
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_dependencies WHERE organization_id=$1 AND dependent_project_id=$2", "org_security", f.project).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if !changed {
		if children != 0 || edges != 0 || comments != 0 {
			t.Fatalf("partial split survived: children=%d edges=%d comments=%d", children, edges, comments)
		}
		var residue int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND type='issue.created' AND work_item_id<>$3", "org_security", f.project, parent).Scan(&residue); err != nil {
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
	if correctlyFiled != 3 {
		t.Fatalf("only %d children have the approved state and priority", correctlyFiled)
	}
	coordinatorActionProject(t, f, func(p *projectstore.Record) { p.Core.RequireDependencies = true })
	for _, terminal := range []bool{false, true} {
		if terminal {
			coordinatorActionItem(t, f, string(blocker), func(row *corestore.WorkItem) { row.WorkflowStateName = new("Done") })
		}
		ids := coordinatorActionCandidates(t, f)
		if slices.Contains(ids, dependent) != terminal {
			t.Fatalf("dependent dispatch=%v while blocker terminal=%v", slices.Contains(ids, dependent), terminal)
		}
	}
}

func seedCoordinatorArchiveLease(t *testing.T, service *Service, id string) {
	t.Helper()
	coordinatorActionLease(t, service, id)
}

func assertCoordinatorArchiveEffect(t *testing.T, f coordinatorActionFixture, changed bool) {
	t.Helper()
	var rows []corestore.WorkItem
	f.readTenant(t, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.WorkItems(t.Context(), f.project, 0, 200)
		return err
	})
	var archived, history int
	for _, row := range rows {
		if row.Archived && row.Revision > 1 && (row.Title == "Original issue" || strings.HasPrefix(row.Title, "Seed ")) {
			archived++
		}
		for _, event := range coordinatorActionEvents(t, f, row.NativeID) {
			if event.Data.Operation == "archive" {
				history++
			}
		}
	}
	want := 0
	if changed {
		want = 5
	}
	if history != want || archived != want {
		t.Fatalf("archive effect: archived=%d history=%d want=%d", archived, history, want)
	}
	if changed {
		ids := coordinatorActionCandidates(t, f)
		if len(ids) != 0 {
			t.Fatalf("archived issues remain dispatchable: %v", ids)
		}
	}
}
