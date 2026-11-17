package validation

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Details         func(context.Context) attachment.DetailStore
	HydrateWorkItem func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	FilterChange    func(context.Context, hubapi.Scope, tracker.ChangeRequest) (tracker.ChangeRequest, bool, error)
	RecordError     func(error) error
	OriginMatches   func(string, gate.ScheduledEvidence) bool

	InvalidRequest func(echo.Context, error) error
	Repository     tenantstore.Repository
	Now            func() time.Time
	SecretKeys     *hubsecrets.Keyring
	Organization   func() tracker.OrganizationID
	Scope          func(echo.Context) hubauth.Input
	Authorize      func(context.Context, accessstore.Reader, hubauth.Input, bool) error
	Error          func(echo.Context, error) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func staleExecution(message string) error {
	return &hubapi.Error{Code: "stale_execution", Message: message, Status: http.StatusConflict}
}

func reporterOwns(scope hubapi.Scope, actor tracker.Actor) bool {
	return scope.HostedRole != "reporter" || scope.Actor.PrincipalID != "" && actor.PrincipalID == scope.Actor.PrincipalID
}
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
