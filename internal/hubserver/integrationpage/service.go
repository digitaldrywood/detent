package integrationpage

import (
	"context"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/labstack/echo/v4"
	"log/slog"
	"time"
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
	Error       func(echo.Context, error) error
	Audit       func(echo.Context, hubapi.Scope) error
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
func (s *Service) snapshot(ctx context.Context, scope hubapi.Scope, run func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if repository, ok := s.deps.Repository.(tenantstore.RequestReader); ok {
		return repository.ReadRequest(ctx, scope.Organization, run)
	}
	return run(ctx)
}
