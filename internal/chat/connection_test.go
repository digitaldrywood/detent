package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

// These cases catch model self-approval, mutable previews, replay, stale
// authority, and treating YOLO as a permission rather than confirmation mode.
func TestConnectionActions(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Service, *actionExecutorStub, context.Context, context.Context, *bool)
	}{
		{"YOLO survives session expiry", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			if err := s.SetConnectionMode(human, "connection", YOLOMode); err != nil {
				t.Fatal(err)
			}
			now := s.now().Add(25 * time.Hour)
			s.now = func() time.Time { return now }
			s.Conversation("prune")
			if err := s.AttachConnection(ctx); err != nil {
				t.Fatal(err)
			}
			result, err := s.Submit(ctx, connectionTestAction())
			if err != nil || result.Status != ActionSucceeded || result.Mode != YOLOMode || executor.calls != 1 {
				t.Fatalf("expired connection lost choice: action=%+v error=%v calls=%d", result, err, executor.calls)
			}
		}},
		{"operator confirmation choice survives session eviction", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			if err := s.SetConnectionMode(human, "connection", YOLOMode); err != nil {
				t.Fatal(err)
			}
			if err := s.SetConnectionMode(human, "connection", ConfirmationMode); err != nil {
				t.Fatal(err)
			}
			s.sessionLimit = 1
			s.Conversation("evict")
			if err := s.AttachConnection(ctx); err != nil {
				t.Fatal(err)
			}
			result, err := s.Submit(ctx, connectionTestAction())
			if err != nil || result.Status != ActionPending || result.Mode != ConfirmationMode || executor.calls != 0 {
				t.Fatalf("evicted connection ignored choice: action=%+v error=%v calls=%d", result, err, executor.calls)
			}
		}},
		{"another principal cannot select YOLO", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			identity := operatortool.ConnectionIdentity(ctx)
			identity.PrincipalID = "other-operator"
			if err := s.SetConnectionMode(WithOperatorApproval(ctx, identity), "connection", YOLOMode); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("cross-principal YOLO=%v", err)
			}
		}},

		{"ordinary organization context selection", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action := connectionTestAction()
			action.Kind = ActionKind(operatortool.OrganizationSwitch)
			result, err := s.Submit(ctx, action)
			if err != nil || result.Status != ActionSucceeded || executor.calls != 1 {
				t.Fatalf("context selection=%+v %v", result, err)
			}
		}},
		{"ordinary write", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action := connectionTestAction()
			action.Kind = ActionSetPriority
			result, err := s.Submit(ctx, action)
			if err != nil || result.Status != ActionSucceeded || executor.calls != 1 {
				t.Fatalf("ordinary write = %+v, %v; calls=%d", result, err, executor.calls)
			}
		}},
		{"model self approval and forged ID", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Confirm(ctx, "connection", action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("model confirm = %v", err)
			}
			if _, err := s.Confirm(human, "connection", "forged"); !errors.Is(err, ErrActionNotFound) {
				t.Fatalf("forged confirm = %v", err)
			}
			if err := s.SetConnectionMode(ctx, "connection", YOLOMode); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("model YOLO = %v", err)
			}
			if _, err := s.Reject(ctx, "connection", action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("model reject = %v", err)
			}
			if executor.calls != 0 {
				t.Fatal("model mutated state")
			}
		}},
		{"creation receipt retains resource identity on retry", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			proposal := connectionTestAction()
			proposal.Kind = ActionFileIssue
			executor.execution = &ActionExecution{Message: "Created", ResourceID: "new-id", Identifier: "new-identifier", URL: "https://dashboard.example/item/new-id"}
			result, err := s.Submit(ctx, proposal)
			if err != nil || result.IssueID != "new-id" || result.Identifier != "new-identifier" || result.ResourceURL != executor.execution.URL {
				t.Fatalf("creation identity=%+v %v", result, err)
			}
			retry, err := s.Submit(ctx, proposal)
			if err != nil || retry.ResourceURL != result.ResourceURL || retry.IssueID != result.IssueID || executor.calls != 1 {
				t.Fatalf("creation retry=%+v %v; calls=%d", retry, err, executor.calls)
			}
		}},
		{"missing executor cannot allow model receipt changes", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			s.actions = nil
			if _, err := s.Confirm(ctx, "connection", action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("model confirmation=%v", err)
			}
			stored, _ := s.Action("connection", action.ID)
			if stored.Status != ActionPending {
				t.Fatalf("unauthorized receipt change=%+v", stored)
			}
		}},
		{"exact preview and approval replay", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			proposal := connectionTestAction()
			action, err := s.Submit(ctx, proposal)
			if err != nil {
				t.Fatal(err)
			}
			proposal.Labels[0] = "changed"
			proposal.Arguments[2] = 'X'
			action.Labels[0] = "changed return"
			if _, err := s.Confirm(human, "connection", action.ID); err != nil {
				t.Fatal(err)
			}
			if executor.action.Labels[0] != "original" || string(executor.action.Arguments) != `{"destination":"Cancelled"}` {
				t.Fatalf("mutated preview: %+v", executor.action)
			}
			if _, err := s.Confirm(human, "connection", action.ID); !errors.Is(err, ErrActionNotPending) {
				t.Fatalf("replay = %v", err)
			}
			if executor.calls != 1 {
				t.Fatalf("calls=%d", executor.calls)
			}
		}},
		{"changed retry arguments", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			changed := connectionTestAction()
			changed.Arguments = json.RawMessage(`{"destination":"Todo"}`)
			if _, err := s.Submit(ctx, changed); !errors.Is(err, operatortool.ErrInvalidArguments) {
				t.Fatalf("changed retry=%v", err)
			}
			retry, err := s.Submit(ctx, connectionTestAction())
			if err != nil || retry.ID != action.ID || executor.calls != 0 {
				t.Fatalf("retry=%+v %v", retry, err)
			}
		}},
		{"rejection", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.RejectConnectionAction(human, "connection", action.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Confirm(human, "connection", action.ID); !errors.Is(err, ErrActionNotPending) {
				t.Fatalf("confirm rejected=%v", err)
			}
			if executor.calls != 0 {
				t.Fatal("rejection mutated state")
			}
		}},
		{"revoked before approval", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			*revoked = true
			if _, err := s.Confirm(human, "connection", action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("revoked confirm=%v", err)
			}
			if executor.calls != 0 {
				t.Fatal("revoked authority executed")
			}
		}},
		{"YOLO still reauthorizes", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			if err := s.AttachConnection(ctx); err != nil {
				t.Fatal(err)
			}
			if err := s.SetConnectionMode(human, "connection", YOLOMode); err != nil {
				t.Fatal(err)
			}
			result, err := s.Submit(ctx, connectionTestAction())
			if err != nil || result.Status != ActionSucceeded || executor.calls != 1 || executor.action.Mode != YOLOMode {
				t.Fatalf("YOLO=%+v %v", result, err)
			}
			denied := connectionTestAction()
			denied.RequestID = "other"
			denied.ProjectID = "other"
			if _, err := s.Submit(ctx, denied); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("project bypass=%v", err)
			}
			*revoked = true
			denied.ProjectID = "project"
			if _, err := s.Submit(ctx, denied); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("revoked YOLO=%v", err)
			}
			if executor.calls != 1 {
				t.Fatal("YOLO expanded authority")
			}
		}},
		{"approved receipt consumes current connection mode", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetConnectionMode(human, "connection", YOLOMode); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Confirm(human, "connection", action.ID); err != nil {
				t.Fatal(err)
			}
			if executor.action.Mode != YOLOMode {
				t.Fatalf("execution mode=%s", executor.action.Mode)
			}
		}},
		{"cross organization approver", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			other := WithOperatorApproval(ctx, operatortool.Identity{PrincipalID: "other", OrganizationID: "other", CredentialID: "other"})
			if _, err := s.Confirm(other, "connection", action.ID); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("cross-org=%v", err)
			}
			if executor.calls != 0 {
				t.Fatal("cross organization mutation")
			}
		}},
		{"expired pending session", func(t *testing.T, s *Service, executor *actionExecutorStub, ctx, human context.Context, revoked *bool) {
			action, err := s.Submit(ctx, connectionTestAction())
			if err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return action.CreatedAt.Add(25 * time.Hour) }
			if _, err := s.Confirm(human, "connection", action.ID); !errors.Is(err, ErrActionNotFound) {
				t.Fatalf("expired preview=%v", err)
			}
			if executor.calls != 0 {
				t.Fatal("expired preview executed")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			revoked := false
			identity := operatortool.Identity{PrincipalID: "operator", OrganizationID: "organization", CredentialID: "credential", SessionID: "browser-session"}
			connection := operatortool.Connection{ID: "connection", Client: "MCP client", Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				if revoked {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
				return operatortool.Authority{Identity: identity, Check: func(_ context.Context, r operatortool.Requirement) error {
					if r.Scope == apikey.ScopeWrite && r.ProjectID != "project" {
						return operatortool.ErrAccessDenied
					}
					return nil
				}}, nil
			}}
			ctx := operatortool.WithConnection(t.Context(), connection)
			human := WithOperatorApproval(t.Context(), identity)
			executor := &actionExecutorStub{result: "completed"}
			service := newTestService(nil, nil, executor)
			if err := service.AttachConnection(ctx); err != nil {
				t.Fatal(err)
			}
			tt.run(t, service, executor, ctx, human, &revoked)
		})
	}
}

func connectionTestAction() Action {
	return Action{Kind: ActionStopRun, ProjectID: "project", RequestID: "request", Arguments: json.RawMessage(`{"destination":"Cancelled"}`), Labels: []string{"original"}}
}

func TestActionConfirmationClassification(t *testing.T) {
	for _, tt := range []struct {
		action Action
		want   bool
	}{
		{Action{Kind: ActionMoveItem, CurrentState: "Backlog", TargetState: "Todo"}, false},
		{Action{Kind: ActionMoveItem, CurrentState: "Todo", TargetState: "Cancelled"}, true},
		{Action{Kind: ActionMoveItem, CurrentState: "Done", TargetState: "Todo"}, true},
		{Action{Kind: ActionMoveItem, CurrentState: "In Progress", TargetState: "Backlog"}, true},
		{Action{Kind: ActionMoveItem, NativeWorkflow: true, CurrentState: "Intake", TargetState: "Ready"}, false},
		{Action{Kind: ActionMoveItem, NativeWorkflow: true, CurrentState: "Ready", TargetState: "Retired", Material: true}, true},
		{Action{Kind: ActionKind(operatortool.CreditCheckout)}, true},
		{Action{Kind: ActionKind(operatortool.CreditAutoFund), Arguments: json.RawMessage(`{"enabled":true}`)}, true},
		{Action{Kind: ActionKind(operatortool.CreditAutoFund), Arguments: json.RawMessage(`{"enabled":false}`)}, false},
		{Action{Kind: ActionKind(operatortool.CreditAutoFund), Arguments: json.RawMessage(`{}`)}, true},
		{Action{Kind: ActionSetPriority}, false}, {Action{Kind: ActionStopRun}, true},
		{Action{Kind: ActionFileIssue}, false}, {Action{Kind: ActionFileIssue, State: "Done"}, true},
		{Action{Kind: "create_workspace"}, false},
		{Action{Kind: "delete_workspace"}, true},
		{Action{Kind: "delete_conversation_attachment"}, true},
		{Action{Kind: "create_project_action_run"}, true},
		{Action{Kind: "post_conversation_command", Arguments: json.RawMessage(`{"input":{"kind":"message"}}`)}, false},
		{Action{Kind: "post_conversation_command", Arguments: json.RawMessage(`{"input":{"kind":"interrupt"}}`)}, true},
		{Action{Kind: "patch_project_action", Arguments: json.RawMessage(`{"input":{"name":"Renamed"}}`)}, false},
		{Action{Kind: "patch_project_action", Arguments: json.RawMessage(`{"input":{"command":"changed"}}`)}, true},
		{Action{Kind: "patch_project_action", Arguments: json.RawMessage(`{"input":{"run_on_worktree_creation":false}}`)}, true},

		{Action{Kind: ActionKind(operatortool.UpdateRunnerHost)}, false},
		{Action{Kind: ActionKind(operatortool.UpdateRunnerHost), MaterialChange: true}, true},
		{Action{Kind: ActionKind(operatortool.RecoverAttempt), Destination: "inspect"}, false},
		{Action{Kind: ActionKind(operatortool.RecoverAttempt), Destination: "abandon"}, true},
		{Action{Kind: ActionKind(operatortool.AddComment)}, false},
		{Action{Kind: ActionKind(operatortool.EditComment)}, false},
		{Action{Kind: ActionKind(operatortool.DeleteComment)}, true},
		{Action{Kind: ActionKind(operatortool.EditItem)}, false},
		{Action{Kind: ActionKind(operatortool.EditItem), Material: true}, true},
		{Action{Kind: ActionFileIssue, Material: true}, true},
		{Action{Kind: ActionMoveItem, CurrentState: "Todo", TargetState: "Backlog", Material: true}, true},
		{Action{Kind: ActionKind(operatortool.SessionLogout)}, true},
		{Action{Kind: "billing"}, true}, {Action{Kind: "access"}, true},
		{Action{Kind: ActionKind(operatortool.ResumeProvisioning)}, true},
		{Action{Kind: "create_native_project"}, false},
		{Action{Kind: "save_onboarding"}, false},
		{Action{Kind: "advance_git_hub_import"}, false},
		{Action{Kind: "command_git_hub_batch", Arguments: json.RawMessage(`{"input":{"action":"discover"}}`)}, false},
		{Action{Kind: "command_git_hub_batch", Arguments: json.RawMessage(`{"input":{"action":"more"}}`)}, false},
		{Action{Kind: "command_git_hub_batch", Arguments: json.RawMessage(`{"input":{"action":"apply","allow_dispatch":false}}`)}, false},
		{Action{Kind: "command_git_hub_batch", Arguments: json.RawMessage(`{"input":{"action":"apply","allow_dispatch":true}}`)}, true},
		{Action{Kind: "command_git_hub_batch", Arguments: json.RawMessage(`{"input":{"action":"retry"}}`)}, true},
		{Action{Kind: "command_git_hub_batch", Arguments: json.RawMessage(`{"input":{"action":"unknown"}}`)}, true},
		{Action{Kind: ActionKind(operatortool.BudgetOverrideClear)}, false},
		{Action{Kind: "start_git_hub_import", Arguments: json.RawMessage(`{"input":{"restart":false}}`)}, false},
		{Action{Kind: "start_git_hub_import", Arguments: json.RawMessage(`{"input":{"restart":true}}`)}, true},
		{Action{Kind: "cutover_project", Arguments: json.RawMessage(`{"input":{"dry_run":true}}`)}, false},
		{Action{Kind: "cutover_project", Arguments: json.RawMessage(`{"input":{"dry_run":false}}`)}, true},
		{Action{Kind: "update_project_integration", Arguments: json.RawMessage(`{"input":{"intake":"manual","projection":"disabled","repository_enabled":false}}`)}, false},
		{Action{Kind: "update_project_integration", Arguments: json.RawMessage(`{"input":{"projection":"summary"}}`)}, true},
		{Action{Kind: "update_project_integration", Arguments: json.RawMessage(`{"input":{"projection":"disabled","repository_enabled":true}}`)}, true},
		{Action{Kind: "update_project_integration", Arguments: json.RawMessage(`{"input":{"projection":"disabled","states":[{"name":"Backlog","dispatchable":false}]}}`)}, true},
		{Action{Kind: "cutover_project", Arguments: json.RawMessage(`{"input":{"dry_run":true},"yolo":true}`)}, true},
		{Action{Kind: "approve_project_policy"}, true},
		{Action{Kind: "approve_change_review_policy"}, true},
		{Action{Kind: ActionKind(operatortool.PublishChangeVersion)}, false},
		{Action{Kind: ActionKind(operatortool.PublishChangeVersion), Mutation: mutation.Metadata{Source: "chat"}}, true},
		{Action{Kind: "revoke_project_policy"}, true},
		{Action{Kind: "remove_project_secret"}, true},
		{Action{Kind: ActionKind(operatortool.BudgetOverrideSet)}, true},
	} {
		if got := RequiresConfirmation(tt.action); got != tt.want {
			t.Errorf("%+v: confirmation=%v, want %v", tt.action, got, tt.want)
		}
	}
}
