package hubserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type bindNativeRepositoryRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	Repository       string           `json:"repository"`
	Source           string           `json:"source,omitempty"`
}

func (s *Service) bindNativeRepository(c echo.Context) error {
	var request bindNativeRepositoryRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.bindNativeRepositoryCommand(c.Request().Context(), nativeRequestScope(c), nativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Feature: hostedMutationFeature(c)}, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
func (s *Service) bindNativeRepositoryCommand(ctx context.Context, scope nativeScope, options nativeCommandOptions, request bindNativeRepositoryRequest) (json.RawMessage, error) {

	if request.Source == "runner_checkout" {
		return s.bindCheckoutRecordCommand(ctx, scope, options, request)
	}
	if _, _, valid := splitRepositoryFullName(request.Repository); !valid {
		return nil, nativePublicInvalid("Repository must be owner/name")
	}
	if request.Source != "" {
		return nil, nativePublicInvalid("Repository source is invalid")
	}
	return nil, nativePublicInvalid("GitHub repository transport is not configured")
}
