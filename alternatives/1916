package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type organizationNameSettings struct {
	Name string `json:"name"`
}

func (s *Service) hostedOrganizationName(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var result organizationNameSettings
	if err := s.database.reader.QueryRowContext(ctx, "SELECT name FROM organizations WHERE id=?", scope.organization).Scan(&result.Name); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) renameHostedOrganization(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if !canManageProjectSecrets(scope.credential) {
		return s.hostedJSONError(c, http.StatusForbidden, "Organization settings require owner or admin access")
	}
	var request struct {
		tracker.Mutation
		organizationNameSettings
	}
	if decodeAPIJSON(c, &request) != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 120 {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, _ time.Time) (any, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE organizations SET name=? WHERE id=?", request.Name, scope.organization); err != nil {
			return nil, err
		}
		if metadata, ok := mutation.FromContext(ctx); !ok || metadata.Source != "api" && metadata.Source != "mcp" {
			if err := s.hostedAuditWith(ctx, tx, scope.credential.Hosted, "action", "PUT "+c.Path(), "", http.StatusOK); err != nil {
				return nil, err
			}
		}
		return request.organizationNameSettings, nil
	})
}
