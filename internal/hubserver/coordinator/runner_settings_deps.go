package coordinator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RunnerDeps struct {
	SnapshotAvailable bool
	ReadSnapshot      func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	Authorize         func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	View              func(runnerstore.RoutingRecord, time.Time, runnerstore.UrgentUpdate) runnerauth.Runner
	Scope             func(context.Context, tracker.ProjectID, bool) (hubscope.NativeScope, error)
	Now               func() time.Time
	UpdateRouting     func(context.Context, hubscope.NativeScope, string, runnerauth.RoutingChange, *string) error
}

type RunnerPreviewDeps struct {
	ProposalDeps
	Runners          RunnerDeps
	ReadableProjects func(context.Context, conversations.ConversationRecord) ([]tracker.ProjectID, error)
}

type SettingsDeps struct {
	Operator           func(context.Context) (hubapi.APICredential, error)
	Runner             func(context.Context, hubscope.NativeScope, string) (runnerauth.Runner, error)
	DecodeRouting      func(json.RawMessage) (runnerauth.RoutingChange, error)
	DecodeCapacity     func(json.RawMessage) (string, string, error)
	FleetTool          func(string) bool
	FleetPermitted     func(string, hubapi.APICredential) bool
	ExecuteProject     func(context.Context, chat.Action) (chat.ActionExecution, error)
	ExecuteFleet       func(context.Context, chat.Action) (chat.ActionExecution, error)
	ReadCapacity       func(context.Context, hubscope.NativeScope, string, string) (any, error)
	ReadProjectRunners func(context.Context, conversations.ConversationRecord, hubapi.APICredential, bool) (any, error)
}

type SettingsPreviewDeps struct {
	ProposalDeps
	Settings       SettingsDeps
	Requirement    func(string, string) operatortool.Requirement
	PreviewFleet   func(context.Context, string, json.RawMessage) (chat.Action, error)
	PreviewProject func(context.Context, operatortool.Call) (json.RawMessage, error)
	ProjectError   func(error) error
}
