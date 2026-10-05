package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cloudModelSelection struct {
	Revision  tracker.Revision       `json:"revision,string"`
	Selection *config.ModelSelection `json:"selection"`
	Effective config.ModelSelection  `json:"effective"`
}

type cloudModelSelectionRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision       `json:"expected_revision,string"`
	Selection        *config.ModelSelection `json:"selection"`
}

func readCloudModelSelection(ctx context.Context, query nativeQueryer, scope nativeScope) (cloudModelSelection, error) {
	var result cloudModelSelection
	var organization string
	var org config.ModelSelection
	if err := query.QueryRowContext(ctx, "SELECT model_selection_json, model_selection_revision FROM organizations WHERE id=?", scope.organization).Scan(&organization, &result.Revision); err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(organization), &org); err != nil {
		return result, err
	}
	result.Selection = &org
	var override config.ModelSelection
	if scope.project != "" {
		var raw sql.NullString
		if err := query.QueryRowContext(ctx, "SELECT model_selection_json, model_selection_revision FROM projects WHERE organization_id=? AND id=?", scope.organization, scope.project).Scan(&raw, &result.Revision); err != nil {
			return result, err
		}
		result.Selection = nil
		if raw.Valid {
			if err := json.Unmarshal([]byte(raw.String), &override); err != nil {
				return result, err
			}
			result.Selection = &override
		}
	}
	result.Effective = config.ResolveCloudModelSelection(org, override)
	return result, nil
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
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, _ time.Time) (any, error) {
		current, err := readCloudModelSelection(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		if scope.project == "" && request.Selection == nil {
			return nil, nativeInvalid("Organization model selection is required")
		}
		var raw any
		if request.Selection != nil {
			encoded, err := json.Marshal(request.Selection)
			if err != nil {
				return nil, err
			}
			raw = string(encoded)
		}
		if scope.project == "" {
			_, err = tx.ExecContext(ctx, "UPDATE organizations SET model_selection_json=?, model_selection_revision=model_selection_revision+1 WHERE id=?", raw, scope.organization)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE projects SET model_selection_json=?, model_selection_revision=model_selection_revision+1 WHERE organization_id=? AND id=?", raw, scope.organization, scope.project)
		}
		if err != nil {
			return nil, err
		}
		saved, err := readCloudModelSelection(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if problems := saved.Effective.Validate(); len(problems) > 0 {
			return nil, &nativeError{Code: "invalid_request", Message: strings.Join(problems, "; "), status: http.StatusUnprocessableEntity, publicMessage: true}
		}
		return saved, nil
	})
}
