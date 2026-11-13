package hubserver

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/githubhosted"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedGitHubService() *githubhosted.Service {
	organization := tracker.OrganizationID("")
	if s.config.Hosted != nil {
		organization = tracker.OrganizationID(s.config.Hosted.OrganizationID)
	}
	return githubhosted.New(githubhosted.Deps{Repository: s.config.TenantRepository, Organization: organization, Error: s.internalAPIError, Unavailable: ErrPostgresFeatureUnavailable})
}
func (s *Service) hostedGitHubRepositories(c echo.Context) error {
	return s.hostedGitHubService().Repositories(c)
}
func (s *Service) hostedGitHubReceipt(c echo.Context) error {
	return s.hostedGitHubService().Receipt(c)
}
