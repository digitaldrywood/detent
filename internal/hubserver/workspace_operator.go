package hubserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type operatorScopeKey struct{}
type workspaceOperatorExecutor struct{ server *Service }
type workspaceToolRequest = operatortool.WorkspaceArguments

var errWorkspaceOperationUnavailable = workspacehub.ErrOperationUnavailable

func (s *Service) workspaceOperator() *workspacehub.Operator {
	command := func(receipt workspacehub.OperatorReceipt) hostedCommand {
		return hostedCommand{actor: receipt.Actor, operation: receipt.Operation, key: receipt.Key, input: receipt.Input}
	}
	return workspacehub.NewOperator(workspacehub.OperatorDeps{
		AuthorizeCatalog:     s.authorizeCatalog,
		ConversationsEnabled: func() bool { return s.conversations != nil },
		WorkspacesEnabled:    func() bool { return s.workspaces != nil },
		Chat:                 s.operatorChat,
		BoundScope: func(ctx context.Context) hubscope.NativeScope {
			return ctx.Value(operatorScopeKey{}).(hubscope.NativeScope)
		},
		LookupScope: func(ctx context.Context) (hubscope.NativeScope, bool) {
			scope, ok := ctx.Value(operatorScopeKey{}).(hubscope.NativeScope)
			return scope, ok
		},
		Clock:                func() time.Time { return s.config.Clock() },
		Logger:               func() *slog.Logger { return s.config.Logger },
		HostedAudit:          s.hostedAudit,
		Repository:           func() tenantstore.Repository { return s.config.TenantRepository },
		AuthorizeAccessScope: s.authorizeAccessScope,
		HydrateActionRecord:  s.hydrateActionRecord,
		NativeConflict:       operatorNativeConflict,
		SafeChangeError:      hubSafeChangeError,
		ReadHostedOperation: func(ctx context.Context, receipt workspacehub.OperatorReceipt) (json.RawMessage, bool, error) {
			return s.readHostedOperation(ctx, command(receipt))
		},
		ClaimHostedOperation: func(ctx context.Context, receipt workspacehub.OperatorReceipt) (bool, json.RawMessage, error) {
			return s.claimHostedOperation(ctx, command(receipt))
		},
		CompleteHostedOperation: func(ctx context.Context, receipt workspacehub.OperatorReceipt, value any) (json.RawMessage, error) {
			return s.completeHostedOperation(ctx, command(receipt), value)
		},
		RequireOperatorRunners: s.requireOperatorRunners,
		RequireWorkspaces:      func() error { _, err := s.requireWorkspaces(); return err },
		ReadOperatorWorkspace: func(ctx context.Context, scope hubscope.NativeScope, id string) (workspacehub.Record, error) {
			return s.workspaceOperations(nil).ReadOperatorWorkspace(ctx, scope, id)
		},
		CheckConversation: func(ctx context.Context, query hubapi.NativeQueryer, scope hubscope.NativeScope, id string) error {
			_, err := s.conversations.LoadConversation(ctx, query, conversationInput(scope), id)
			return err
		},
		CheckWorkItem: func(ctx context.Context, query hubapi.NativeQueryer, scope hubscope.NativeScope, id string) error {
			_, err := s.resolveOperatorNativeItem(ctx, query, scope, id)
			return err
		},
		ReadWorkspaces:               s.readWorkspaces,
		ReadWorkspace:                s.readWorkspace,
		ReadableConversationProjects: s.readableConversationProjects,
		ReadConversationsPage:        s.readConversationsPage,
		ReadConversationSnapshotFor:  s.readConversationSnapshotFor,
		ReadConversationSnapshot:     s.readConversationSnapshot,
		ReadConversationMessages:     s.readConversationMessages,
		ReadAttachment: func(ctx context.Context, scope hubscope.NativeScope, conversation, id string) (string, []byte, error) {
			value, content, err := s.readAttachment(ctx, scope, conversation, id)
			return value.ID, content, err
		},
		ReadWorkItemReferences: s.readWorkItemReferences,
		ReadActions:            s.readActions,
		ReadActionRuns:         s.readActionRuns,
		ReadActionRun: func(ctx context.Context, scope hubscope.NativeScope, action, run string) (workspacesession.Run, string, error) {
			value, err := s.readActionRun(ctx, scope, action, run)
			if err != nil {
				return workspacesession.Run{}, "", err
			}
			return value.resource(), value.Output, nil
		},
		ListTerminalRecordings: func(ctx context.Context, scope hubscope.NativeScope, workspace string, limit int) (json.RawMessage, error) {
			records, err := s.readRecordings(ctx, scope, workspace, "", limit)
			if err != nil {
				return nil, err
			}
			return json.Marshal(records)
		},
		ReadTerminalRecording: func(ctx context.Context, scope hubscope.NativeScope, workspace, id string) (string, string, error) {
			value, err := s.readRecording(ctx, scope, workspace, id)
			return value.ID, value.Cast, err
		},
		ReadWorkspaceFilesTool:    s.readWorkspaceFilesTool,
		CommandCreateWorkspace:    s.commandCreateWorkspace,
		CommandCloseWorkspace:     s.commandCloseWorkspace,
		CommandCreateConversation: s.commandCreateConversation,
		CommandPostConversation:   s.commandPostConversation,
		CommandLinkConversation:   s.commandLinkConversation,
		CommandPatchConversation:  s.commandPatchConversation,
		CommandUploadAttachment:   s.commandUploadAttachment,
		CommandDeleteAttachment:   s.commandDeleteAttachment,
		ProjectActionCommand:      s.workspaceProjectActionCommand,
	})
}

func (e workspaceOperatorExecutor) OpenConnection(ctx context.Context) error {
	return e.server.workspaceOperator().OpenConnection(ctx)
}
func (e workspaceOperatorExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	return e.server.workspaceOperator().ListTools(ctx)
}
func (e workspaceOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	return e.server.workspaceOperator().Execute(ctx, call)
}
func (e workspaceOperatorExecutor) ExecuteAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return e.server.workspaceOperator().ExecuteAction(ctx, action)
}
func (e workspaceOperatorExecutor) AuditAction(ctx context.Context, action chat.Action, outcome string) {
	e.server.workspaceOperator().AuditAction(ctx, action, outcome)
}
func (s *Service) checkOperatorResource(ctx context.Context, query hubapi.NativeQueryer, scope hubscope.NativeScope, r operatortool.Requirement) error {
	return s.workspaceOperator().CheckResource(ctx, query, scope, r)
}
func (s *Service) readWorkspaceTool(ctx context.Context, scope hubscope.NativeScope, name string, r workspaceToolRequest) (json.RawMessage, error) {
	return s.workspaceOperator().ReadTool(ctx, scope, name, r)
}
func (s *Service) workspaceProjectActionCommand(ctx context.Context, scope hubscope.NativeScope, name string, r workspaceToolRequest) (json.RawMessage, error) {
	switch name {
	case "create_project_action":
		var input projectActionRequest
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.IdempotencyKey = r.RequestID
		return s.commandCreateAction(ctx, scope, input)
	case "patch_project_action":
		var input projectActionPatch
		if operatortool.DecodeArguments(r.Input, &input) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input.IdempotencyKey = r.RequestID
		return s.commandPatchAction(ctx, scope, r.ActionID, input)
	case "delete_project_action":
		return s.commandDeleteAction(ctx, scope, r.ActionID)
	case "create_project_action_run":
		return s.commandRunAction(ctx, scope, r.ActionID, projectActionRunRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, WorkspaceID: r.WorkspaceID, ExpectedActionRevision: r.ExpectedRevision})
	}
	return nil, operatortool.ErrUnknownTool
}
func safeWorkspaceError(err error) error {
	return workspacehub.NewOperator(workspacehub.OperatorDeps{NativeConflict: operatorNativeConflict, SafeChangeError: hubSafeChangeError}).SafeError(err)
}
