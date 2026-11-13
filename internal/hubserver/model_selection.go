package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cloudModelSelection = projectsettings.ModelSelection

func (s *Service) getCloudModelSelection(c echo.Context) error {
	scope := nativeRequestScope(c)
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	if s.config.AccessRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if err := s.accessRead(c.Request().Context(), scope.organization, func(r tenantstore.Reader) error {
		return s.authorizeAccessScope(c.Request().Context(), r, scope, false)
	}); err != nil {
		return s.nativeAPIError(c, err)
	}
	if scope.project == "" {
		value, err := s.readOrganizationModels(c.Request().Context(), scope.organization)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, value)
	}
	record, err := s.readProjectRecord(c.Request().Context(), scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.projectRecordModel(c.Request().Context(), record)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) organizationModelSelectionScope(c echo.Context) (nativeScope, error) {
	if s.config.Hosted == nil {
		credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
		if !ok || credential.NativeOnly || credential.Runner.RunnerID != "" {
			return nativeScope{}, operatortool.ErrAccessDenied
		}
		return nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, nil
	}
	credential, _, err := s.appCredential(c)
	return nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), credential: credential, requireHostedAdmin: true}, err
}

func (s *Service) getOrganizationModelSelection(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	c.Set("native_scope", scope)
	return s.getCloudModelSelection(c)
}

func (s *Service) updateOrganizationModelSelection(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	c.Set("native_scope", scope)
	return s.updateCloudModelSelection(c)
}

func (s *Service) updateCloudModelSelection(c echo.Context) error {
	var request cloudModelSelectionRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	return s.nativeMutation(c, request.Mutation, request, nil)
}
