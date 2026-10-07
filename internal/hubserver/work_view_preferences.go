package hubserver

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type workViewPreference struct {
	Query *string `json:"query"`
}

func (s *Service) workViewPreference(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{
		organization: tracker.OrganizationID(s.config.Hosted.OrganizationID),
		project:      tracker.ProjectID(c.Param("project")), credential: credential,
	}
	if scope.project != "" {
		if err := s.requireHostedProject(ctx, s.database.db, scope, false); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	value := workViewPreference{}
	switch c.Request().Method {
	case http.MethodGet:
		var query string
		err = s.database.db.QueryRowContext(ctx, `SELECT view_query FROM hosted_work_view_preferences WHERE organization_id = ? AND user_id = ? AND scope = ?`, scope.organization, credential.Hosted.Subject, scope.project).Scan(&query)
		if err == nil {
			value.Query = &query
		} else if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	case http.MethodPut:
		if err := decodeAPIJSON(c, &value); err != nil {
			return invalidAPIRequest(c, err)
		}
		if value.Query == nil || len(*value.Query) > 8192 {
			return s.nativeAPIError(c, nativeInvalid("A view query of at most 8192 bytes is required"))
		}
		if _, err := url.ParseQuery(*value.Query); err != nil {
			return s.nativeAPIError(c, nativeInvalid("The view query is invalid"))
		}
		_, err = s.database.db.ExecContext(ctx, `INSERT INTO hosted_work_view_preferences (organization_id, user_id, scope, view_query) VALUES (?, ?, ?, ?) ON CONFLICT (organization_id, user_id, scope) DO UPDATE SET view_query = excluded.view_query`, scope.organization, credential.Hosted.Subject, scope.project, *value.Query)
	case http.MethodDelete:
		_, err = s.database.db.ExecContext(ctx, `DELETE FROM hosted_work_view_preferences WHERE organization_id = ? AND user_id = ? AND scope = ?`, scope.organization, credential.Hosted.Subject, scope.project)
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}
