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

func (s *Service) readReadableOrganizationProjectRank(ctx context.Context, db nativeQueryer, credential apiCredential) (organizationProjectRank, error) {
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	value := organizationProjectRank{ProjectIDs: []tracker.ProjectID{}}
	if err := db.QueryRowContext(ctx, "SELECT scheduling_revision FROM organizations WHERE id = ?", organization).Scan(&value.Revision); err != nil {
		return value, err
	}
	condition, args := credential.projectGrantSQL("p.organization_id", "p.id")
	args = append([]any{organization}, args...)
	rows, err := db.QueryContext(ctx, "SELECT p.id FROM projects p WHERE p.organization_id = ? AND p.deleted_at IS NULL AND ("+condition+") ORDER BY p.scheduling_rank, p.created_at, p.id", args...)
	if err != nil {
		return value, err
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.ProjectID
		if err := rows.Scan(&id); err != nil {
			return value, err
		}
		value.ProjectIDs = append(value.ProjectIDs, id)
	}
	return value, rows.Err()
}

func (s *Service) hostedProjectRank(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	value, err := s.readReadableOrganizationProjectRank(ctx, s.database.reader, credential)
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
		readable, err := s.readReadableOrganizationProjectRank(ctx, tx, credential)
		if err != nil {
			return nil, err
		}
		value, err := projectsettings.UpdateActiveRank(ctx, tx, scope.organization, change, readable.ProjectIDs)
		if errors.Is(err, projectsettings.ErrConflict) {
			return nil, nativeConflict(tracker.Revision(value.Revision))
		}
		if errors.Is(err, projectsettings.ErrInvalid) {
			return nil, nativeInvalid("Order must include every readable project once")
		}
		if err != nil {
			return nil, err
		}
		return s.readReadableOrganizationProjectRank(ctx, tx, credential)
	})
}
