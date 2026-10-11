package hubserver

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

func (s *Service) hostedGitHubRepositories(c echo.Context) error {
	rows, err := s.database.db.QueryContext(c.Request().Context(), `
SELECT lower(r.github_owner || '/' || r.github_name) FROM projects p
JOIN repositories r ON r.id = p.repository_id WHERE p.deleted_at IS NULL AND p.profile = 'native' AND p.organization_id = ?
UNION
SELECT lower(checkout_repository) FROM projects
WHERE deleted_at IS NULL AND profile = 'native' AND organization_id = ? AND checkout_repository != ''`, s.config.Hosted.OrganizationID, s.config.Hosted.OrganizationID)
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

func (s *Service) hostedGitHubReceipt(c echo.Context) error {
	var received sql.NullString
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT max(last_received_at) FROM github_webhook_inbox").Scan(&received); err != nil {
		return s.internalAPIError(c, "tenant_unavailable", "GitHub webhook receipt is unavailable", err)
	}
	var result struct {
		ReceivedAt *time.Time `json:"received_at"`
	}
	if received.Valid {
		value, err := time.Parse(time.RFC3339Nano, received.String)
		if err != nil {
			return err
		}
		result.ReceivedAt = &value
	}
	return c.JSON(http.StatusOK, result)
}
