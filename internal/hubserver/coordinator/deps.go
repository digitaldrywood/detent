package coordinator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProposalDeps struct {
	ActionContext func(context.Context, conversations.ConversationRecord) (context.Context, error)
	MessageID     func() string
	Submit        func(context.Context, conversations.ConversationRecord, runner.AgentToolCall, chat.Action) (any, error)
}

type WorkflowPreviewDeps struct {
	ProposalDeps
	ProjectScope         func(string, bool) apikey.Scope
	LocalConfiguration   func(context.Context, string, operatortool.LocalProjectArguments) (operatortool.Result, error)
	ReadConfiguration    func(context.Context, operatortool.Call) (operatortool.Result, error)
	ReadPolicy           func(context.Context, operatortool.Call) (operatortool.Result, error)
	PreviewApproval      func(context.Context, operatortool.Call) (json.RawMessage, error)
	PreviewConfiguration func(context.Context, string, operatortool.LocalProjectArguments) (operatortool.Result, error)
}

type WorkflowExecutionDeps struct {
	LocalConfiguration func(context.Context, string, operatortool.LocalProjectArguments) (operatortool.Result, error)
	Approve            func(context.Context, operatortool.Call) (json.RawMessage, error)
	Audit              func(context.Context, chat.Action, string)
}

type SlashActionDeps struct {
	ProposalDeps
	ResolveIssue  func(context.Context, hubscope.NativeScope, string) (tracker.NativeIssue, error)
	ReadProject   func(context.Context, hubscope.NativeScope) (projectstore.Record, error)
	ProjectAction func(context.Context, conversations.ConversationRecord, runner.AgentToolCall, ...bool) (any, error)
}

type IssueContextDeps struct {
	ActionContext    func(context.Context, conversations.ConversationRecord) (context.Context, error)
	ResolveReference func(context.Context, conversations.ConversationRecord, string) (string, error)
	WorkReader       func(hubscope.NativeScope) operatortool.WorkReader
}

type StopRunPreviewDeps struct {
	ActionContext func(context.Context, conversations.ConversationRecord) (context.Context, error)
	Submit        func(context.Context, conversations.ConversationRecord, runner.AgentToolCall, chat.Action) (any, error)
	ResolveIssue  func(context.Context, hubscope.NativeScope, string) (tracker.NativeIssue, error)
	ReadProject   func(context.Context, hubscope.NativeScope) (projectstore.Record, error)
	ActiveLease   func(context.Context, hubscope.NativeScope, tracker.NativeWorkItemID) (leasestore.Lease, error)
	Now           func() time.Time
}

type StopRunTransition func(context.Context, hubscope.NativeScope, string, tracker.Transition, *operatortool.NativeStopRunArguments, string) (json.RawMessage, error)

type CatalogToolDeps struct {
	ProposalDeps
	Operator     func(context.Context) (hubapi.APICredential, error)
	WithOperator func(context.Context, hubapi.APICredential, string) context.Context
	Catalog      func(context.Context) ([]operatortool.Definition, error)
	Execute      func(context.Context, operatortool.Call) (operatortool.Result, error)
	Excluded     func(string) bool
}

type CatalogExecutionDeps struct {
	BindConnection func(context.Context, string) (context.Context, error)
	Catalog        func(context.Context) ([]operatortool.Definition, error)
	Execute        func(context.Context, operatortool.Call) (operatortool.Result, error)
}
