package coordinator

import (
	"context"
	"log/slog"
	"net/url"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type toolCall func(context.Context, conversations.ConversationRecord, runner.AgentToolCall) (any, error)

type ToolDeps struct {
	ConversationID          string
	CatalogCallName         string
	CatalogFindName         string
	Logger                  *slog.Logger
	RunDetailContext        func(context.Context) context.Context
	EndSegment              func(context.Context)
	ReadConversation        func(context.Context) (conversations.ConversationRecord, error)
	ActionContext           func(context.Context, conversations.ConversationRecord) (context.Context, error)
	Operator                func(context.Context) (hubapi.APICredential, error)
	AuthorizeRecord         func(context.Context, hubscope.NativeScope, conversations.ConversationRecord, bool) error
	ReadableProjects        func(context.Context, conversations.ConversationRecord) ([]tracker.ProjectID, error)
	PostActionRefusal       func(context.Context, string, error) error
	RestrictReadAudience    func(context.Context, conversations.ConversationRecord, hubapi.APICredential) error
	ScreenTool              func(string) bool
	IsSpriteTool            func(string) bool
	SpriteMutation          func(string) bool
	ScreenRead              toolCall
	SettingsTool            toolCall
	WorkflowTool            toolCall
	RunnerTool              toolCall
	PromoteSpritesTokenTool toolCall
	SpriteTool              toolCall
	ReadIssueHistory        toolCall
	ProjectAction           func(context.Context, conversations.ConversationRecord, runner.AgentToolCall, ...bool) (any, error)
	ProposeStopRun          toolCall
	SlashAction             toolCall
	FindTools               toolCall
	CallTool                toolCall
	ProposeIssueArchive     toolCall
	ProposeIssueSplit       toolCall
	ListAttention           func(context.Context, conversations.ConversationRecord, []tracker.ProjectID, string, int) (any, error)
	ExplainIssue            func(context.Context, conversations.ConversationRecord, []tracker.ProjectID, string) (any, error)
	ProposeIssue            func(context.Context, conversations.ConversationRecord, []tracker.ProjectID, string, string, string) (any, error)
}

type WorkToolDeps struct {
	ActionContext   func(context.Context, conversations.ConversationRecord) (context.Context, error)
	WorkReadContext func(context.Context, conversations.ConversationRecord) (context.Context, hubscope.NativeScope, error)
	Operator        func(context.Context) (hubapi.APICredential, error)
	ReadTenant      func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	Authorize       func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	AllowedProjects func(context.Context, tenantstore.Reader, hubscope.NativeScope, []tracker.ProjectID) ([]tracker.ProjectID, error)
	Now             func() time.Time
	ResolveIssue    func(context.Context, hubscope.NativeScope, string) (tracker.NativeIssue, error)
	ReadWorkItem    func(context.Context, hubscope.NativeScope, string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	HydrateIssue    func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	ReadAttempt     func(context.Context, hubscope.NativeScope, string, string) (tracker.NativeAttempt, error)
	ReadComments    func(context.Context, hubscope.NativeScope, string, url.Values) (tracker.Page[tracker.NativeComment], error)
}
