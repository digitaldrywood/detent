package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type organizationProjectRank = projectsettings.Rank

type projectRankChange = projectsettings.RankChange

func (s *Service) hostedProjectRank(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	value, err := s.readOrganizationRankRecord(ctx, nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), credential: credential})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateHostedProjectRank(c echo.Context) error {
	credential, err := s.appAdministrator(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	var change projectRankChange
	if err := hubapi.DecodeAPIJSON(c, &change); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.updateOrganizationProjectRankCommand(c.Request().Context(), credential, change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateOrganizationProjectRankCommand(ctx context.Context, credential hubapi.APICredential, change projectRankChange) (any, error) {
	scope := nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), credential: credential, requireHostedAdmin: true}
	return s.updateOrganizationRankRecord(ctx, scope, change)
}
