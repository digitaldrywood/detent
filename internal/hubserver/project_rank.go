package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type organizationProjectRank = projectsettings.Rank

type projectRankChange = projectsettings.RankChange

func readOrganizationProjectRank(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID) (organizationProjectRank, error) {
	return projectsettings.ReadActiveRank(ctx, db, organization)
}

func (s *Service) hostedProjectRank(c echo.Context) error {
	_, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	value, err := readOrganizationProjectRank(c.Request().Context(), s.database.db, tracker.OrganizationID(s.config.Hosted.OrganizationID))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateHostedProjectRank(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	var change projectRankChange
	if err := decodeAPIJSON(c, &change); err != nil {
		return invalidAPIRequest(c, err)
	}
	value, err := s.updateOrganizationProjectRankCommand(c.Request().Context(), credential, change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateOrganizationProjectRankCommand(ctx context.Context, credential apiCredential, change projectRankChange) (any, error) {
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential, requireHostedAdmin: true}
	return s.runnerAdminTransaction(ctx, scope, false, func(ctx context.Context, tx *sql.Tx, _ time.Time) (any, error) {
		value, err := projectsettings.UpdateActiveRank(ctx, tx, scope.organization, change)
		if errors.Is(err, projectsettings.ErrConflict) {
			return nil, nativeConflict(tracker.Revision(value.Revision))
		}
		if errors.Is(err, projectsettings.ErrInvalid) {
			return nil, nativeInvalid("Rank must include every organization project once")
		}
		return value, err
	})
}
