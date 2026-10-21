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

type renameProjectRequest struct {
	tracker.Mutation
	Name             string           `json:"name"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func (s *Service) renameProject(c echo.Context) error {
	var request renameProjectRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if scope.credential.Hosted != nil && scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" {
		return s.nativeAPIError(c, &nativeError{Code: "forbidden", Message: "Project administration is required", status: http.StatusForbidden, publicMessage: true})
	}
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	actor := scope.actor()
	ctx := mutation.WithContext(c.Request().Context(), mutation.Metadata{PrincipalID: actor.PrincipalID, ActorKind: actor.Kind, ViaKey: actor.ViaKey, OrganizationID: string(scope.organization), ProjectID: string(scope.project), Action: c.Request().Method + " " + c.Path(), Source: "api", Confirmation: "none", CorrelationID: newNativeID("api")})
	c.SetRequest(c.Request().WithContext(ctx))
	return s.nativeMutation(c, request.Mutation, request, s.renameProjectOperation(request))
}

func (s *Service) renameProjectOperation(request renameProjectRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, _ time.Time) (any, error) {
		current, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if scope.credential.Hosted == nil && scope.credential.Scope != apiScopeAdmin {
			return nil, &nativeError{Code: "forbidden", Message: "Project administration is required", status: http.StatusForbidden, publicMessage: true}
		}
		if current.Profile != "native" {
			return nil, nativeInvalid("Project name is externally owned")
		}
		name := strings.TrimSpace(request.Name)
		if name == "" || len(name) > 200 || request.ExpectedRevision <= 0 {
			return nil, &nativeError{Code: "invalid_request", Message: "A project name of at most 200 bytes and a positive expected revision are required", status: http.StatusBadRequest, publicMessage: true}
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET name=?, integration_revision=integration_revision+1 WHERE organization_id=? AND id=? AND deleted_at IS NULL", name, scope.organization, scope.project); err != nil {
			return nil, err
		}
		return readProjectIntegration(ctx, tx, scope)
	}
}
