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
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorProjectActions(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"propose_stop_run", "propose_file_issue", "propose_maintenance_issue", "propose_move_item", "propose_set_priority", "propose_backlog_admission", "update_project_integration", operatortool.MoveItem, operatortool.EditItem, operatortool.AddComment, string(chat.ActionIssueSplit), string(chat.ActionArchiveItems), "set_sprite_pool", "scale_up_sprite_pool"} {
		if tool == string(chat.ActionIssueSplit) {
			runCoordinatorIssueSplitPostgresCases(t)
			continue
		}
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
		if tool == string(chat.ActionArchiveItems) {
			outcomes = append(outcomes, "running", "merging", "became running", "became merging", "archive failure", "history failure", "already archived", "duplicate", "empty", "outside project", "number resolution")
		}
		if coordinatorSpriteMutation(tool) {
			outcomes = append(outcomes, "no runner grant", "runner grant revoked", "grantless project")
		}
		if tool == "set_sprite_pool" {
			outcomes = append(outcomes, "intake preview", "sandbox access", "full access", "preserve access")
		}
		for _, outcome := range outcomes {
			t.Run(tool+"/"+outcome, func(t *testing.T) {
				t.Parallel()
				sharedEntry := strings.HasPrefix(outcome, "shared entry")
				mcpPost := sharedEntry || outcome == "stored key preview"
				var clock atomic.Int64
				clock.Store(time.Now().UnixNano())
				configure := func(cfg *Config) {
					cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir()}
					if sharedEntry {
						cfg.Clock = func() time.Time { return time.Unix(0, clock.Load()) }
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
				var f hostedSecurityFixture
				var shared hostedSharedFixture
				if sharedEntry {
					shared = newHostedSharedFixture(t, configure)
					f = shared.hostedSecurityFixture
				} else {
					f = newHostedSecurityFixture(t, configure)
				}
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
						fresh.IssuedAt = f.service.config.Clock()
						fresh.ExpiresAt = fresh.IssuedAt.Add(cloudassert.MaxLifetime)
						return fresh, nil
					}
				}
				var storedToken string
				coordinatorFixtureProject(t, f.service, "org_security", f.project, func(row *projectstore.Record) { row.Settings.DuplicateChallenge = false })
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
					key, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Name: "coordinator", Scope: hubapi.APIScopeOperator, Issuer: &credential, KeyScope: apikey.ScopeWrite, ExpiresAt: &expires, ProjectIDs: []string{string(f.project)}, ProjectAccess: hubapi.HostedProjectsSelected})
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
						coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
							_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: "prj_grantless", Name: "owner/repository", Profile: "github_compatible"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}})
							return err
						})
					}
					if _, err := f.service.approveTestTenantPolicy(t.Context(), "org_security/"+string(f.project), "test", policy.Change{Policy: hubTestPolicy()}); err != nil {
						t.Fatal(err)
					}
				}
				if outcome == "number resolution" {
					seedArchiveIssues(t, f.service, hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}, 18, "Todo")
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
				humanPark := tool == operatortool.MoveItem && strings.HasPrefix(outcome, "human ")
				parkedMove := humanPark || tool == operatortool.MoveItem && outcome == "dependency park"
				if parkedMove {
					coordinatorActionState(t, f.service, "org_security", f.project, tracker.NativeState{Name: "Blocked", Transitions: []string{"Done"}}, false)
					coordinatorActionWriteIssue(t, f.service, "org_security", f.project, id, func(issue *tracker.NativeIssue) { issue.State = "Blocked" })
					coordinatorActionParkAttempt(t, f.service, "org_security", f.project, id, "archive-lease", humanPark && outcome != "human park recorded on submit")
				}
				if tool == "propose_stop_run" {
					coordinatorActionState(t, f.service, "org_security", f.project, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}}, true)
					if outcome != "no active attempt" {
						seedCoordinatorArchiveLease(t, f.service, string(id))
					}
				}
				if tool == "propose_backlog_admission" {
					coordinatorActionState(t, f.service, "org_security", f.project, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}}, false)
					coordinatorActionWriteIssue(t, f.service, "org_security", f.project, id, func(issue *tracker.NativeIssue) { issue.State = "Backlog" })
				}
				seedCoordinatorRepository(t, f.service, f.project)
				if outcome == "execute with manual import" {
					coordinatorFixtureProject(t, f.service, "org_security", f.project, func(row *projectstore.Record) { row.Settings.Intake = "manual" })
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
								claims.Subject, claims.ProviderOrganization, claims.Role = u.identity.Subject, "org_provider", "owner"
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
				c := f.service.conversations.Coordinator.(*conversationTurnCoordinator)
				record, err := c.service.ReadPublicationConversation(t.Context(), created.Conversation.ID)
				if err != nil {
					t.Fatal(err)
				}
				messages, err := c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100})
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
					waitCoordinatorIdle(t, t.Context(), f.service)
					f.service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{f.service}, chat.WithClock(f.service.config.Clock), chat.WithSessionStore(operatorChatStore{f.service.database}, f.service.resolveOperatorChatAuthority))
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
						if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							persisted, err = r.OperatorSession(t.Context(), operatortool.CurrentConnection(ctx).ID, f.service.config.Clock().Add(-time.Hour))
							return err
						}); err != nil {
							t.Fatal(err)
						}
						rawOrigin, err := json.Marshal(persisted.Origin)
						if err != nil {
							t.Fatal(err)
						}
						var origin map[string]any
						if err := json.Unmarshal(rawOrigin, &origin); err != nil {
							t.Fatal(err)
						}
						if origin["credential_binding"] != strings.Repeat("a", 64) || origin["key_id"] != "key_coordinator" || origin["exp"] != nil || origin["iat"] != nil || origin["token"] != nil || origin["token_hash"] != nil {
							t.Fatalf("persisted request credentials or missing stable origin: %v", origin)
						}
					}
					identity := operatortool.ConnectionIdentity(ctx)
					var principal string
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
						member, err := r.AccessMember(t.Context(), u.identity.Subject)
						principal = member.PrincipalID
						return err
					}); err != nil {
						t.Fatal(err)
					}
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
					waitCoordinatorIdle(t, t.Context(), f.service)
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
						coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
							return w.RevokeAccessMember(t.Context(), u.identity.Subject, f.service.config.Clock())
						})
					case "shared entry changed issuer":
						f.service.config.Hosted.SharedEntry.Issuer = "other-entry"
					case "shared entry changed generation":
						f.service.config.Hosted.SharedEntry.Generation++
					case "shared entry no write grant":
						f.grant(t, u, false, false)
					}
					if sharedEntry {
						f.service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{f.service}, chat.WithClock(f.service.config.Clock), chat.WithSessionStore(operatorChatStore{f.service.database}, f.service.resolveOperatorChatAuthority))
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
					coordinatorActionSpriteSeed(t, f, u)
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
					coordinatorActionState(t, f.service, "org_security", f.project, tracker.NativeState{Name: "Merging"}, false)
					scope := hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}
					children := 4
					if outcome == "number resolution" {
						coordinatorActionState(t, f.service, "org_security", f.project, tracker.NativeState{Name: "Blocked"}, false)
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
							coordinatorActionWriteIssue(t, f.service, "org_security", f.project, tracker.NativeWorkItemID(workItemID), func(issue *tracker.NativeIssue) { issue.State = state })
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
						coordinatorActionWriteIssue(t, f.service, "org_security", f.project, tracker.NativeWorkItemID(archiveIDs[4]), func(issue *tracker.NativeIssue) { issue.State = "Merging" })
					}
					if outcome == "already archived" {
						coordinatorActionWriteIssue(t, f.service, "org_security", f.project, tracker.NativeWorkItemID(archiveIDs[4]), func(issue *tracker.NativeIssue) { issue.Archived = true })
					}
					if outcome == "outside project" {
						response := f.request(t, u, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "archive-other-project", "name": "Other project", "grant_access": true})
						requireNativeStatus(t, response, http.StatusCreated)
						var created struct {
							ProjectID tracker.ProjectID `json:"project_id"`
						}
						decodeHubResponse(t, response, &created)
						scope.Project = created.ProjectID
						foreign := seedArchiveIssues(t, f.service, scope, 1, "Todo")[0]
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
				case "set_sprite_pool":
					arguments = map[string]any{"min_runners": 0, "max_runners": 0}
					if outcome == "sandbox access" {
						arguments["isolation_tier"] = "sandbox"
					}
					if outcome == "full access" {
						arguments["isolation_tier"] = "native-trusted"
					}
					if outcome == "preserve access" {
						coordinatorActionPoolChange(t, f.service, func(row *postgres.SpritePool) { row.IsolationTier = "sandbox" })
					}
					if outcome == "intake preview" {
						arguments = map[string]any{"min_runners": 2, "max_runners": 2}
					}
				case "scale_up_sprite_pool":
					arguments = map[string]any{}
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
					coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
						member, err := w.AccessMember(t.Context(), u.identity.Subject)
						if err != nil {
							return err
						}
						member.Role = "viewer"
						return w.UpdateAccessMember(t.Context(), member)
					})
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
				if tool == string(chat.ActionArchiveItems) && result.Success && outcome != "number resolution" {
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
				messages, err = c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100})
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
					pool := coordinatorActionPool(t, f.service)
					floor, ceiling, bootstrap = pool.MinRunners, pool.MaxRunners, pool.Bootstrap
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
					for _, item := range coordinatorActionItems(t, f.service, "org_security", f.project) {
						if item.Archived {
							archived++
						}
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
						coordinatorActionPoolChange(t, f.service, func(*postgres.SpritePool) {})
					} else if tool == "update_project_integration" {
						coordinatorFixtureProject(t, f.service, "org_security", f.project, func(*projectstore.Record) {})
					} else if tool != operatortool.AddComment {
						staleID := id
						if tool == string(chat.ActionArchiveItems) {
							staleID = tracker.NativeWorkItemID(archiveIDs[4])
						}
						coordinatorActionWriteIssue(t, f.service, "org_security", f.project, staleID, func(*tracker.NativeIssue) {})
					}
				}
				if tool == string(chat.ActionArchiveItems) {
					if outcome == "became running" {
						seedCoordinatorArchiveLease(t, f.service, archiveIDs[4])
					}
					if outcome == "became merging" {
						coordinatorActionWriteIssue(t, f.service, "org_security", f.project, tracker.NativeWorkItemID(archiveIDs[4]), func(issue *tracker.NativeIssue) { issue.State = "Merging" })
					}
				}
				if tool == "propose_stop_run" {
					switch outcome {
					case "replaced attempt":
						coordinatorActionRelease(t, f.service, "org_security", "archive-lease", "released")
						coordinatorActionLease(t, f.service, "org_security", id, "replacement-lease")
					case "expired attempt":
						if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=$1::timestamptz - interval '2 seconds', renewed_at=$1::timestamptz - interval '1 second', expires_at=$1 WHERE organization_id=$2 AND lease_id=$3", f.service.config.Clock().Add(-time.Second), "org_security", "archive-lease"); err != nil {
							t.Fatal(err)
						}
					}
				}
				if outcome == "human park recorded on submit" {
					coordinatorActionParkAttempt(t, f.service, "org_security", f.project, id, "human-action-lease", true)
				}
				var injected atomic.Bool
				faultPhase := ""
				switch outcome {
				case "archive failure":
					faultPhase = "archive"
				case "history failure":
					faultPhase = "history"
				case "release failure":
					faultPhase = "release"
				case "routing failure", "human transition failure":
					faultPhase = "transition"
				case "human comment failure":
					faultPhase = "comment"
				}
				if faultPhase != "" {
					target := id
					if tool == string(chat.ActionArchiveItems) {
						target = tracker.NativeWorkItemID(archiveIDs[4])
					}
					if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
						t.Fatal(err)
					}
					waitCoordinatorIdle(t, t.Context(), f.service)
					original := f.service.config.TenantRepository
					f.service.config.TenantRepository = coordinatorActionFailureRepository{Repository: original, phase: faultPhase, target: target, injected: &injected}
					t.Cleanup(func() {
						waitCoordinatorIdle(t, context.Background(), f.service)
						f.service.config.TenantRepository = original
					})
				}

				response = f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
				if faultPhase != "" && !injected.Load() {
					t.Fatalf("%s fixture did not reach its write failure", faultPhase)
				}
				changed := outcome == "human answer" || outcome == "dependency park" || outcome == "full access" || outcome == "preserve access" || outcome == "execute" || outcome == "execute with manual import" || outcome == "validation then valid" || outcome == "wrong role" || outcome == "no runner grant" || outcome == "grantless project" || outcome == "stale" && tool == operatortool.AddComment
				if changed {
					requireNativeStatus(t, response, http.StatusOK)
				} else if outcome == "runner grant revoked" {
					requireNativeStatus(t, response, http.StatusNotFound)
				} else if response.Code < 400 {
					t.Fatalf("refused call=%d %s", response.Code, response.Body.String())
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
					storedComments := coordinatorActionComments(t, f.service, "org_security", f.project, id)
					comments = len(storedComments)
					for _, event := range coordinatorActionEvents(t, f.service, "org_security", f.project, id) {
						if event.Type == "workflow.transitioned" {
							transitions++
							reason = event.Data.ReasonDetail
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
						if len(storedComments) == 1 {
							comment = storedComments[0]
						}
						if err != nil || comment.Body != answer || comment.Actor.Kind != "human" || comment.Actor.PrincipalID != operatortool.ConnectionIdentity(ctx).PrincipalID || comment.Provenance == nil || comment.Provenance.Provider != "chat" || !strings.HasPrefix(comment.Provenance.ExternalID, record.ID+":") {
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
					got = coordinatorActionPool(t, f.service).IsolationTier
					if got != want {
						t.Fatalf("saved tier=%q want=%q err=%v", got, want, nil)
					}
				}
				if tool == "update_project_integration" {
					wantIntake := "disabled"
					if outcome == "execute with manual import" {
						wantIntake = "manual"
					}
					var intake string
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.ProjectRecord(t.Context(), f.project)
						intake = row.Settings.Intake
						return err
					}); err != nil {
						t.Fatal(err)
					}
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
				if changed && tool == string(chat.ActionArchiveItems) {
					requireNativeStatus(t, f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
					assertCoordinatorArchiveEffect(t, f, true)
					for _, archived := range []string{"", "true"} {
						result, err := (operatorWorkReads{service: f.service, scope: hubscope.NativeScope{Organization: "org_security", Project: f.project}}).ReadWork(ctx, operatortool.WorkList, operatortool.WorkReadRequest{ProjectID: string(f.project), Archived: archived, Limit: 100})
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
					messages, err = c.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100})
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
	case "set_sprite_pool":
		actual = coordinatorActionPool(t, f.service).MaxRunners == 0
	case "scale_up_sprite_pool":
		err = f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			members, err := owner.SpriteMembers(t.Context(), "", 1)
			actual = len(members) > 0
			return err
		})
	case "update_project_integration":
		err = f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.ProjectRecord(t.Context(), f.project)
			actual = row.Settings.RepositoryEnabled
			return err
		})
	case operatortool.MoveItem, "propose_move_item":
		issue, readErr := coordinatorActionIssue(t.Context(), f.service, "org_security", f.project, id)
		err, actual = readErr, issue.State == "Done"
	case "propose_stop_run":
		issue, readErr := coordinatorActionIssue(t.Context(), f.service, "org_security", f.project, id)
		err, actual = readErr, issue.State == "Blocked" && issue.Priority != nil && *issue.Priority == 1
		var released bool
		if queryErr := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			lease, err := r.Lease(t.Context(), "archive-lease")
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			released = lease.ReleasedAt != nil && lease.ReleaseReason == "cancelled"
			return err
		}); queryErr != nil {
			t.Fatal(queryErr)
		}
		if released != changed {
			t.Fatalf("stop released=%v, want %v", released, changed)
		}
	case "propose_file_issue", "propose_maintenance_issue":
		for _, item := range coordinatorActionItems(t, f.service, "org_security", f.project) {
			actual = actual || item.Title == "Slash issue"
		}
	case "propose_backlog_admission":
		issue, readErr := coordinatorActionIssue(t.Context(), f.service, "org_security", f.project, id)
		err, actual = readErr, issue.State == "Todo"
	case "propose_set_priority":
		issue, readErr := coordinatorActionIssue(t.Context(), f.service, "org_security", f.project, id)
		err, actual = readErr, issue.Priority != nil && *issue.Priority == 1
	case operatortool.EditItem:
		issue, readErr := coordinatorActionIssue(t.Context(), f.service, "org_security", f.project, id)
		err, actual = readErr, issue.Title == "Edited by Luna" && issue.Body == nativeContractTestBody && issue.Priority != nil && *issue.Priority == 2 && len(issue.Labels) == 1 && issue.Labels[0] == "approved"
		if !changed && (issue.Title != "Original issue" || issue.Body != nativeContractTestBody || issue.Priority != nil || len(issue.Labels) != 1 || issue.Labels[0] != "original") {
			t.Fatalf("unapproved edit changed the issue: %+v", issue)
		}
	case operatortool.AddComment:
		for _, comment := range coordinatorActionComments(t, f.service, "org_security", f.project, id) {
			actual = actual || comment.Body == "Comment approved by the owner"
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
	organization := tracker.OrganizationID(service.config.Hosted.OrganizationID)
	coordinatorFixtureProject(t, service, organization, project, func(row *projectstore.Record) {
		row.Settings.Repository = "example/repo"
		row.Settings.CheckoutRepository = "example/repo"
		row.Settings.RepositoryEnabled = false
	})
}

func (f *browserHostedFixture) seedCoordinatorActions(t *testing.T) {
	t.Helper()
	seedCoordinatorRepository(t, f.service, tracker.ProjectID(f.project))
	var issue tracker.NativeIssue
	base := browserHostedOrganizationBase + "/projects/" + f.project
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/work-items", map[string]any{"idempotency_key": "luna-blocked", "title": "Landing blocked by branch rules", "body": "Enable GitHub pull-request mode and retry." + issueContractTestSections, "state": "Blocked"}, http.StatusOK), &issue)
	f.workItem = string(issue.WorkItemID)
	scope := hubscope.NativeScope{Organization: "org_browser_preview", Project: tracker.ProjectID(f.project), Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}
	archiveIssues := seedArchiveIssues(t, f.service, scope, 5, "Todo")
	blockedArchive := seedArchiveIssues(t, f.service, scope, 1, "Merging")[0]
	coordinatorActionWriteIssue(t, f.service, "org_browser_preview", tracker.ProjectID(f.project), blockedArchive.WorkItemID, func(issue *tracker.NativeIssue) { issue.Title = "Running cleanup issue" })
	seedCoordinatorArchiveLease(t, f.service, string(blockedArchive.WorkItemID))
	backend := f.service.conversations.Config.Backend.(*fakeCoordinatorBackend)
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

func seedCoordinatorArchiveLease(t *testing.T, service *Service, id string) {
	t.Helper()
	coordinatorActionLease(t, service, tracker.OrganizationID(service.config.Hosted.OrganizationID), tracker.NativeWorkItemID(id), "archive-lease")
}

func assertCoordinatorArchiveEffect(t *testing.T, f hostedSecurityFixture, changed bool) {
	t.Helper()
	archived, history := 0, 0
	for _, item := range coordinatorActionItems(t, f.service, "org_security", f.project) {
		if item.Archived && item.Revision > 1 && (item.Title == "Original issue" || strings.HasPrefix(item.Title, "Seed ")) {
			archived++
		}
		for _, event := range coordinatorActionEvents(t, f.service, "org_security", f.project, item.NativeID) {
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
		var ids []claimstore.Candidate
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: f.project, Scope: string(f.project), AvailableAt: f.service.config.Clock(), Limit: 1})
			ids = page.Items
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Fatalf("archived issues remain dispatchable: %v", ids)
		}
	}
}

func runCoordinatorIssueSplitPostgresCases(t *testing.T) {
	t.Helper()
	outcomes := []string{"execute", "reject", "unauthorized", "revoked", "stale", "foreign issue", "expired session", "wrong role", "no write grant", "bad arguments", "child failure", "edge failure", "comment failure", "invalid state", "cycle", "validation then valid", "multiple edges"}
	for _, outcome := range outcomes {
		t.Run(string(chat.ActionIssueSplit)+"/"+outcome, func(t *testing.T) {
			t.Parallel()
			provider := newHostedSecurityProvider()
			fixture := newConfiguredCoordinatorFixture(t, "Approved split", func(cfg *Config) {
				cfg.Hosted = &hostedplans.HostedConfig{OrganizationID: "org_split", WorkOSOrganizationID: "provider_org_split", BootstrapSubject: "user_org_split", PublicURL: "https://hub.example.test", Provider: provider}
			})
			service := fixture.service
			if err := service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			repository := service.config.TenantRepository
			for _, organization := range []tracker.OrganizationID{"org_split", "org_split_second"} {
				t.Run(string(organization), func(t *testing.T) {
					waitCoordinatorIdle(t, t.Context(), service)
					selectPostgresHubTestOrganization(service, organization)
					native := newNativeFixture(t, service, organization, "Split scope")
					f := hostedSecurityFixture{service: service, provider: provider, project: native.project.ID, base: native.base}
					now := service.config.Clock()
					role := "owner"
					if outcome == "wrong role" {
						role = "member"
					}
					identity := auth.Identity{Subject: "user_" + string(organization) + "_split", Email: "split@example.test", EmailVerified: true,
						Hosted: &auth.HostedIdentity{Subject: "user_" + string(organization) + "_split", OrganizationID: "provider_" + string(organization), SessionID: "session_" + string(organization) + "_split", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
					provider.mu.Lock()
					provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
					provider.mu.Unlock()
					membership, err := provider.CreateMembership(t.Context(), identity.Subject, identity.Hosted.OrganizationID, role)
					if err != nil {
						t.Fatal(err)
					}
					if err := service.addHostedMember(t.Context(), identity, membership); err != nil {
						t.Fatal(err)
					}
					token, _, err := service.hostedSessions.CreateIdentitySession(t.Context(), identity)
					if err != nil {
						t.Fatal(err)
					}
					user := hostedSecurityUser{identity: identity, token: token}
					grant := func(write bool) {
						t.Helper()
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							member, err := w.AccessMember(t.Context(), identity.Subject)
							if err != nil {
								return err
							}
							if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: identity.Subject, ProjectID: f.project, CanWrite: write}); err != nil {
								return err
							}
							return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: f.project})
						}); err != nil {
							t.Fatal(err)
						}
					}
					grant(true)
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						project, err := w.ProjectRecord(t.Context(), f.project)
						if err != nil {
							return err
						}
						project.Core.RequireDependencies = true
						project.Settings.DuplicateChallenge = false
						_, err = w.UpdateProjectRecord(t.Context(), project)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					response := f.request(t, user, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "split-parent"}, Title: "Original issue", Body: nativeContractTestBody, State: "Todo", Labels: []string{"original"}})
					requireNativeStatus(t, response, http.StatusOK)
					var parent tracker.NativeIssue
					decodeHubResponse(t, response, &parent)
					var sourceSequence int64
					readSequence := func() {
						t.Helper()
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							sourceSequence, err = r.ProjectEventSequence(t.Context(), f.project)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					readSequence()
					response = f.request(t, user, http.MethodPost, f.base+"/conversations", map[string]any{"key": "split-conversation", "first_message": map[string]any{"key": "split-message", "text": "Please split this issue"}})
					requireNativeStatus(t, response, http.StatusCreated)
					var created struct {
						Conversation struct {
							ID string `json:"id"`
						} `json:"conversation"`
					}
					decodeHubResponse(t, response, &created)
					coordinator := service.conversations.Coordinator.(*conversationTurnCoordinator)
					record, err := coordinator.readConversation(t.Context(), nil, created.Conversation.ID)
					if err != nil {
						t.Fatal(err)
					}
					messages, err := coordinator.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Limit: 100, Oldest: true})
					if err != nil || len(messages) != 1 {
						t.Fatalf("split user messages=%d err=%v", len(messages), err)
					}
					tools := newCoordinatorToolset(coordinator, &coordinatorTurnState{coordinator: coordinator, conversationID: record.ID, users: messages})
					priority := 1
					split := chat.IssueSplit{ParentID: string(parent.WorkItemID), Children: []chat.IssueSplitChild{
						{Title: "Split storage", Description: "Create storage" + issueContractTestSections, Priority: &priority, State: "Todo"},
						{Title: "Split API", Description: "Use storage" + issueContractTestSections, Priority: &priority, State: "Todo"},
						{Title: "Split UI", Description: "Build UI" + issueContractTestSections, Priority: &priority, State: "Todo"},
					}, Edges: []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}}}
					if outcome == "multiple edges" {
						split.Edges = []chat.IssueSplitEdge{{Dependent: 3, Blocker: 2}, {Dependent: 2, Blocker: 1}, {Dependent: 3, Blocker: 1}, {Dependent: 0, Blocker: 3}}
					}
					if outcome == "foreign issue" {
						other := tracker.OrganizationID("org_split")
						if organization == other {
							other = "org_split_second"
						}
						waitCoordinatorIdle(t, t.Context(), service)
						selectPostgresHubTestOrganization(service, other)
						foreign := newNativeFixture(t, service, other, "Foreign split scope").create(t, "Foreign issue")
						selectPostgresHubTestOrganization(service, organization)
						split.ParentID = string(foreign.WorkItemID)
					}
					if outcome == "invalid state" {
						split.Children[1].State = "Missing"
					}
					if outcome == "cycle" {
						split.Edges = append(split.Edges, chat.IssueSplitEdge{Dependent: 1, Blocker: 2})
					}
					if outcome == "no write grant" {
						grant(false)
					}
					if outcome == "unauthorized" {
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							member, err := w.AccessMember(t.Context(), identity.Subject)
							if err != nil {
								return err
							}
							member.Role = "viewer"
							return w.UpdateAccessMember(t.Context(), member)
						}); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "expired session" {
						provider.mu.Lock()
						delete(provider.sessions, identity.Hosted.SessionID)
						provider.mu.Unlock()
					}
					arguments, err := json.Marshal(split)
					if err != nil {
						t.Fatal(err)
					}
					if outcome == "bad arguments" {
						arguments = json.RawMessage(`{"unexpected":true}`)
					}
					if outcome == "validation then valid" {
						bad := split
						bad.Children = slices.Clone(split.Children)
						bad.Children[1].State = ""
						raw, err := json.Marshal(bad)
						if err != nil {
							t.Fatal(err)
						}
						result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: string(chat.ActionIssueSplit), Arguments: raw})
						if err != nil || result.Success || !strings.Contains(result.Content, "Each child requires a bounded title, description and target state") {
							t.Fatalf("validation feedback=%+v error=%v", result, err)
						}
						assertCoordinatorSplitPostgresEffect(t, f, organization, parent.WorkItemID, sourceSequence, split.Edges, false)
					}
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: string(chat.ActionIssueSplit), Arguments: arguments})
					if err != nil {
						t.Fatal(err)
					}
					if slices.Contains([]string{"unauthorized", "foreign issue", "expired session", "no write grant", "bad arguments", "invalid state", "cycle"}, outcome) {
						if result.Success || !strings.Contains(result.Content, "error") {
							t.Fatalf("refused preview=%+v", result)
						}
						assertCoordinatorSplitPostgresEffect(t, f, organization, parent.WorkItemID, sourceSequence, split.Edges, false)
						return
					}
					if !result.Success {
						t.Fatalf("preview=%s", result.Content)
					}
					replay, err := tools.handle(t.Context(), runner.AgentToolCall{Name: string(chat.ActionIssueSplit), Arguments: arguments})
					if err != nil || replay.Content != result.Content {
						t.Fatalf("proposal replay=%+v error=%v, want %s", replay, err, result.Content)
					}
					var preview struct {
						ActionID string `json:"action_id"`
						Status   string `json:"status"`
					}
					if err := json.Unmarshal([]byte(result.Content), &preview); err != nil || preview.Status != "proposed" {
						t.Fatalf("preview=%s error=%v", result.Content, err)
					}
					allMessages, err := coordinator.coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: record.ID, Limit: 100, Oldest: true})
					if err != nil {
						t.Fatal(err)
					}
					var action chat.Action
					for _, message := range allMessages {
						var data struct {
							Proposal struct {
								Action chat.Action `json:"action"`
							} `json:"operator_action"`
						}
						if json.Unmarshal(message.Data, &data) == nil && data.Proposal.Action.ID == preview.ActionID {
							action = data.Proposal.Action
						}
					}
					if action.RequestID == "" || !action.RequiresConfirmation {
						t.Fatalf("inline split proposal missing explicit confirmation: %+v", action)
					}
					assertCoordinatorSplitPostgresEffect(t, f, organization, parent.WorkItemID, sourceSequence, split.Edges, false)
					if outcome == "reject" {
						return
					}
					if outcome == "revoked" {
						grant(false)
					}
					if outcome == "stale" {
						title := "Changed after proposal"
						requireNativeStatus(t, f.request(t, user, http.MethodPatch, f.base+"/work-items/"+string(parent.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "split-stale"}, ExpectedRevision: parent.Revision, Title: &title}), http.StatusOK)
					}
					readSequence()
					injected := false
					waitCoordinatorIdle(t, t.Context(), service)
					if strings.HasSuffix(outcome, " failure") {
						service.config.TenantRepository = coordinatorSplitFailureRepository{Repository: repository, phase: strings.TrimSuffix(outcome, " failure"), injected: &injected}
					}
					response = f.request(t, user, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
					waitCoordinatorIdle(t, t.Context(), service)
					service.config.TenantRepository = repository
					if strings.HasSuffix(outcome, " failure") && !injected {
						t.Fatalf("split never reached the injected %s: %d %s", outcome, response.Code, response.Body.String())
					}
					changed := outcome == "execute" || outcome == "wrong role" || outcome == "validation then valid" || outcome == "multiple edges"
					if changed {
						requireNativeStatus(t, response, http.StatusOK)
					} else if response.Code < http.StatusBadRequest {
						t.Fatalf("refused confirmation=%d %s", response.Code, response.Body.String())
					}
					assertCoordinatorSplitPostgresEffect(t, f, organization, parent.WorkItemID, sourceSequence, split.Edges, changed)
					if changed {
						requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
						assertCoordinatorSplitPostgresEffect(t, f, organization, parent.WorkItemID, sourceSequence, split.Edges, true)
					}
				})
			}
		})
	}
}

type coordinatorSplitFailureRepository struct {
	tenantstore.Repository
	phase    string
	injected *bool
}

func (r coordinatorSplitFailureRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(coordinatorSplitFailureWriter{Writer: w, phase: r.phase, injected: r.injected})
	})
}

func (r coordinatorSplitFailureRepository) ReadRequest(ctx context.Context, organization tracker.OrganizationID, read func(context.Context) error) error {
	return r.Repository.(tenantstore.RequestReader).ReadRequest(ctx, organization, read)
}

type coordinatorSplitFailureWriter struct {
	tenantstore.Writer
	phase    string
	injected *bool
}

func (w coordinatorSplitFailureWriter) CreateWorkItem(ctx context.Context, item corestore.WorkItem) (corestore.WorkItem, error) {
	stored, err := w.Writer.CreateWorkItem(ctx, item)
	if err == nil && w.phase == "child" && item.Title == "Split API" {
		*w.injected = true
		err = errors.New("split child write failed")
	}
	return stored, err
}

func (w coordinatorSplitFailureWriter) SetRelationship(ctx context.Context, relationship corestore.Relationship, operation string, now time.Time) error {
	if err := w.Writer.SetRelationship(ctx, relationship, operation, now); err != nil {
		return err
	}
	if w.phase == "edge" {
		*w.injected = true
		return errors.New("split dependency write failed")
	}
	return nil
}

func (w coordinatorSplitFailureWriter) CreateComment(ctx context.Context, comment corestore.Comment) (corestore.Comment, error) {
	stored, err := w.Writer.CreateComment(ctx, comment)
	if err == nil && w.phase == "comment" {
		*w.injected = true
		err = errors.New("split comment write failed")
	}
	return stored, err
}

func assertCoordinatorSplitPostgresEffect(t *testing.T, f hostedSecurityFixture, organization tracker.OrganizationID, parent tracker.NativeWorkItemID, sourceSequence int64, expectedEdges []chat.IssueSplitEdge, changed bool) {
	t.Helper()
	ctx := t.Context()
	repository := f.service.config.TenantRepository
	var children []corestore.WorkItem
	var comments []corestore.Comment
	var edges []corestore.Relationship
	if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		items, err := r.WorkItems(ctx, f.project, 0, 20)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.OrganizationID != organization || item.ProjectID != f.project {
				t.Fatalf("foreign split item %+v", item)
			}
			relationships, err := r.Relationships(ctx, f.project, item.NativeID, corestore.RelationshipCursor{}, 20)
			if err != nil {
				return err
			}
			for _, relationship := range relationships.Items {
				if relationship.Type == "dependency" && relationship.Direction == "blocked_by" {
					edges = append(edges, relationship)
				}
			}
			if !strings.HasPrefix(item.Title, "Split ") {
				continue
			}
			children = append(children, item)
			if hubapi.StringValue(item.WorkflowStateName) != "Todo" || item.Priority == nil || *item.Priority != 1 {
				t.Fatalf("child lost approved state or priority: %+v", item)
			}
			queue, err := r.QueueEntry(ctx, f.project, item.NativeID, string(f.project))
			if err != nil {
				return err
			}
			if queue.Priority == nil || *queue.Priority != 1 {
				t.Fatalf("child queue lost approved priority: %+v", queue)
			}
			events, err := r.Events(ctx, f.project, item.NativeID, 0, 20)
			if err != nil {
				return err
			}
			position := map[string]int{"Split storage": 1, "Split API": 2, "Split UI": 3}[item.Title]
			expectedHistory := int64(1)
			for _, edge := range expectedEdges {
				if edge.Dependent == position {
					expectedHistory++
				}
			}
			if int64(len(events)) != expectedHistory || events[0].Type != "issue.created" || item.Revision != expectedHistory || item.EventSequence != expectedHistory {
				t.Fatalf("split child revision/history=%+v / %+v, want %d", item, events, expectedHistory)
			}
			if _, err := r.CollaborationVersion(ctx, string(item.NativeID), item.Revision); err != nil {
				return err
			}
		}
		comments, err = r.Comments(ctx, f.project, parent, 0, 20)
		if err != nil {
			return err
		}
		if changed {
			row, err := r.WorkItem(ctx, f.project, parent)
			if err != nil {
				return err
			}
			expectedRevision := int64(1)
			for _, edge := range expectedEdges {
				if edge.Dependent == 0 {
					expectedRevision++
				}
			}
			events, err := r.Events(ctx, f.project, parent, 0, 20)
			if err != nil {
				return err
			}
			if row.Revision != expectedRevision || row.EventSequence != expectedRevision+1 || int64(len(events)) != expectedRevision+1 || events[len(events)-1].Type != "comment.created" {
				t.Fatalf("split parent revision/history=%+v / %+v, want %d plus comment", row, events, expectedRevision)
			}
		}
		sequence, err := r.ProjectEventSequence(ctx, f.project)
		if err != nil {
			return err
		}
		want := sourceSequence
		if changed {
			want += int64(3 + len(expectedEdges) + 1)
		}
		if sequence != want {
			t.Fatalf("split history sequence=%d, want %d; partial or repeated history survived", sequence, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !changed {
		if len(children) != 0 || len(edges) != 0 || len(comments) != 0 {
			t.Fatalf("partial split survived: children=%d edges=%d comments=%d", len(children), len(edges), len(comments))
		}
		return
	}
	if len(children) != 3 || len(edges) != len(expectedEdges) || len(comments) != 1 {
		t.Fatalf("split: children=%d edges=%d comments=%d", len(children), len(edges), len(comments))
	}
	nodes := map[tracker.NativeWorkItemID]int{parent: 0}
	for _, child := range children {
		switch child.Title {
		case "Split storage":
			nodes[child.NativeID] = 1
		case "Split API":
			nodes[child.NativeID] = 2
		case "Split UI":
			nodes[child.NativeID] = 3
		}
	}
	actualEdges := make(map[chat.IssueSplitEdge]bool, len(edges))
	for _, edge := range edges {
		dependent, dependentKnown := nodes[edge.WorkItemID]
		blocker, blockerKnown := nodes[edge.RelatedWorkItemID]
		if !dependentKnown || !blockerKnown || edge.OrganizationID != organization || edge.ProjectID != f.project || edge.RelatedProjectID != f.project {
			t.Fatalf("split dependency escaped the approved scope: %+v", edge)
		}
		actualEdges[chat.IssueSplitEdge{Dependent: dependent, Blocker: blocker}] = true
	}
	if len(actualEdges) != len(expectedEdges) {
		t.Fatalf("split edges=%+v, want %+v", actualEdges, expectedEdges)
	}
	for _, edge := range expectedEdges {
		if !actualEdges[edge] {
			t.Fatalf("missing approved dependency %+v in %+v", edge, actualEdges)
		}
	}
	for _, child := range children {
		var version eventstore.CollaborationVersion
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			version, err = r.CollaborationVersion(ctx, string(child.NativeID), child.Revision)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		issue, err := f.service.hydrateWorkItemRecord(ctx, child, version)
		if err != nil {
			t.Fatal(err)
		}
		wantBodies := map[string]string{"Split storage": "Create storage", "Split API": "Use storage", "Split UI": "Build UI"}
		body, found := wantBodies[issue.Title]
		if !found || issue.Body != body+issueContractTestSections {
			t.Fatalf("split changed the approved child content: %+v", issue)
		}
	}
	var blocker, dependent corestore.WorkItem
	for _, child := range children {
		switch child.Title {
		case "Split storage":
			blocker = child
		case "Split API":
			dependent = child
		}
	}
	if blocker.NativeID == "" || dependent.NativeID == "" {
		t.Fatalf("missing approved dependency endpoints: %+v", children)
	}
	rollback := errors.New("restore split dependency fixture")
	err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		for _, terminal := range []bool{false, true} {
			if terminal {
				row, err := w.LockWorkItem(ctx, f.project, blocker.NativeID)
				if err != nil {
					return err
				}
				row.WorkflowStateName = new("Done")
				if _, err := w.UpdateWorkItem(ctx, row); err != nil {
					return err
				}
			}
			page, err := w.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: f.project, Scope: string(f.project), WorkItemID: dependent.NativeID, AvailableAt: f.service.config.Clock(), Limit: 1})
			if err != nil {
				return err
			}
			if (len(page.Items) == 1) != terminal {
				t.Fatalf("dependent dispatch=%v while blocker terminal=%v", len(page.Items) == 1, terminal)
			}
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("dependency fixture rollback=%v", err)
	}
	if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(ctx, f.project, blocker.NativeID)
		if err != nil {
			return err
		}
		if row.Revision != blocker.Revision || hubapi.StringValue(row.WorkflowStateName) != "Todo" {
			t.Fatalf("dependency assertion leaked its fixture transition: %+v", row)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func coordinatorActionIssue(ctx context.Context, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID) (tracker.NativeIssue, error) {
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(ctx, project, id)
		if err != nil {
			return err
		}
		version, err = r.CollaborationVersion(ctx, string(id), row.Revision)
		return err
	})
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	return s.hydrateCanonicalWorkItemRecord(ctx, row, version)
}

func coordinatorActionWriteIssue(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID, change func(*tracker.NativeIssue)) {
	t.Helper()
	issue, err := coordinatorActionIssue(t.Context(), s, organization, project, id)
	if err != nil {
		t.Fatal(err)
	}
	change(&issue)
	issue.Revision++
	issue.UpdatedAt = s.config.Clock()
	issue.LastActivityAt = issue.UpdatedAt
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(s.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	search, err := searchstore.NewContent(s.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: organization, ProjectID: project, WorkItemID: id, ID: "issue:" + string(id), SourceID: string(id), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	if err != nil {
		t.Fatal(err)
	}
	coordinatorFixtureWrite(t, s, organization, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), project, id)
		if err != nil {
			return err
		}
		if row.Revision+1 != int64(issue.Revision) {
			return corestore.ErrRevisionConflict
		}
		row.Title = issue.Title
		row.WorkflowStateName = &issue.State
		row.Priority = nil
		if issue.Priority != nil {
			row.Priority = new(*issue.Priority)
		}
		row.Archived = issue.Archived
		row.NativeUpdatedAt = issue.UpdatedAt
		row.UpdatedAt = issue.UpdatedAt
		row.LastActivityAt = issue.LastActivityAt
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		if err := w.PutSearch(t.Context(), search, row.Revision); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	})
}

func coordinatorActionState(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, state tracker.NativeState, fromFirst bool) {
	t.Helper()
	coordinatorFixtureProject(t, s, organization, project, func(row *projectstore.Record) {
		if i := slices.IndexFunc(row.States, func(current tracker.NativeState) bool { return current.Name == state.Name }); i < 0 {
			row.States = append(row.States, state)
		} else {
			row.States[i] = state
		}
		if fromFirst && len(row.States) > 0 && !slices.Contains(row.States[0].Transitions, state.Name) {
			row.States[0].Transitions = append(row.States[0].Transitions, state.Name)
		}
	})
}

func coordinatorActionLease(t *testing.T, s *Service, organization tracker.OrganizationID, id tracker.NativeWorkItemID, leaseID string) leasestore.Lease {
	t.Helper()
	var lease leasestore.Lease
	coordinatorFixtureWrite(t, s, organization, func(w tenantstore.Writer) error {
		item, err := w.WorkItemByNativeID(t.Context(), id)
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "archive-machine", Hostname: "archive-host", Capacity: 1, Version: "test"}); err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "archive-machine", SessionID: leaseID + "-session", LeaseID: leaseID, TTL: time.Hour})
		return err
	})
	return lease
}

func coordinatorActionRelease(t *testing.T, s *Service, organization tracker.OrganizationID, leaseID, reason string) {
	t.Helper()
	coordinatorFixtureWrite(t, s, organization, func(w tenantstore.Writer) error {
		lease, err := w.Lease(t.Context(), leaseID)
		if err != nil {
			return err
		}
		_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, reason)
		return err
	})
}

func coordinatorActionParkAttempt(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID, leaseID string, human bool) {
	t.Helper()
	lease := coordinatorActionLease(t, s, organization, id, leaseID)
	data := tracker.NativeRunData{AttemptID: "attempt_" + leaseID, RunID: "run_" + leaseID, LeaseID: tracker.LeaseID(leaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "succeeded", Disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: human}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := eventstore.NewContent(s.runDetailStore()).PreparePayload(t.Context(), organization, raw)
	if err != nil {
		t.Fatal(err)
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: organization, ProjectID: project, AggregateType: "work_item", AggregateID: id, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "archive-runner"}, RecordedAt: s.config.Clock(), Data: tracker.CollaborationData{Run: &data}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventstore.NewContent(s.runDetailStore()).PrepareEvent(t.Context(), organization, eventRaw)
	if err != nil {
		t.Fatal(err)
	}
	coordinatorFixtureWrite(t, s, organization, func(w tenantstore.Writer) error {
		_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, RunID: data.RunID, Sequence: 1, Status: "succeeded", StartedAt: s.config.Clock(), PayloadObjectKey: ref.Key, PayloadReference: ref})
		if err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, "released")
		return err
	})
}

func coordinatorActionItems(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID) []corestore.WorkItem {
	t.Helper()
	var rows []corestore.WorkItem
	if err := s.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.WorkItems(t.Context(), project, 0, 200)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 200 {
		t.Fatal("coordinator fixture exceeds item read bound")
	}
	return rows
}

func coordinatorActionEvents(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID) []tracker.CollaborationEvent {
	t.Helper()
	var rows []eventstore.Event
	if err := s.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.Events(t.Context(), project, id, 0, 200)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 200 {
		t.Fatal("coordinator fixture exceeds event read bound")
	}
	result := make([]tracker.CollaborationEvent, 0, len(rows))
	for _, row := range rows {
		value, err := eventstore.NewContent(s.runDetailStore()).Event(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	return result
}

func coordinatorActionComments(t *testing.T, s *Service, organization tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID) []tracker.NativeComment {
	t.Helper()
	var rows []corestore.Comment
	var versions []eventstore.CollaborationVersion
	if err := s.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.SelectComments(t.Context(), project, id, 0, false, 200)
		if err != nil {
			return err
		}
		for _, row := range rows {
			v, err := r.CollaborationVersion(t.Context(), row.ID, row.Revision)
			if err != nil {
				return err
			}
			versions = append(versions, v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 200 {
		t.Fatal("coordinator fixture exceeds comment read bound")
	}
	result := make([]tracker.NativeComment, 0, len(rows))
	for i, row := range rows {
		value, err := s.hydrateCommentRecord(t.Context(), row, versions[i])
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	return result
}

func coordinatorActionPool(t *testing.T, s *Service) spritePoolView {
	t.Helper()
	view, _, err := s.readSpritePoolSnapshot(t.Context(), hubscope.NativeScope{Organization: "org_security"}, false, func(tenantstore.Reader) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func coordinatorActionPoolChange(t *testing.T, s *Service, change func(*postgres.SpritePool)) {
	t.Helper()
	coordinatorFixtureWrite(t, s, "org_security", func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		row, err := owner.SpritePool(t.Context())
		if err != nil {
			return err
		}
		expected := row.Revision
		change(&row)
		_, err = owner.PutSpritePool(t.Context(), row, expected)
		return err
	})
}

func coordinatorActionSpriteSeed(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
	t.Helper()
	s := f.service
	organization := tracker.OrganizationID("org_security")
	envelope, err := s.config.SecretKeys.Seal([]byte(spritesSecretSentinel), secretAAD(string(organization), "", flySpritesToken))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := prepareChangeContent(s.contentContext(t.Context()), string(organization), []byte("private-provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	coordinatorFixtureWrite(t, s, organization, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), u.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: flySpritesToken, OrganizationSlug: "detent-test", Envelope: envelope, UpdatedAt: s.config.Clock()}, accessstore.SecretAudit{OrganizationID: organization, Actor: member.PrincipalID, Kind: flySpritesToken, Event: "stored", KeyVersion: envelope.Version, RecordedAt: s.config.Clock()}); err != nil {
			return err
		}
		if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: contentArchiveID(string(organization), ref), Reference: ref, CreatedAt: s.config.Clock()}); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		_, err = owner.PutSpritePool(t.Context(), postgres.SpritePool{OrganizationID: organization, MinRunners: 1, MaxRunners: 1, IdleSeconds: 300, BootstrapObjectKey: &ref.Key, ConfiguredBy: member.PrincipalID, IsolationTier: "native-trusted", PlacementMode: "blended"}, 0)
		return err
	})
}

type coordinatorActionFailureRepository struct {
	tenantstore.Repository
	phase    string
	target   tracker.NativeWorkItemID
	injected *atomic.Bool
}

func (r coordinatorActionFailureRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(coordinatorActionFailureWriter{Writer: w, phase: r.phase, target: r.target, injected: r.injected})
	})
}
func (r coordinatorActionFailureRepository) ReadRequest(ctx context.Context, organization tracker.OrganizationID, read func(context.Context) error) error {
	return r.Repository.(tenantstore.RequestReader).ReadRequest(ctx, organization, read)
}
func (r coordinatorActionFailureRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, organization, workload, read)
}

type coordinatorActionFailureWriter struct {
	tenantstore.Writer
	phase    string
	target   tracker.NativeWorkItemID
	injected *atomic.Bool
}

func (w coordinatorActionFailureWriter) LockUnreleasedIssueLease(ctx context.Context, issueID int64) (leasestore.Lease, error) {
	return w.Writer.(leasestore.UnreleasedIssueLeaseLocker).LockUnreleasedIssueLease(ctx, issueID)
}

func (w coordinatorActionFailureWriter) failure() error {
	w.injected.Store(true)
	return errors.New("coordinator fixture write failed")
}
func (w coordinatorActionFailureWriter) UpdateWorkItem(ctx context.Context, item corestore.WorkItem) (corestore.WorkItem, error) {
	row, err := w.Writer.UpdateWorkItem(ctx, item)
	if err == nil && (w.phase == "archive" && item.Archived && item.Title == "Seed 3" || w.phase == "transition" && item.NativeID == w.target) {
		return row, w.failure()
	}
	return row, err
}
func (w coordinatorActionFailureWriter) CreateComment(ctx context.Context, comment corestore.Comment) (corestore.Comment, error) {
	row, err := w.Writer.CreateComment(ctx, comment)
	if err == nil && w.phase == "comment" && comment.WorkItemID == w.target {
		return row, w.failure()
	}
	return row, err
}
func (w coordinatorActionFailureWriter) AppendEvent(ctx context.Context, event eventstore.Event) (eventstore.Event, error) {
	row, err := w.Writer.AppendEvent(ctx, event)
	if err == nil && w.phase == "history" && event.WorkItemID == w.target && event.Type == "issue.edited" {
		return row, w.failure()
	}
	return row, err
}
func (w coordinatorActionFailureWriter) Release(ctx context.Context, fence leasestore.Fence, reason string) (leasestore.Lease, error) {
	row, err := w.Writer.Release(ctx, fence, reason)
	if err == nil && w.phase == "release" && fence.LeaseID == "archive-lease" {
		return row, w.failure()
	}
	return row, err
}

func TestBindCoordinatorConnectionWithoutOperatorChat(t *testing.T) {
	t.Parallel()
	service := &Service{}
	ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: operatortool.Identity{PrincipalID: "user_example", OrganizationID: "org_example", CredentialID: "credential_example"}, Resolve: func(context.Context) (operatortool.Authority, error) { return operatortool.Authority{}, nil }})
	for _, test := range []struct {
		name    string
		key     string
		wantErr error
	}{
		{name: "keyed command", key: "command-key", wantErr: chat.ErrUnavailable},
		{name: "unkeyed command", key: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.bindCoordinatorConnection(ctx, test.key)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("bindCoordinatorConnection error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestBindCoordinatorConnectionPerOrganization(t *testing.T) {
	t.Parallel()
	service := &Service{}
	service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(time.Now))
	connections := map[string]string{}
	for _, organization := range []string{"org_first", "org_second"} {
		ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: operatortool.Identity{PrincipalID: "user_example", OrganizationID: organization, CredentialID: "credential_example"}, Resolve: func(context.Context) (operatortool.Authority, error) { return operatortool.Authority{}, nil }})
		bound, err := service.bindCoordinatorConnection(ctx, "command-key")
		if err != nil {
			t.Fatalf("bind %s: %v", organization, err)
		}
		connections[operatortool.CurrentConnection(bound).ID] = organization
	}
	if len(connections) != 2 {
		t.Fatalf("organizations share a coordinator connection: %v", connections)
	}
}

func waitCoordinatorIdle(t *testing.T, ctx context.Context, service *Service) {
	t.Helper()
	coordinator := service.conversations.Coordinator.(*conversationTurnCoordinator)
	testenv.WaitUntil(t, ctx, func() error {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		if coordinator.workers != 0 || len(coordinator.queue) != 0 {
			return fmt.Errorf("coordinator workers=%d queued=%d", coordinator.workers, len(coordinator.queue))
		}
		return nil
	})
}
