package workspacerelay

import (
	"context"
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HTTPDeps struct {
	ReadOperatorWorkspace   func(context.Context, hubscope.NativeScope, string) (workspacehub.Record, error)
	Relay                   *Relay
	AccessRead              func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	AccessWrite             func(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error
	AuthInput               func(hubscope.NativeScope) hubauth.Input
	AuthorizeAccessScope    func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	NativeAPIError          func(echo.Context, error, ...any) error
	Repository              func() tenantstore.Repository
	Clock                   func() time.Time
	Logger                  *slog.Logger
	ActorLogger             func() *slog.Logger
	TerminalConfig          func() workspacehub.TerminalConfig
	Hosted                  func() bool
	PublicURL               func() string
	CookieName              string
	Authenticate            func(context.Context, string) (auth.Session, error)
	HostedSessionCredential func(context.Context, auth.Session, string) (hubapi.APICredential, int, error)
	RequireWorkspaces       func() error
	AppOrganizationID       func() string
	ContentContext          func(context.Context) context.Context
	RunDetailStore          func() attachment.DetailStore
	PrepareMeteredContent   func(context.Context, string, []byte, bool) (attachment.ArchiveReference, error)
	ActionRunReason         func(string) string
	ActionRunClaimed        error
	RunnerForLease          func(context.Context, tenantstore.Reader, string) (runnerstore.Identity, error)
	RunnerMatchesLease      func(runnerstore.Identity, tracker.OrganizationID, string, string, tracker.MachineID, time.Time) bool
	RunnerExcluded          func([]runnerauth.Exclusion) error
	TicketCursor            func() string
	SetTicketCursor         func(string)
}

type HTTP struct {
	HTTPDeps
}

func (w *HTTP) now() time.Time { return w.Clock().UTC() }

type WorkerIdentity = workspacehub.WorkerIdentity
