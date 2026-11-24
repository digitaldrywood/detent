package integrationpage

import (
	"context"
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type Deps struct {
	Repository  tenantstore.Repository
	Details     attachment.DetailStore
	Now         func() time.Time
	Logger      *slog.Logger
	Workflow    func(context.Context, projectstore.Record) (string, error)
	RecordError func(error) error
	Resolve     func(context.Context, echo.Context) error
	Scope       func(echo.Context) hubapi.Scope
	ReadProject func(context.Context, hubapi.Scope) (projectstore.Record, error)
	Live        func(echo.Context, *projectstore.Integration) error
	Error       func(echo.Context, error) error
	Audit       func(echo.Context, hubapi.Scope) error
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
func (s *Service) snapshot(ctx context.Context, scope hubapi.Scope, run func(context.Context) error) error {
	if repository, ok := s.deps.Repository.(tenantstore.RequestReader); ok {
		return repository.ReadRequest(ctx, scope.Organization, run)
	}
	return run(ctx)
}

func (s *Service) authorizeLive(ctx context.Context, scope hubapi.Scope) error {
	return s.deps.RecordError(s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return scope.Authorize(ctx, r, false)
	}))
}
