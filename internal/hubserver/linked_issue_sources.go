package hubserver

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/githubhosted"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const linkedIssueRepositoryRequired = githubhosted.LinkedIssueRepositoryRequired
const linkedIssueRepositoryMismatch = githubhosted.LinkedIssueRepositoryMismatch

func validateLinkedIssueRepository(integration ProjectIntegration, repository string) error {
	return githubhosted.ValidateLinkedIssueRepository(integration, repository)
}

func (s *Service) intakeLinkedIssue(c echo.Context) error {
	var request tracker.GitHubIntake
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	return s.intakeLinkedIssueRecord(c, request)
}
