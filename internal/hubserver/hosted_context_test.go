package hubserver

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type hostedContextReply struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func hostedContextProtocol(t *testing.T, service *Service, ctx context.Context, transport string) func(string, string, any) hostedContextReply {
	t.Helper()
	var exchange func([]byte) []byte
	executor := hostedOperatorExecutor{service}
	if transport == "stdio" {
		input, inputWriter := io.Pipe()
		outputReader, output := io.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- mcp.NewServer(executor, "test", mcp.WithLogger(service.config.Logger)).Serve(context.WithoutCancel(ctx), input, output)
		}()
		t.Cleanup(func() {
			if err := inputWriter.Close(); err != nil {
				t.Error(err)
			}
			if err := <-done; err != nil {
				t.Error(err)
			}
			if err := output.Close(); err != nil {
				t.Error(err)
			}
			if err := outputReader.Close(); err != nil {
				t.Error(err)
			}
		})
		reader := bufio.NewReader(outputReader)
		exchange = func(frame []byte) []byte {
			if _, err := inputWriter.Write(append(frame, '\n')); err != nil {
				t.Fatal(err)
			}
			raw, err := reader.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
	} else {
		handler := mcp.NewHTTPHandler(executor, "test", mcp.HTTPConfig{Logger: service.config.Logger, Principal: func(r *http.Request) operatortool.Identity { return operatortool.ConnectionIdentity(r.Context()) }})
		t.Cleanup(func() {
			if err := handler.Shutdown(context.WithoutCancel(ctx)); err != nil {
				t.Error(err)
			}
		})
		exchange = func(frame []byte) []byte {
			var message struct {
				Method string `json:"method"`
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if err := json.Unmarshal(frame, &message); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "http://detent.example/mcp", strings.NewReader(string(frame))).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Mcp-Protocol-Version", "2026-07-28")
			r.Header.Set("Mcp-Method", message.Method)
			if message.Params.Name != "" {
				r.Header.Set("Mcp-Name", message.Params.Name)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			return response.Body.Bytes()
		}
	}
	return func(method, name string, args any) hostedContextReply {
		params := map[string]any{"_meta": json.RawMessage(fleetProtocolMeta)}
		if method == "tools/call" {
			params["name"], params["arguments"] = name, args
		} else if args != nil {
			params["cursor"] = args
		}
		frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		raw := exchange(frame)
		for _, secret := range []string{"csrf_token", "form_token", "session_owner", "refresh_token", "token_hash", "credential-sensitive-value-sentinel", "private-worktree-sentinel"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("protocol leaked %s", secret)
			}
		}
		var reply hostedContextReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("protocol response: %s: %v", raw, err)
		}
		return reply
	}
}

func hostedContextData(t *testing.T, reply hostedContextReply, denied bool) json.RawMessage {
	t.Helper()
	var result struct {
		IsError bool            `json:"isError"`
		Data    json.RawMessage `json:"structuredContent"`
	}
	if len(reply.Result) > 0 {
		if err := json.Unmarshal(reply.Result, &result); err != nil {
			t.Fatal(err)
		}
	}
	failed := result.IsError || len(reply.Error) > 0
	if failed != denied || denied && len(result.Data) != 0 || !denied && len(result.Data) == 0 {
		t.Fatalf("denied=%t response=%s %s", denied, reply.Result, reply.Error)
	}
	return result.Data
}

func seedHostedContextRunner(t *testing.T, f hostedSecurityFixture, id, version string, heartbeat time.Time, revoked bool) {
	t.Helper()
	now := formatHubTime(f.service.config.now())
	token := "tok_" + id
	operatorSQL(t, f, `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at,native_only) VALUES(?,?,?,?,'worker',?,?,1)`, token, token, sha256Hex(token), id, now, now)
	operatorSQL(t, f, `INSERT INTO machines(id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id,token_id) VALUES(?,?,?,1,?,?,?,?,'org_security',?)`, "machine_"+id, id, id, version, formatHubTime(heartbeat), now, now, token)
	operatorSQL(t, f, `INSERT INTO runner_enrollments(id,organization_id,runner_id,machine_id,token_hash,operations_json,created_at,expires_at,created_by,redeemed_at) VALUES(?,'org_security',?,?,?,'["claim"]',?,?,?,?)`, "enr_"+id, id, "machine_"+id, sha256Hex("enr_"+id), now, formatHubTime(f.service.config.now().Add(time.Hour)), token, now)
	operatorSQL(t, f, `INSERT INTO runner_identities(id,organization_id,machine_id,token_id,enrollment_id,operations_json,created_at,display_name,capacity_limit,reported_capacity,os,architecture,last_heartbeat_at) VALUES(?,'org_security',?,?,?,'["claim"]',?,?,1,1,'linux','arm64',?)`, id, "machine_"+id, token, "enr_"+id, now, id, formatHubTime(heartbeat))
	if revoked {
		operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", now, token)
	}
}

func TestHostedContextMCP(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, transport := range []string{"stdio", "http"} {
			for _, scenario := range []string{"content", "move revision contract", "disabled services", "viewer", "runner grant removed", "project grant removed", "role downgraded", "membership removed", "session revoked", "foreign identifiers", "unavailable read", "oversized read", "project bound", "runner bound", "workspace observation"} {
				t.Run(deployment+"/"+transport+"/"+scenario, func(t *testing.T) {
					role := "member"
					if scenario == "viewer" {
						role = "viewer"
					}
					f, mcpCtx := newHostedKeyMCPFixture(t, deployment, role)
					f.grant(t, f.user, true, true)
					authority, err := operatorCatalog(mcpCtx)
					if err != nil {
						t.Fatal(err)
					}
					mcpCtx = f.service.withOperatorCatalog(mcpCtx, authority.credential, authority.identity.OrganizationID)
					f.service.config.Version = "v1.2.4"
					f.service.clientBuild = appClientBuild{Build: "client-build-sentinel"}
					send := hostedContextProtocol(t, f.service, mcpCtx, transport)
					call := func(name string, args any, denied bool) json.RawMessage {
						return hostedContextData(t, send("tools/call", name, args), denied)
					}
					project := string(f.project)
					if scenario == "content" {
						f.service.conversations = &conversationService{config: ConversationConfig{Model: "model-sentinel", ReasoningEffort: "medium"}, coordinator: &recordingWaker{}}
						t.Cleanup(func() { f.service.conversations = nil })
						var bootstrap appBootstrap
						if err := json.Unmarshal(call(operatortool.AppBootstrapPayload, map[string]any{}, false), &bootstrap); err != nil {
							t.Fatal(err)
						}
						var browser appBootstrap
						response := f.browser(http.MethodGet, "/app/bootstrap", nil)
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &browser)
						browser.CSRFToken = ""
						if !reflect.DeepEqual(bootstrap, browser) || bootstrap.Actor.Email == "" || !bootstrap.Feature.Conversation || !bootstrap.Capabilities.Coordinator || len(bootstrap.Preferences.Models) != 2 || bootstrap.Preferences.Models[1].ID != "model-sentinel" || len(bootstrap.Preferences.Efforts) != 4 || len(bootstrap.Preferences.Access) != 3 || bootstrap.Plan == nil || bootstrap.Version != "v1.2.4" {
							t.Fatalf("bootstrap=%+v browser=%+v", bootstrap, browser)
						}
						for _, runner := range []struct {
							id, version     string
							online, revoked bool
						}{{"a-old", "v1.2.3", true, false}, {"b-offline", "v1.2.4", false, false}, {"c-current", "v1.2.4", true, false}, {"d-revoked", "v1.0.0", true, true}} {
							at := f.service.config.now()
							if !runner.online {
								at = at.Add(-time.Hour)
							}
							seedHostedContextRunner(t, f.hostedSecurityFixture, runner.id, runner.version, at, runner.revoked)
						}
						var updates appUpdates
						if err := json.Unmarshal(call(operatortool.AppUpdates, map[string]any{}, false), &updates); err != nil {
							t.Fatal(err)
						}
						var dashboard appUpdates
						decodeHubResponse(t, f.browser(http.MethodGet, "/app/updates", nil), &dashboard)
						if !reflect.DeepEqual(updates, dashboard) || updates.Current != "v1.2.4" || updates.Source != "hub" || updates.Client != f.service.clientBuild || updates.MinimumRunnerVersion != "v1.2.4" || updates.BehindCount != 1 || len(updates.Runners) != 3 || !updates.Runners[0].Online || !updates.Runners[0].Behind || updates.Runners[0].ClaimRefusalReason == "" || updates.Runners[1].Online || updates.Runners[2].Behind {
							t.Fatalf("updates=%+v dashboard=%+v", updates, dashboard)
						}
					}
					listed := send("tools/list", "", nil)
					var catalog struct {
						Tools []operatortool.Definition `json:"tools"`
					}
					if len(listed.Error) > 0 || json.Unmarshal(listed.Result, &catalog) != nil {
						t.Fatalf("list=%s %s", listed.Result, listed.Error)
					}
					for _, name := range []string{operatortool.AppBootstrapPayload, operatortool.AppUpdates, operatortool.HostedEvents} {
						found := slices.ContainsFunc(catalog.Tools, func(d operatortool.Definition) bool { return d.Name == name })
						if found != (name != operatortool.AppUpdates || scenario != "viewer") {
							t.Fatalf("discovery %s=%t", name, found)
						}
					}
					if scenario == "move revision contract" {
						var schema struct {
							Properties map[string]struct {
								Type string `json:"type"`
							} `json:"properties"`
						}
						for _, definition := range catalog.Tools {
							if definition.Name == operatortool.MoveItem {
								if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
									t.Fatal(err)
								}
							}
						}
						if schema.Properties["expected_revision"].Type != "string" {
							t.Fatalf("advertised move revision type=%q, want string", schema.Properties["expected_revision"].Type)
						}
						states := []tracker.NativeState{{Name: "Backlog", Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true}}
						stateJSON, err := json.Marshal(states)
						if err != nil {
							t.Fatal(err)
						}
						operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET states_json=? WHERE id=?", string(stateJSON), project)
						operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Backlog','Backlog',0,0,?,?)", project, testTimestamp, testTimestamp)
						var created struct {
							Data struct {
								WorkItemID string `json:"work_item_id"`
								Revision   string `json:"revision"`
							} `json:"data"`
						}
						if err := json.Unmarshal(call(operatortool.FileIssue, map[string]any{"project_id": project, "request_id": "create", "title": "Revision contract", "state": "Backlog"}, false), &created); err != nil || created.Data.WorkItemID == "" || created.Data.Revision != "1" {
							t.Fatalf("created=%+v err=%v", created, err)
						}
						arguments := map[string]any{"project_id": project, "request_id": "move", "identifier": created.Data.WorkItemID, "target_state": "Todo", "expected_revision": created.Data.Revision}
						var moved struct {
							Status string              `json:"status"`
							Data   tracker.NativeIssue `json:"data"`
						}
						if err := json.Unmarshal(call(operatortool.MoveItem, arguments, false), &moved); err != nil || moved.Status != "succeeded" || string(moved.Data.WorkItemID) != created.Data.WorkItemID || moved.Data.State != "Todo" || moved.Data.Revision != 2 {
							t.Fatalf("moved=%+v err=%v", moved, err)
						}
						var observed struct {
							Data tracker.NativeIssue `json:"data"`
						}
						if err := json.Unmarshal(call(operatortool.WorkItem, map[string]any{"project_id": project, "reference": created.Data.WorkItemID}, false), &observed); err != nil || observed.Data.State != "Todo" || observed.Data.Revision != 2 {
							t.Fatalf("persisted move=%+v err=%v", observed, err)
						}
						return
					}
					if scenario == "disabled services" {
						var bootstrap appBootstrap
						if err := json.Unmarshal(call(operatortool.AppBootstrapPayload, map[string]any{}, false), &bootstrap); err != nil {
							t.Fatal(err)
						}
						if bootstrap.Feature.Conversation || bootstrap.Feature.Workspaces || bootstrap.Capabilities.Coordinator || len(bootstrap.Preferences.Models) != 0 {
							t.Fatalf("disabled=%+v", bootstrap)
						}
						call(operatortool.HostedEvents, map[string]any{"project_id": project, "workspace_id": "wsp_missing"}, true)
						return
					}
					if scenario == "viewer" {
						call(operatortool.AppUpdates, map[string]any{}, true)
						return
					}
					if scenario == "runner grant removed" || scenario == "role downgraded" {
						if scenario == "runner grant removed" {
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_project_grants SET manage_runner=0 WHERE user_id=?", f.user.identity.Subject)
						} else {
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", f.user.identity.Subject)
						}
						call(operatortool.AppUpdates, map[string]any{}, true)
						var bootstrap appBootstrap
						if err := json.Unmarshal(call(operatortool.AppBootstrapPayload, map[string]any{}, false), &bootstrap); err != nil {
							t.Fatal(err)
						}
						if bootstrap.Actor.CanManageRunners || bootstrap.Projects[0].CanManageRunners {
							t.Fatalf("stale authority=%+v", bootstrap)
						}
						return
					}
					if scenario == "project grant removed" || scenario == "membership removed" || scenario == "session revoked" {
						switch scenario {
						case "project grant removed":
							operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
						case "session revoked":
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_sessions SET revoked_at=?", formatHubTime(f.service.config.now()))
						default:
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET active=0 WHERE user_id=?", f.user.identity.Subject)
						}
						if reply := send("tools/list", "", "invalid-partial-discovery"); len(reply.Error) == 0 {
							t.Fatalf("invalid discovery succeeded: %s", reply.Result)
						}
						call(operatortool.HostedEvents, map[string]any{"project_id": project}, true)
						call(operatortool.AppUpdates, map[string]any{}, true)
						if scenario != "project grant removed" {
							call(operatortool.AppBootstrapPayload, map[string]any{}, true)
						} else {
							var bootstrap appBootstrap
							if err := json.Unmarshal(call(operatortool.AppBootstrapPayload, map[string]any{}, false), &bootstrap); err != nil {
								t.Fatal(err)
							}
							if len(bootstrap.Projects) != 0 {
								t.Fatalf("revoked projects=%+v", bootstrap.Projects)
							}
						}
						return
					}
					if scenario == "foreign identifiers" {
						call(operatortool.HostedEvents, map[string]any{"project_id": "foreign-project"}, true)
						call(operatortool.AppBootstrapPayload, map[string]any{"organization_id": "foreign-org"}, true)
						call(operatortool.AppUpdates, map[string]any{"project_id": project}, true)
						call(operatortool.HostedEvents, map[string]any{"project_id": strings.Repeat("x", 257)}, true)
						call(operatortool.HostedEvents, map[string]any{"project_id": project, "cursor": strings.Repeat("x", 2049)}, true)
						call(operatortool.AppBootstrapPayload, map[string]any{"unsupported": strings.Repeat("x", operatortool.MaxArgumentBytes)}, true)
						return
					}
					if scenario == "unavailable read" {
						operatorSQL(t, f.hostedSecurityFixture, "DROP TABLE collaboration_events")
						call(operatortool.HostedEvents, map[string]any{"project_id": project}, true)
						return
					}
					if scenario == "oversized read" {
						operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET name=? WHERE id=?", strings.Repeat("x", operatortool.MaxResultBytes), project)
						call(operatortool.AppBootstrapPayload, map[string]any{}, true)
						return
					}
					if scenario == "runner bound" {
						for i := 0; i <= operatortool.MaxItemLimit; i++ {
							seedHostedContextRunner(t, f.hostedSecurityFixture, newNativeID("runner"), "v1.2.4", f.service.config.now(), false)
						}
						call(operatortool.AppUpdates, map[string]any{}, true)
						return
					}
					if scenario == "project bound" {
						for range operatortool.MaxItemLimit {
							id := newNativeID("prj")
							operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO projects(id,organization_id,name,profile,created_at) VALUES(?,'org_security',?,'native',?)", id, id, testTimestamp)
							operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id) VALUES(?,'org_security',?) ON CONFLICT(user_id,project_id) DO UPDATE SET can_write=excluded.can_write,manage_runner=excluded.manage_runner", f.user.identity.Subject, id)
						}
						call(operatortool.AppBootstrapPayload, map[string]any{}, true)
						call(operatortool.AppUpdates, map[string]any{}, true)
						return
					}
					args := map[string]any{"project_id": project}
					if scenario == "workspace observation" {
						item := f.seedIssue(t, 1)
						workspace := workspacesession.NewID()
						stamp := formatHubTime(f.service.config.now())
						operatorSQL(t, f.hostedSecurityFixture, `INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,ref,state,worktree,worktree_path,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) VALUES(?,'org_security',?,?,'develop','ready','retained','private-worktree-sentinel',300,?,?,?,?,?)`, workspace, project, item, stamp, stamp, f.user.identity.Subject, stamp, stamp)
						f.service.workspaces = &workspaceService{server: f.service, logger: f.service.config.Logger}
						t.Cleanup(func() { f.service.workspaces = nil })
						args["workspace_id"] = workspace
					}
					var first hostedEventResult
					if err := json.Unmarshal(call(operatortool.HostedEvents, args, false), &first); err != nil {
						t.Fatal(err)
					}
					if !first.Changed || first.Sequence != 0 || first.Cursor == "" || first.ObservedAt == "" || first.Freshness != "current_observation" || first.Semantics == "" {
						t.Fatalf("initial=%+v", first)
					}
					if scenario == "workspace observation" && (first.Workspace == nil || first.Workspace.Revision != 1 || first.Workspace.Worktree != "" || first.Workspace.WorktreePath != "" || len(first.Workspace.RelaySessions) > 0) {
						t.Fatalf("unsafe workspace=%+v", first.Workspace)
					}
					args["cursor"] = first.Cursor
					var unchanged hostedEventResult
					if err := json.Unmarshal(call(operatortool.HostedEvents, args, false), &unchanged); err != nil {
						t.Fatal(err)
					}
					if unchanged.Changed || unchanged.Cursor != first.Cursor {
						t.Fatalf("heartbeat changed=%+v", unchanged)
					}
					if scenario == "workspace observation" {
						operatorSQL(t, f.hostedSecurityFixture, "UPDATE workspace_sessions SET revision=revision+1,state='idle'")
						var workspaceChange hostedEventResult
						if err := json.Unmarshal(call(operatortool.HostedEvents, args, false), &workspaceChange); err != nil {
							t.Fatal(err)
						}
						if !workspaceChange.Changed || workspaceChange.Sequence != first.Sequence || workspaceChange.Workspace.Revision != 2 || workspaceChange.Workspace.State != "idle" {
							t.Fatalf("workspace change=%+v", workspaceChange)
						}
						args["workspace_id"] = workspacesession.NewID()
						call(operatortool.HostedEvents, args, true)
						return
					}
					credential, err := (hubAdministration{f.service}).credential(mcpCtx)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.createNativeIssueCommand(t.Context(), nativeScope{organization: "org_security", project: f.project, credential: credential}, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "event-change"}, Title: "event change", State: "Todo"}); err != nil {
						t.Fatal(err)
					}
					var changed hostedEventResult
					if err := json.Unmarshal(call(operatortool.HostedEvents, args, false), &changed); err != nil {
						t.Fatal(err)
					}
					if !changed.Changed || changed.Sequence <= first.Sequence || changed.Cursor == first.Cursor {
						t.Fatalf("change=%+v", changed)
					}
					for _, scope := range []string{"organization", "project", "workspace"} {
						position := hostedEventCursor{Organization: "org_security", Project: project, Sequence: first.Sequence}
						switch scope {
						case "organization":
							position.Organization = "foreign"
						case "project":
							position.Project = "foreign"
						case "workspace":
							position.Workspace = "foreign"
						}
						raw, err := json.Marshal(position)
						if err != nil {
							t.Fatal(err)
						}
						args["cursor"] = base64.RawURLEncoding.EncodeToString(raw)
						call(operatortool.HostedEvents, args, true)
					}
					args["cursor"] = "invalid"
					call(operatortool.HostedEvents, args, true)
					args["cursor"] = first.Cursor
					operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", f.user.identity.Subject)
					call(operatortool.HostedEvents, args, true)
				})
			}
		}
	}
}

func TestHostedContextKeyProjection(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(deployment+"/"+transport, func(t *testing.T) {
				f, mcpCtx := newHostedKeyMCPFixture(t, deployment, "owner")
				f.grant(t, f.user, true, true)
				operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO projects(id,organization_id,name,profile,created_at) VALUES('prj_private','org_security','hidden-project-sentinel','native',?)", testTimestamp)
				operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner) VALUES(?,'org_security','prj_private',1,1) ON CONFLICT(user_id,project_id) DO UPDATE SET can_write=excluded.can_write,manage_runner=excluded.manage_runner", f.user.identity.Subject)
				operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO token_grants(token_id,organization_id,project_id) SELECT principal_id,'org_security','prj_private' FROM hosted_members WHERE user_id=? ON CONFLICT DO NOTHING", f.user.identity.Subject)
				credential, err := (hubAdministration{f.service}).credential(mcpCtx)
				if err != nil {
					t.Fatal(err)
				}
				key, err := f.service.createHostedAPIKeyFor(mcpCtx, credential, hostedKeyRequest{Name: "context-key", Scope: apikey.ScopeRead, Days: 30, ProjectAccess: hostedProjectsSelected, Projects: []string{string(f.project)}})
				if err != nil {
					t.Fatal(err)
				}
				seedHostedContextRunner(t, f.hostedSecurityFixture, "private-catalog", "v1.2.4", f.service.config.now(), false)
				operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES('tok_private-catalog','org_security','prj_private')")
				now, err := f.service.database.currentTime()
				if err != nil {
					t.Fatal(err)
				}
				reports, err := json.Marshal([]providercapacity.Report{{Provider: "openai", Backend: "codex", AccountAlias: "private", Models: []string{"hidden-model-sentinel"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}})
				if err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE runner_identities SET provider_reports_json=? WHERE id='private-catalog'", string(reports))
				f.service.conversations = &conversationService{config: ConversationConfig{}}
				t.Cleanup(func() { f.service.conversations = nil })
				ctx := workspaceOperatorContext(t, f.service, key.Token, "org_security", "context-key")
				send := hostedContextProtocol(t, f.service, ctx, transport)
				raw := hostedContextData(t, send("tools/call", operatortool.AppBootstrapPayload, map[string]any{}), false)
				if strings.Contains(string(raw), "hidden-project-sentinel") || strings.Contains(string(raw), "hidden-model-sentinel") || strings.Contains(string(raw), key.Token) {
					t.Fatalf("key leaked context: %s", raw)
				}
				var bootstrap appBootstrap
				if err := json.Unmarshal(raw, &bootstrap); err != nil {
					t.Fatal(err)
				}
				if len(bootstrap.Projects) != 1 || bootstrap.Actor.Email == "" || bootstrap.Actor.PrincipalID != credential.ID || bootstrap.Actor.CanManageRunners || len(bootstrap.Organizations) != 0 || len(bootstrap.Preferences.Models) != 1 {
					t.Fatalf("key projection=%+v", bootstrap)
				}
				hostedContextData(t, send("tools/call", operatortool.AppUpdates, map[string]any{}), true)
				hostedContextData(t, send("tools/call", operatortool.HostedEvents, map[string]any{"project_id": "prj_private"}), true)
				hostedContextData(t, send("tools/call", operatortool.HostedEvents, map[string]any{"project_id": string(f.project)}), false)
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at=? WHERE id=?", testTimestamp, key.ID)
				hostedContextData(t, send("tools/call", operatortool.AppBootstrapPayload, map[string]any{}), true)
			})
		}
	}
}

func TestHostedContextUnavailable(t *testing.T) {
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			ctx := workspaceOperatorContext(t, f.service, testHubAdminToken, string(f.project.OrganizationID), "hosted-unavailable")
			credential, _, err := f.service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
			if err != nil {
				t.Fatal(err)
			}
			ctx = f.service.withOperatorCatalog(ctx, credential, string(f.project.OrganizationID))
			ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead})
			if err != nil {
				t.Fatal(err)
			}
			send := hostedContextProtocol(t, f.service, ctx, transport)
			listed := send("tools/list", "", nil)
			var catalog struct {
				Tools []operatortool.Definition `json:"tools"`
			}
			if len(listed.Error) > 0 || json.Unmarshal(listed.Result, &catalog) != nil {
				t.Fatalf("list=%s %s", listed.Result, listed.Error)
			}
			for _, d := range catalog.Tools {
				if operatortool.IsHostedContext(d.Name) {
					t.Fatalf("unavailable tool advertised: %s", d.Name)
				}
			}
			for _, name := range []string{operatortool.AppBootstrapPayload, operatortool.AppUpdates, operatortool.HostedEvents} {
				args := map[string]any{}
				if name == operatortool.HostedEvents {
					args["project_id"] = string(f.project.ID)
				}
				hostedContextData(t, send("tools/call", name, args), true)
			}
		})
	}
}
