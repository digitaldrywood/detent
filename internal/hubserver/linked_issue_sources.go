package hubserver

import (
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const linkedIssueRepositoryRequired = "Read get_project_integration for this project. An administrator must associate its repository through bind_native_repository before linking GitHub issues; use source runner_checkout with an enrolled runner reporting the matching checkout."
const linkedIssueRepositoryMismatch = "GitHub issue must belong to this native project's attached repository"

func validateLinkedIssueRepository(integration ProjectIntegration, repository string) error {
	if integration.Profile == "native" && integration.Repository == "" && integration.CheckoutRepository == "" {
		return hubapi.NativePublicInvalid(linkedIssueRepositoryRequired)
	}
	if integration.Profile != "native" || (!strings.EqualFold(repository, integration.Repository) && !strings.EqualFold(repository, integration.CheckoutRepository)) {
		return hubapi.NativePublicInvalid(linkedIssueRepositoryMismatch)
	}
	return nil
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
