package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

func (s *Service) hostedGitHubRepositories(c echo.Context) error {
	rows, err := s.database.db.QueryContext(c.Request().Context(), `
SELECT lower(r.github_owner || '/' || r.github_name) FROM projects p
JOIN repositories r ON r.id = p.repository_id WHERE p.profile = 'native' AND p.organization_id = ?
UNION
SELECT lower(checkout_repository) FROM projects
WHERE profile = 'native' AND organization_id = ? AND checkout_repository != ''`, s.config.Hosted.OrganizationID, s.config.Hosted.OrganizationID)
	if err != nil {
		return s.internalAPIError(c, "tenant_unavailable", "Repository bindings are unavailable", err)
	}
	defer rows.Close()
	repositories := []string{}
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return err
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, repositories)
}
