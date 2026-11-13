package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
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
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if scope.credential.Hosted != nil && scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" || scope.credential.Hosted == nil && scope.credential.Scope != hubapi.APIScopeAdmin {
		return s.nativeAPIError(c, &hubapi.NativeError{Code: "forbidden", Message: "Project administration is required", Status: http.StatusForbidden, PublicMessage: true})
	}
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	actor := scope.actor()
	ctx := mutation.WithContext(c.Request().Context(), mutation.Metadata{PrincipalID: actor.PrincipalID, ActorKind: actor.Kind, ViaKey: actor.ViaKey, OrganizationID: string(scope.organization), ProjectID: string(scope.project), Action: c.Request().Method + " " + c.Path(), Source: "api", Confirmation: "none", CorrelationID: hubapi.NewID("api")})
	c.SetRequest(c.Request().WithContext(ctx))
	return s.nativeMutation(c, request.Mutation, request, nil)
}
