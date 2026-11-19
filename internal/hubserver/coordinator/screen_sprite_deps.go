package coordinator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/sprites"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ScreenReadDeps struct {
	ActionContext    func(context.Context, conversations.ConversationRecord) (context.Context, error)
	Operator         func(context.Context) (hubapi.APICredential, error)
	AuthorizeRecord  func(context.Context, hubscope.NativeScope, conversations.ConversationRecord, bool) error
	RestrictAudience func(context.Context, conversations.ConversationRecord, hubapi.APICredential) error
	ReadSlack        func(context.Context, tracker.OrganizationID) (any, error)
	ReadBudget       func(context.Context, hubscope.NativeScope) (any, error)
	ReadRunners      func(context.Context, conversations.ConversationRecord, hubapi.APICredential, bool) (any, error)
	AllRunnerGrants  func(context.Context, hubapi.APICredential) bool
	ReadReport       func(context.Context, conversations.ConversationRecord, hubapi.APICredential, runner.AgentToolCall) (any, error)
	FleetRequirement func(string) operatortool.Requirement
	RunnerAttached   func(context.Context, conversations.ConversationRecord, string) (bool, error)
	ExecuteOperator  func(context.Context, operatortool.Call) (operatortool.Result, error)
	ProjectMembers   func(json.RawMessage, tracker.ProjectID) (any, error)
}

type ScreenReportDeps struct {
	Now               func() time.Time
	FleetPermitted    func(string, hubapi.APICredential) bool
	UsageWindow       func(string, time.Time) (operatortool.AnalyticsWindow, error)
	ReadProjectReport func(context.Context, hubapi.APICredential, string, operatortool.AnalyticsWindow) (any, bool, error)
	ReadDiagnostics   func(context.Context, hubapi.APICredential, operatortool.AnalyticsWindow, tracker.ProjectID) (any, error)
}

type ReadAudienceDeps struct {
	ReadTenant func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	Write      func(context.Context, string, func(context.Context, tenantstore.Writer, *conversations.ConversationRecord, time.Time) error, ...func(context.Context, *conversations.ConversationPublicationPlan) error) error
}

type SpriteBootstrap struct {
	Name         string
	State        string
	BootstrapLog string
}

type SpritePool struct {
	Settings      sprites.PoolSettings
	LastBootstrap *SpriteBootstrap
	Status        func(context.Context, hubscope.NativeScope, string, tracker.ProjectID) (any, error)
}

type SpriteCommandDeps struct {
	HostedSecrets   bool
	TokenPresent    func(context.Context, hubscope.NativeScope) (bool, error)
	ReadPool        func(context.Context, hubscope.NativeScope) (SpritePool, error)
	RecordAuthority func(context.Context, hubscope.NativeScope, *int64) (string, error)
	Update          func(context.Context, hubscope.NativeScope, tracker.Mutation, SpriteChange, func(sprites.PoolSettings) (sprites.PoolSettings, *string, error)) error
	RecheckMutation func(context.Context, hubscope.NativeScope) error
	StartPool       func(hubscope.NativeScope)
}

type SpritePreviewDeps struct {
	ProposalDeps
	Commands   SpriteCommandDeps
	HostedPath func(string) string
}
