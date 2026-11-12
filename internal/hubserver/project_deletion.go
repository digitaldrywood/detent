package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type deleteCloudProjectRequest struct {
	tracker.Mutation
	ConfirmName string `json:"confirm_name"`
}

func (s *Service) deleteCloudProject(c echo.Context) error {
	scope := nativeRequestScope(c)
	if s.config.Hosted == nil || scope.credential.Hosted == nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	var request deleteCloudProjectRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutationStatus(c, http.StatusOK, request.Mutation, request, nil)
}
