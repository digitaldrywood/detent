package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// registerConversationAPIRoutes mounts the operator-facing conversation API
// (the conversation contract). Hosted sessions authenticate as operator credentials.
//
// The project routes use requireConversationScope rather than
// requireNativeScope: the native middleware turns a hosted read-only grant
// into an opaque 404 on every mutation, while the conversation contract
// distinguishes "cannot see" (404) from "can read but cannot control" (403).
// Write capability is therefore checked per handler by authorizeWrite.

// requireConversationScope authenticates an operator or admin credential
// for a project route and requires read access to the project. It mirrors
// requireNativeScope except that hosted write capability is left to the
// handlers so that a read-only member receives 403 instead of 404. Extra
// scopes widen the credential set; the per-handler authorization is
// unchanged, so a wider route is still bounded by the conversation's own
// read rule.
func (s *Service) requireConversationScope(extra ...hubapi.APIScope) echo.MiddlewareFunc {
	allowed := append([]hubapi.APIScope{hubapi.APIScopeOperator, hubapi.APIScopeAdmin}, extra...)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireAPIScope(allowed...)(func(c echo.Context) error {
			credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
			if !ok {
				return s.nativeAPIError(c, hubapi.NativeNotFound())
			}
			ctx := c.Request().Context()
			scope := hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project")), Credential: credential}

			if err := s.authorizeProjectRecord(ctx, scope, false); err != nil {
				if c.Param("conversation") != "" {
					var denied *hubapi.NativeError
					if errors.Is(err, accessstore.ErrDenied) || errors.As(err, &denied) && denied.Status == http.StatusForbidden {
						return s.nativeAPIError(c, hubapi.NativeNotFound())
					}
				}
				return s.nativeAPIError(c, err)
			}
			if err := s.requestAuth().RequireActiveProject(ctx, s.authInput(scope)); err != nil {
				return s.nativeAPIError(c, hubapi.FromFeatureError(err))
			}
			if item := c.Param("item"); item != "" {
				if _, _, err := s.readWorkItemRecord(ctx, scope, item); err != nil {
					return s.nativeAPIError(c, err)
				}
			}

			c.Set("native_scope", scope)
			if c.Request().Method == http.MethodGet {
				c.SetRequest(c.Request().WithContext(s.archiveRequestContext(c, scope)))
			}
			c.Response().Header().Set("Cache-Control", "no-store")
			if credential.Hosted != nil {
				if err := s.hostedAudit(ctx, credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.Project), 0); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			return next(c)
		})
	}
}

// requireConversationOrganization authenticates an operator or admin
// credential for the organization-level listing, which has no :project.
// requireAPIScope cannot be reused: in hosted mode it rejects session
// credentials outside the native project base.
func (s *Service) requireConversationOrganization() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.authenticateAPIRequest(c)
			if err != nil {
				return s.refusedAPIError(c, status, "unauthorized", "Valid scoped API token is required", hubapi.APIErrorResponse{Code: "unauthorized", Message: "Valid scoped API token is required"})
			}
			if s.config.Hosted != nil && credential.Hosted == nil {
				return s.nativeAPIError(c, hubapi.NativeNotFound())
			}
			if credential.Scope != hubapi.APIScopeOperator && credential.Scope != hubapi.APIScopeAdmin {
				return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "API token scope does not permit this operation", hubapi.APIErrorResponse{Code: "insufficient_scope", Message: "API token scope does not permit this operation"})
			}
			ctx := c.Request().Context()
			scope := hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}
			if err := s.authorizeConversationOrganization(ctx, scope); err != nil {
				return s.nativeAPIError(c, err)
			}
			c.Set("hub_api_credential", credential)
			c.Set("native_scope", scope)
			c.Response().Header().Set("Cache-Control", "no-store")
			if credential.Hosted != nil {
				if err := s.hostedAudit(ctx, credential.Hosted, "action", c.Request().Method+" "+c.Path(), "", 0); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			return next(c)
		}
	}
}

func (s *Service) authorizeConversationOrganization(ctx context.Context, scope hubscope.NativeScope) error {
	return hubapi.FromFeatureError(s.requestAuth().AuthorizeOrganization(ctx, s.authInput(scope)))
}

func (s *Service) registerConversationRoutes(e *echo.Echo) {
	if s.conversations == nil {
		return
	}
	conversations.RegisterRoutes(e.Group(""), s.conversationHTTPDeps())
}

func (s *Service) conversationHTTPDeps() conversations.HTTPDeps {
	return conversations.HTTPDeps{
		Service:         func() *conversationService { return s.conversations },
		Scope:           s.requireConversationScope,
		Organization:    s.requireConversationOrganization(),
		Operator:        s.operatorAuthority,
		Worker:          s.requireNativeScope(hubapi.APIScopeWorker),
		Action:          s.postConversationAction,
		Error:           func(c echo.Context, err error) error { return s.nativeAPIError(c, err) },
		InvalidRequest:  s.invalidAPIRequest,
		RefreshStream:   s.refreshConversationStream,
		StreamHeartbeat: s.streamHeartbeat,
		StreamAuthorize: s.streamAuthorize,
	}
}

func (s *Service) refreshConversationStream(readCtx context.Context, c echo.Context, scope *hubscope.NativeScope) error {
	if scope.Credential.EntryKey != nil {
		claims, ok := hostedSharedClaims(c)
		if !ok || claims.Key == nil {
			return auth.ErrHostedIdentity
		}
		encoded, err := json.Marshal(entryauthority.Origin{KeyID: claims.Key.ID, CredentialBinding: claims.Key.CredentialBinding, Subject: claims.Subject, Issuer: claims.Issuer, Generation: claims.Generation})
		if err != nil {
			return err
		}
		credential, err := s.currentEntryOriginCredential(readCtx, string(encoded))
		if err != nil {
			return err
		}
		scope.Credential = credential
	} else if scope.Credential.Hosted != nil {
		credential, _, err := s.authenticateAPIRequestContext(readCtx, c)
		if err != nil {
			return err
		}
		scope.Credential = credential
	}

	if err := s.authorizeProjectRecord(readCtx, *scope, false); err != nil {
		return err
	}
	return hubapi.FromFeatureError(s.requestAuth().RequireActiveProject(readCtx, s.authInput(*scope)))
}

func (s *Service) streamHeartbeat() time.Duration {
	if s.config.deps.ConversationStreamHeartbeat > 0 {
		return s.config.deps.ConversationStreamHeartbeat
	}
	return conversationHeartbeatInterval
}

func (s *Service) streamAuthorize() time.Duration {
	if s.config.deps.ConversationStreamAuthorize > 0 {
		return s.config.deps.ConversationStreamAuthorize
	}
	return conversationAuthorizeInterval
}
