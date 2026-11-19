package coordinator

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProjectActionDeps struct {
	ProposalDeps
	GitHubAvailable    bool
	ReadProject        func(context.Context, hubscope.NativeScope) (projectstore.Record, error)
	ReadWorkflow       func(context.Context, projectstore.Record) (string, error)
	PreviewCommand     func(context.Context, operatortool.Call) (json.RawMessage, error)
	ObserveIssue       func(context.Context, hubscope.NativeScope, string) (tracker.NativeIssue, error)
	ValidateMoveAnswer func(context.Context, hubscope.NativeScope, tracker.NativeIssue, string, string) error
}

type actionExecutor func(context.Context, chat.Action) (chat.ActionExecution, error)

type ActionDispatchDeps struct {
	Authorize          func(context.Context, chat.Action) (context.Context, error)
	ProposalAgent      func(context.Context, chat.Action) (string, error)
	WithAgent          func(context.Context, string) context.Context
	CatalogWrite       func(string) bool
	ExecuteCatalog     actionExecutor
	ExecuteSettings    actionExecutor
	ExecuteArchive     actionExecutor
	ExecuteSplit       actionExecutor
	ExecuteRunner      actionExecutor
	PromoteSpriteToken actionExecutor
	ExecuteSprite      actionExecutor
	ExecuteWorkflow    actionExecutor
	ExecuteProject     func(context.Context, operatortool.Call) (json.RawMessage, error)
	StopRun            func(context.Context, hubscope.NativeScope, chat.Action) (chat.ActionExecution, error)
	CreateIssue        func(context.Context, hubscope.NativeScope, tracker.CreateIssue) (json.RawMessage, error)
	TransitionIssue    func(context.Context, hubscope.NativeScope, string, tracker.Transition, string) (json.RawMessage, error)
	MutateIssue        func(context.Context, hubscope.NativeScope, string, operatortool.WorkArguments) (json.RawMessage, error)
}

type ActionAuthorizationDeps struct {
	SettingsRequirement func(string, string) operatortool.Requirement
	CatalogWrite        func(string) bool
	CatalogRequirement  func(chat.Action) operatortool.Requirement
	Operator            func(context.Context) (hubapi.APICredential, error)
	ReadConversation    func(context.Context, hubscope.NativeScope, string) (conversations.ConversationRecord, error)
}
