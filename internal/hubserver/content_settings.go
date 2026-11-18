package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cloudContentSettingsRequest struct {
	tracker.Mutation
	projectsettings.ContentChange
}

func (s *Service) getCloudContentSettings(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	record, err := s.readProjectMetadata(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, projectsettings.Content{Revision: tracker.Revision(record.Core.ContentSettingsRevision), RequireVisualAttachments: record.Settings.RequireVisualAttachments})
}

func (s *Service) updateCloudContentSettings(c echo.Context) error {
	var request cloudContentSettingsRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	scope.RequireHostedAdmin = true
	c.Set("native_scope", scope)
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	options := hubapi.NativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Feature: hostedMutationFeature(c)}
	raw, err := s.executeProjectRecordMutation(c.Request().Context(), scope, options, request.Mutation, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}
