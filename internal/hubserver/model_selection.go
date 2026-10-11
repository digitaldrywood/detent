package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cloudModelSelection = projectsettings.ModelSelection

type cloudModelSelectionRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision       `json:"expected_revision,string"`
	Selection        *config.ModelSelection `json:"selection"`
}

func readCloudModelSelection(ctx context.Context, query nativeQueryer, scope nativeScope) (cloudModelSelection, error) {
	return projectsettings.ReadModelSelection(ctx, query, scope.organization, scope.project)
}

func (s *Service) getCloudModelSelection(c echo.Context) error {
	result, err := readCloudModelSelection(c.Request().Context(), s.database.db, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) organizationModelSelectionScope(c echo.Context) (nativeScope, error) {
	credential, _, err := s.hostedCredential(c.Request().Context(), c)
	return nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential, requireHostedAdmin: true}, err
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
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, updateCloudModelSelectionOperation(request))
}

func updateCloudModelSelectionOperation(request cloudModelSelectionRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, _ time.Time) (any, error) {
		value, err := projectsettings.UpdateModelSelection(ctx, tx, scope.organization, scope.project, projectsettings.ModelSelectionChange{ExpectedRevision: request.ExpectedRevision, Selection: request.Selection})
		if errors.Is(err, projectsettings.ErrConflict) {
			return nil, nativeConflict(value.Revision)
		}
		if errors.Is(err, projectsettings.ErrInvalid) {
			return nil, &nativeError{Code: "invalid_request", Message: err.Error(), status: http.StatusUnprocessableEntity, publicMessage: true}
		}
		return value, err
	}
}
