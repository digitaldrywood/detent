package githubhosted

import (
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type Deps struct {
	Repository   tenantstore.Repository
	Organization tracker.OrganizationID
	Error        func(echo.Context, string, string, error) error
	Unavailable  error
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
