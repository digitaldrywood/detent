package projectadmin

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RenameRequest struct {
	tracker.Mutation
	Name             string           `json:"name"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

type RenameDeps struct {
	InvalidRequest func(echo.Context, error) error
	APIError       func(echo.Context, error, ...any) error
	Mutate         func(echo.Context, RenameRequest) error
}

func Rename(c echo.Context, deps RenameDeps) error {
	var request RenameRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return deps.InvalidRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Hosted != nil && scope.Credential.HostedRole != "owner" && scope.Credential.HostedRole != "admin" || scope.Credential.Hosted == nil && scope.Credential.Scope != hubapi.APIScopeAdmin {
		return deps.APIError(c, &hubapi.NativeError{Code: "forbidden", Message: "Project administration is required", Status: http.StatusForbidden, PublicMessage: true})
	}
	scope.RequireHostedAdmin = true
	c.Set("native_scope", scope)
	actor := scope.Actor()
	ctx := mutation.WithContext(c.Request().Context(), mutation.Metadata{PrincipalID: actor.PrincipalID, ActorKind: actor.Kind, ViaKey: actor.ViaKey, OrganizationID: string(scope.Organization), ProjectID: string(scope.Project), Action: c.Request().Method + " " + c.Path(), Source: "api", Confirmation: "none", CorrelationID: hubapi.NewID("api")})
	c.SetRequest(c.Request().WithContext(ctx))
	return deps.Mutate(c, request)
}
