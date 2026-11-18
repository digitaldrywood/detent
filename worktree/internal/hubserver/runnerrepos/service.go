package runnerrepos

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

const (
	nativeBase = "/api/v2/organizations/:organization/projects/:project"
	reportPath = nativeBase + "/onboarding/repositories/report"
)

type Deps struct {
	Repository     tenantstore.Repository
	Now            func() time.Time
	Details        func() attachment.DetailStore
	Auth           func() *hubauth.Authenticator
	RequireFeature func(context.Context, billingstore.Reader, string, time.Time) error
	ApplyUsage     func(context.Context, billingstore.Writer, time.Time, string, bool, bool) error
	RecordError    func(error) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

type HTTPDeps struct {
	Service func() *Service
	Admin   echo.MiddlewareFunc
	Worker  echo.MiddlewareFunc
	Input   func(echo.Context) hubauth.Input
	Feature func(echo.Context) string
	Decode  func(echo.Context, any) error
	Error   func(echo.Context, error) error
	Invalid func(echo.Context, error) error
}

type httpRoutes struct{ deps HTTPDeps }

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	h := &httpRoutes{deps: deps}
	e.POST(nativeBase+"/onboarding/repositories", h.request, deps.Admin)
	e.POST(reportPath, h.report, deps.Worker)
}

func ReportRoute(path string) bool { return path == reportPath }
