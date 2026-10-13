package hubserver

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

func (s *Service) renameHostedOrganization(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if !canManageProjectSecrets(scope.credential) {
		return s.hostedJSONError(c, http.StatusForbidden, "Organization settings require owner or admin access")
	}
	var request struct {
		Name string `json:"name"`
	}
	if decodeAPIJSON(c, &request) != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 120 {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	ctx := c.Request().Context()
	if err := s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE organizations SET name=? WHERE id=?", request.Name, scope.organization)
		return err
	}); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.hostedAudit(ctx, scope.credential.Hosted, "action", "PUT "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, request)
}
