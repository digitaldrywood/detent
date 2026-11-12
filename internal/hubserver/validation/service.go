package validation

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
	"net/http"
	"time"
)

type Deps struct {
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
