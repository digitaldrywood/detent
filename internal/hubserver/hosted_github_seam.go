package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/githubhosted"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func (s *Service) hostedGitHubService() *githubhosted.Service {
	return githubhosted.New(githubhosted.Deps{Repository: s.config.TenantRepository, Organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), Error: s.internalAPIError, Unavailable: ErrPostgresFeatureUnavailable})
}
func (s *Service) hostedGitHubRepositories(c echo.Context) error {
	return s.hostedGitHubService().Repositories(c)
}
func (s *Service) hostedGitHubReceipt(c echo.Context) error {
	return s.hostedGitHubService().Receipt(c)
}
