package conversations

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ContentSettingsRequest struct {
	tracker.Mutation
	projectsettings.ContentChange
}

func (s ContentSettingsHTTP) GetCloudContentSettings(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	if !s.deps.StorageAvailable {
		return s.deps.APIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	record, err := s.deps.ReadProject(ctx, scope)
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSON(http.StatusOK, projectsettings.Content{Revision: tracker.Revision(record.Core.ContentSettingsRevision), RequireVisualAttachments: record.Settings.RequireVisualAttachments})
}

func (s ContentSettingsHTTP) UpdateCloudContentSettings(c echo.Context) error {
	var request ContentSettingsRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	scope.RequireHostedAdmin = true
	c.Set("native_scope", scope)
	if !s.deps.StorageAvailable {
		return s.deps.APIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	options := hubapi.NativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Feature: s.deps.MutationFeature(c)}
	raw, err := s.deps.UpdateProject(c.Request().Context(), scope, options, request)
	if err != nil {
		return s.deps.APIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}
