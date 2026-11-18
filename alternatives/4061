package threads

import (
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	s.registerUnread(e)
	base := "/api/v2/organizations/:organization/threads"
	for _, route := range []struct{ method, path, name string }{
		{http.MethodGet, base, "list_threads"},
		{http.MethodPost, base, "create_thread"},
		{http.MethodGet, base + "/:thread", "get_thread"},
		{http.MethodPost, base + "/:thread/posts", "post_to_thread"},
		{http.MethodPatch, base + "/:thread", "resolve_thread"},
	} {
		e.Add(route.method, route.path, func(c echo.Context) error {
			var in operatortool.ThreadArguments
			if c.Request().Method == http.MethodGet {
				in = operatortool.ThreadArguments{Scope: c.QueryParam("scope"), ProjectID: c.QueryParam("project_id"), IssueID: c.QueryParam("issue_id"), Status: c.QueryParam("status"), Since: c.QueryParam("since"), Cursor: c.QueryParam("cursor"), Search: c.QueryParam("search")}
				if raw := c.QueryParam("limit"); raw != "" {
					n, err := strconv.Atoi(raw)
					if err != nil {
						return s.deps.Error(c, Invalid())
					}
					in.Limit = n
				}
			} else if err := hubapi.DecodeJSON(c, &in); err != nil {
				return s.deps.Error(c, Invalid())
			}
			in.ThreadID = c.Param("thread")
			value, err := s.Operation(c.Request().Context(), route.name, in)
			if err != nil {
				return s.deps.Error(c, err)
			}
			c.Response().Header().Set("Cache-Control", "no-store")
			status := http.StatusOK
			if route.name == "create_thread" || route.name == "post_to_thread" {
				status = http.StatusCreated
			}
			return c.JSON(status, value)
		}, s.deps.Operator)
	}
}

func (s *HTTPService) Operation(ctx context.Context, name string, in operatortool.ThreadArguments) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if err := CheckArguments(in); err != nil {
		return nil, err
	}
	required := apikey.ScopeRead
	if name != "list_threads" && name != "get_thread" {
		required = apikey.ScopeWrite
	}
	ctx, scope, err := s.Authority(ctx, name, required)
	if err != nil {
		return nil, err
	}
	return s.deps.Service().Operation(ctx, s.caller(scope), name, in)
}

func (s *HTTPService) Authority(ctx context.Context, name string, required apikey.Scope) (context.Context, hubauth.Input, error) {
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: required, Action: name})
	if err != nil {
		return ctx, hubauth.Input{}, err
	}
	scope, err := s.deps.Resolve(ctx)
	if err != nil {
		return ctx, hubauth.Input{}, err
	}
	scope.Action = name
	repository := s.deps.Service().deps.Repository
	if repository == nil {
		return ctx, hubauth.Input{}, operatortool.ErrServiceUnavailable
	}
	if err := s.deps.Auth().AuthorizeProject(ctx, scope, false); err != nil {
		return ctx, hubauth.Input{}, err
	}
	if scope.Credential.Hosted == nil && (scope.Credential.Scope != hubauth.Admin || scope.Credential.NativeOnly) {
		err := repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			grants, err := r.AccessTokenGrants(ctx, scope.Credential.ID, "", 1)
			if err != nil {
				return err
			}
			if len(grants) == 0 {
				return hubapi.NotFound()
			}
			return nil
		})
		if err != nil {
			return ctx, hubauth.Input{}, err
		}
	}
	if scope.Credential.HostedRole == "account" {
		return ctx, hubauth.Input{}, operatortool.ErrAccessDenied
	}
	return ctx, scope, nil
}

func (s *HTTPService) registerUnread(e *echo.Group) {
	e.GET("/api/v2/organizations/:organization/threads/unread", func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
		defer cancel()
		ctx, scope, err := s.Authority(ctx, "list_threads", apikey.ScopeRead)
		if err != nil {
			return s.deps.Error(c, err)
		}
		projects := slices.Clone(c.QueryParams()["project_id"])
		if len(projects) > 100 || c.QueryParam("refresh") != "" && c.QueryParam("refresh") != "true" {
			return s.deps.Error(c, Invalid())
		}
		slices.Sort(projects)
		projects = slices.Compact(projects)
		for _, project := range projects {
			if project == "" || len(project) > 256 {
				return s.deps.Error(c, Invalid())
			}
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, Action: "list_threads", ProjectID: project}); err != nil {
				return s.deps.Error(c, err)
			}
			check := scope
			check.Project = tracker.ProjectID(project)
			if err := s.deps.Auth().AuthorizeProject(ctx, check, false); err != nil {
				return s.deps.Error(c, s.deps.Service().recordError(err))
			}
		}
		result, err := s.deps.Service().RequestUnread(ctx, s.caller(scope), projects, c.QueryParam("refresh") == "true")
		if err != nil {
			return s.deps.Error(c, err)
		}
		if result.Pending && s.deps.NotifyUnread != nil {
			s.deps.NotifyUnread()
		}

		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, result)
	}, s.deps.Operator)
}
