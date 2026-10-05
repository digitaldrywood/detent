package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkflowExecutionFailureProjection(t *testing.T) {
	for _, test := range []struct {
		name, message string
		err, safe     error
	}{
		{"graph", "Workflow transition is not allowed", &nativeError{Code: "transition_not_allowed", Message: "credential-secret", Details: map[string]any{"private": "credential-secret"}, status: http.StatusUnprocessableEntity}, operatortool.ErrInvalidArguments},
		{"revision", "Resource has changed", &nativeError{Code: "revision_conflict", Message: "credential-secret", CurrentRevision: 9, status: http.StatusConflict}, mutation.ErrConflict},
		{"foreign graph", operatortool.ErrAccessDenied.Error(), &nativeError{Code: "transition_not_allowed", Message: "credential-secret", status: http.StatusForbidden}, operatortool.ErrAccessDenied},
		{"missing", operatortool.ErrAccessDenied.Error(), nativeNotFound(), operatortool.ErrAccessDenied},
		{"revoked", operatortool.ErrAccessDenied.Error(), operatortool.ErrAccessDenied, operatortool.ErrAccessDenied},
		{"underlying", operatortool.ErrServiceUnavailable.Error(), errors.New("SQL credential-secret"), operatortool.ErrServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution, err := workflowExecutionFailure(test.err)
			if !errors.Is(err, test.safe) || execution.Message != test.message || execution.ResourceID != "" || len(execution.Data) != 0 {
				t.Fatalf("execution=%+v error=%v", execution, err)
			}
		})
	}
}

func TestMCPAuthorityExecutesDirectly(t *testing.T) {
	for _, test := range []struct {
		name, tool, target             string
		scope                          apikey.Scope
		revoked, expired, idle, denied bool
		pressure                       bool
	}{
		{name: "write Done", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite},
		{name: "write terminal creation", tool: operatortool.FileIssue, target: "Done", scope: apikey.ScopeWrite},
		{name: "write Cancelled", tool: operatortool.MoveItem, target: "Cancelled", scope: apikey.ScopeWrite},
		{name: "idle write Done", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, idle: true},
		{name: "restored connection under session pressure", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, pressure: true},
		{name: "read write denied", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeRead, denied: true},
		{name: "revoked write", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, revoked: true, denied: true},
		{name: "expired write", tool: operatortool.MoveItem, target: "Done", scope: apikey.ScopeWrite, expired: true, denied: true},
		{name: "admin policy", tool: "approve_project_policy", scope: apikey.ScopeAdmin},
		{name: "write policy denied", tool: "approve_project_policy", scope: apikey.ScopeWrite, denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, human := newHostedKeyMCPFixture(t, "dedicated", "owner")
			executor := hostedOperatorExecutor{f.service}
			issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: f.user.identity.Hosted, Email: f.user.identity.Email}, apikey.HashToken(f.user.token))
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour)
			scope := apiScopeOperator
			if test.scope == apikey.ScopeAdmin {
				scope = apiScopeAdmin
			}
			key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "client", Scope: scope, KeyScope: test.scope, Issuer: &issuer, ProjectIDs: []string{string(f.project)}, ProjectAccess: hostedProjectsSelected, ExpiresAt: &expires})
			if err != nil {
				t.Fatal(err)
			}
			ctx := changeOperatorContext(t, f.service, key.Token, "org_security")
			if err := executor.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			info, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.ConnectionInfo, Arguments: json.RawMessage(`{}`)})
			if err != nil || strings.Contains(string(info.Content), "mode") || strings.Contains(string(info.Content), "setup_url") {
				t.Fatalf("info=%s %v", info.Content, err)
			}
			states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done", "Cancelled"}}, {Name: "Done", Terminal: true}, {Name: "Cancelled", Terminal: true}}
			stateJSON, _ := json.Marshal(states)
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET states_json=? WHERE id=?", string(stateJSON), f.project)
			operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Cancelled','Cancelled',1,0,?,?)", f.project, testTimestamp, testTimestamp)
			var call operatortool.Call
			var issue tracker.NativeIssue
			var descriptor policy.Descriptor
			if test.tool == operatortool.MoveItem {
				raw, _ := json.Marshal(map[string]any{"project_id": f.project, "request_id": "create", "title": "Terminal move", "state": "Todo"})
				result, err := executor.Execute(human, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
				var created struct {
					Data tracker.NativeIssue `json:"data"`
				}
				if err != nil || json.Unmarshal(result.Content, &created) != nil || created.Data.WorkItemID == "" {
					t.Fatalf("create=%s %v", result.Content, err)
				}
				issue = created.Data
				raw, _ = json.Marshal(operatortool.MoveItemArguments{ProjectID: string(f.project), RequestID: "move", Identifier: string(issue.WorkItemID), TargetState: test.target, ExpectedRevision: int64(issue.Revision)})
				call = operatortool.Call{Name: test.tool, Arguments: raw}
			} else if test.tool == operatortool.FileIssue {
				ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"project_id": f.project, "request_id": "create-terminal", "title": "Terminal creation", "state": test.target})
				call = operatortool.Call{Name: test.tool, Arguments: raw}
			} else {
				current, err := readProjectPolicy(t.Context(), f.service.database.db, "org_security/"+string(f.project))
				if err != nil {
					var failure *nativeError
					if !errors.As(err, &failure) || failure.Code != "policy_mismatch" {
						t.Fatal(err)
					}
				}
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
					ConfigPath: "detent.yaml", HasConfig: true,
					Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: digitaldrywood/detent\ngate:\n  run: true\n  required_status_checks: []\n"),
					WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Implement the assigned issue.\n", 100)),
					AgentsPath: "AGENTS.md", HasAgents: true, Agents: []byte(strings.Repeat("Preserve policy authority.\n", 100)),
				})
				if err != nil {
					t.Fatal(err)
				}
				workflow.SharedPrompt = strings.Repeat("Review the material policy.\n", 100)
				descriptor, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				call = projectCall(t, test.tool, string(f.project), "policy", operatortool.PolicyApprovalInput{ExpectedID: current.Policy.ID, Policy: descriptor})
			}
			if test.revoked {
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
			}
			if test.expired {
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(time.Now().Add(-time.Hour)), key.ID)
			}
			if test.pressure {
				connection := operatortool.CurrentConnection(ctx)
				storedAt := f.service.config.now().Add(-5 * time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z")
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE operator_chat_sessions SET last_used_at=? WHERE connection_id=?", storedAt, connection.ID)
				for index := 0; index < 256; index++ {
					if _, err := f.service.operatorChat.Send(t.Context(), fmt.Sprintf("pressure-%d", index), "hello"); !errors.Is(err, chat.ErrUnavailable) {
						t.Fatal(err)
					}
				}
				if err := f.service.operatorChat.RestoreConnection(t.Context(), connection.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.operatorChat.Send(t.Context(), "next-session", "hello"); !errors.Is(err, chat.ErrUnavailable) {
					t.Fatal(err)
				}
				conversation := f.service.operatorChat.Conversation(connection.ID)
				if conversation.ConnectionID != connection.ID || conversation.PrincipalID != connection.Identity.PrincipalID || conversation.OrganizationID != connection.Identity.OrganizationID {
					t.Fatalf("restored connection lost: %+v", conversation)
				}
				ctx, err = f.service.operatorChat.OriginatingContext(t.Context(), connection.ID, connection.Identity.PrincipalID, connection.Identity.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				var after string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_used_at FROM operator_chat_sessions WHERE connection_id=?", connection.ID).Scan(&after); err != nil || after != storedAt {
					t.Fatalf("read extended lifetime: before=%s after=%s error=%v", storedAt, after, err)
				}
			}
			if test.idle {
				f.service.operatorChat = chat.NewService(nil, nil, executor, chat.WithClock(func() time.Time { return time.Now().Add(25 * time.Hour) }))
			}
			result, err := executor.Execute(ctx, call)
			if test.denied {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("denied=%s %v", result.Content, err)
				}
			} else {
				if err != nil || strings.Contains(string(result.Content), "approval_url") || strings.Contains(string(result.Content), `"status":"pending"`) {
					t.Fatalf("direct=%s %v", result.Content, err)
				}
				if test.tool == operatortool.MoveItem || test.tool == operatortool.FileIssue {
					var receipt struct {
						Data tracker.NativeIssue `json:"data"`
					}
					if json.Unmarshal(result.Content, &receipt) != nil || receipt.Data.State != test.target || receipt.Data.Revision != issue.Revision+1 {
						t.Fatalf("transition=%s", result.Content)
					}
				} else {
					approval, err := readProjectPolicy(t.Context(), f.service.database.db, "org_security/"+string(f.project))
					if err != nil || !reflect.DeepEqual(approval.Policy, descriptor) || approval.ApprovedBy != key.ID {
						t.Fatalf("policy=%+v %v", approval, err)
					}
				}
				if _, err := executor.Execute(ctx, call); err != nil {
					t.Fatalf("replay=%v", err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(time.Now()), key.ID)
				if _, err := executor.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("revoked replay=%v", err)
				}
			}
			for _, action := range f.service.operatorChat.Conversation(operatortool.CurrentConnection(ctx).ID).Actions {
				if action.Status == chat.ActionPending {
					t.Fatal("pending action retained")
				}
			}
			requireNativeStatus(t, f.browser(http.MethodGet, "/chat/approval?connection_id=old", nil), http.StatusNotFound)
		})
	}
}
