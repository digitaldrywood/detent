package hubserver

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Hosted != nil && scope.Credential.HostedRole != "owner" && scope.Credential.HostedRole != "admin" || scope.Credential.Hosted == nil && scope.Credential.Scope != hubapi.APIScopeAdmin {
		return s.nativeAPIError(c, &hubapi.NativeError{Code: "forbidden", Message: "Project administration is required", Status: http.StatusForbidden, PublicMessage: true})
	}
	scope.RequireHostedAdmin = true
	c.Set("native_scope", scope)
	actor := scope.Actor()
	ctx := mutation.WithContext(c.Request().Context(), mutation.Metadata{PrincipalID: actor.PrincipalID, ActorKind: actor.Kind, ViaKey: actor.ViaKey, OrganizationID: string(scope.Organization), ProjectID: string(scope.Project), Action: c.Request().Method + " " + c.Path(), Source: "api", Confirmation: "none", CorrelationID: hubapi.NewID("api")})
	c.SetRequest(c.Request().WithContext(ctx))
	return s.nativeMutation(c, request.Mutation, request, nil)
}
