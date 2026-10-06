package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type organizationProjectRank struct {
	Revision   int64               `json:"revision"`
	ProjectIDs []tracker.ProjectID `json:"project_ids"`
}

type projectRankChange struct {
	ExpectedRevision int64               `json:"expected_revision"`
	ProjectIDs       []tracker.ProjectID `json:"project_ids"`
}

func readOrganizationProjectRank(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID) (organizationProjectRank, error) {
	value := organizationProjectRank{ProjectIDs: []tracker.ProjectID{}}
	if err := db.QueryRowContext(ctx, "SELECT scheduling_revision FROM organizations WHERE id = ?", organization).Scan(&value.Revision); err != nil {
		return value, err
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM projects WHERE organization_id = ? ORDER BY scheduling_rank, created_at, id", organization)
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
		current, err := readOrganizationProjectRank(ctx, tx, scope.organization)
		if err != nil {
			return nil, err
		}
		if current.Revision != change.ExpectedRevision {
			return nil, nativeConflict(tracker.Revision(current.Revision))
		}
		if len(change.ProjectIDs) != len(current.ProjectIDs) {
			return nil, nativeInvalid("Rank must include every organization project once")
		}
		allowed := map[tracker.ProjectID]bool{}
		for _, id := range current.ProjectIDs {
			allowed[id] = true
		}
		for rank, id := range change.ProjectIDs {
			if !allowed[id] {
				return nil, nativeInvalid("Rank must include every organization project once")
			}
			delete(allowed, id)
			if _, err := tx.ExecContext(ctx, "UPDATE projects SET scheduling_rank = ? WHERE organization_id = ? AND id = ?", rank, scope.organization, id); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE organizations SET scheduling_revision = scheduling_revision + 1 WHERE id = ?", scope.organization); err != nil {
			return nil, err
		}
		return readOrganizationProjectRank(ctx, tx, scope.organization)
	})
}
