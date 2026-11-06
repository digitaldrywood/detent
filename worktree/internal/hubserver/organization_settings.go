package hubserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type renameOrganizationRequest struct {
	tracker.Mutation
	organizationNameSettings
}

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
	scope.requireHostedAdmin = false
	var name string
	err = s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		row, err := r.AccessOrganization(ctx)
		name = row.Name
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	return c.JSON(http.StatusOK, organizationNameSettings{Name: name})
}

func (s *Service) renameHostedOrganization(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if !canManageProjectSecrets(scope.credential) {
		return s.hostedJSONError(c, http.StatusForbidden, "Organization settings require owner or admin access")
	}
	var request renameOrganizationRequest
	if decodeAPIJSON(c, &request) != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 120 {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter an organization name of at most 120 bytes")
	}
	scope.requireHostedAdmin = true
	raw, err := s.renameOrganizationRecord(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}
