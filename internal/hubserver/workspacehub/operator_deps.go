package workspacehub

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type OperatorReceipt struct {
	Actor, Operation, Key string
	Input                 json.RawMessage
}

type OperatorChat interface {
	AttachConnection(context.Context) error
	CheckConnection(context.Context) error
	Submit(context.Context, chat.Action) (chat.Action, error)
	RestoreActionResult(context.Context, chat.Action, json.RawMessage) (chat.Action, error)
	ConnectionResult(context.Context, string) (json.RawMessage, error)
	Action(string, string) (chat.Action, bool)
}

type OperatorDeps struct {
	AuthorizeCatalog             func(context.Context, operatortool.Requirement) (context.Context, error)
	ConversationsEnabled         func() bool
	WorkspacesEnabled            func() bool
	Chat                         OperatorChat
	BoundScope                   func(context.Context) hubscope.NativeScope
	LookupScope                  func(context.Context) (hubscope.NativeScope, bool)
	Clock                        func() time.Time
	Logger                       func() *slog.Logger
	HostedAudit                  func(context.Context, *auth.HostedIdentity, string, string, string, int) error
	Repository                   func() tenantstore.Repository
	AuthorizeAccessScope         func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	HydrateActionRecord          func(context.Context, hubscope.NativeScope, actionstore.Record) (workspacesession.Action, error)
	NativeConflict               func(error) error
	SafeChangeError              func(error) error
	ReadHostedOperation          func(context.Context, OperatorReceipt) (json.RawMessage, bool, error)
	ClaimHostedOperation         func(context.Context, OperatorReceipt) (bool, json.RawMessage, error)
	CompleteHostedOperation      func(context.Context, OperatorReceipt, any) (json.RawMessage, error)
	RequireOperatorRunners       func(context.Context, hubapi.NativeQueryer, hubscope.NativeScope) error
	RequireWorkspaces            func() error
	ReadOperatorWorkspace        func(context.Context, hubscope.NativeScope, string) (Record, error)
	CheckConversation            func(context.Context, hubapi.NativeQueryer, hubscope.NativeScope, string) error
	CheckWorkItem                func(context.Context, hubapi.NativeQueryer, hubscope.NativeScope, string) error
	ReadWorkspaces               func(context.Context, hubscope.NativeScope, string, string, int) (json.RawMessage, error)
	ReadWorkspace                func(context.Context, hubscope.NativeScope, string) (json.RawMessage, error)
	ReadableConversationProjects func(context.Context, hubscope.NativeScope) ([]tracker.ProjectID, bool, error)
	ReadConversationsPage        func(context.Context, hubscope.NativeScope, conversations.ConversationListQuery) (json.RawMessage, error)
	ReadConversationSnapshotFor  func(context.Context, hubscope.NativeScope, string, string, bool) (json.RawMessage, error)
	ReadConversationSnapshot     func(context.Context, hubscope.NativeScope, string) (json.RawMessage, error)
	ReadConversationMessages     func(context.Context, hubscope.NativeScope, string, int64, int) (json.RawMessage, error)
	ReadAttachment               func(context.Context, hubscope.NativeScope, string, string) (string, []byte, error)
	ReadWorkItemReferences       func(context.Context, hubscope.NativeScope, string) (json.RawMessage, error)
	ReadActions                  func(context.Context, hubscope.NativeScope) (json.RawMessage, error)
	ReadActionRuns               func(context.Context, hubscope.NativeScope, string, int) (json.RawMessage, error)
	ReadActionRun                func(context.Context, hubscope.NativeScope, string, string) (workspacesession.Run, string, error)
	ListTerminalRecordings       func(context.Context, hubscope.NativeScope, string, int) (json.RawMessage, error)
	ReadTerminalRecording        func(context.Context, hubscope.NativeScope, string, string) (string, string, error)
	ReadWorkspaceFilesTool       func(context.Context, hubscope.NativeScope, string, operatortool.WorkspaceArguments) (json.RawMessage, error)
	CommandCreateWorkspace       func(context.Context, hubscope.NativeScope, Request) (json.RawMessage, error)
	CommandCloseWorkspace        func(context.Context, hubscope.NativeScope, string) (json.RawMessage, error)
	CommandCreateConversation    func(context.Context, hubscope.NativeScope, conversations.ConversationCreateRequest) (json.RawMessage, error)
	CommandPostConversation      func(context.Context, hubscope.NativeScope, string, conversation.Command) (json.RawMessage, error)
	CommandLinkConversation      func(context.Context, hubscope.NativeScope, string, conversations.ConversationLinkRequest) (json.RawMessage, error)
	CommandPatchConversation     func(context.Context, hubscope.NativeScope, string, conversations.ConversationPatchRequest) (json.RawMessage, error)
	CommandUploadAttachment      func(context.Context, hubscope.NativeScope, string, conversations.ConversationAttachmentUpload) (json.RawMessage, error)
	CommandDeleteAttachment      func(context.Context, hubscope.NativeScope, string, string) (json.RawMessage, error)
	ProjectActionCommand         func(context.Context, hubscope.NativeScope, string, operatortool.WorkspaceArguments) (json.RawMessage, error)
}

type Operator struct {
	OperatorDeps
}

func NewOperator(deps OperatorDeps) *Operator {
	return &Operator{OperatorDeps: deps}
}
