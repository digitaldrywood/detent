package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

type operatorEntryOriginKey struct{}

func operatorRequestCredential(c echo.Context) (hubapi.APICredential, error) {
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok || credential.ID == "" {
		return hubapi.APICredential{}, operatortool.ErrAccessDenied
	}
	return credential, nil
}

type operatorAuthorizationFailure struct {
	cause error
	kind  error
}

func (e *operatorAuthorizationFailure) Error() string { return e.kind.Error() }
func (e *operatorAuthorizationFailure) Unwrap() error { return e.kind }
func (e *operatorAuthorizationFailure) Is(target error) bool {
	return errors.Is(e.kind, target) || target == tenantauth.ErrUnavailable && (errors.Is(e.kind, operatortool.ErrServiceUnavailable) || errors.Is(e.kind, operatortool.ErrProviderUnavailable))
}

func operatorAuthorizationError(err error) error {
	var failure *operatorAuthorizationFailure
	if errors.As(err, &failure) {
		return failure
	}
	var native *hubapi.NativeError
	if errors.As(err, &native) {
		if native.Status == http.StatusForbidden || native.Status == http.StatusNotFound {
			return operatortool.ErrAccessDenied
		}
	}
	var denied *operatortool.AuthorizationError
	if errors.As(err, &denied) && errors.As(denied.Cause, &native) {
		return operatorAuthorizationError(denied.Cause)
	}
	classified := hubauth.Classify(err)
	var moved *hubauth.AuthorizationFailure
	if errors.As(classified, &moved) {
		return &operatorAuthorizationFailure{cause: moved.Cause, kind: moved.Kind}
	}
	return classified
}

func (s *Service) registerOperatorTools(e *echo.Echo) {
	// Hubs expose native work commands and hosted billing/usage operations.
	s.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{s}, chat.WithClock(s.config.now), chat.WithSessionStore(operatorChatStore{s.database}, s.resolveOperatorChatAuthority))
	s.administration = s.operatorAdministration()
	s.administration.Chat = s.operatorChat
	executor := hostedOperatorExecutor{s}
	s.mcpHTTP = mcp.NewHTTPHandler(executor, s.config.Version, mcp.HTTPConfig{
		Logger: s.config.Logger,
		Principal: func(request *http.Request) operatortool.Identity {
			return operatortool.ConnectionIdentity(request.Context())
		},
	})
	e.Any("/api/v2/organizations/:organization/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	if s.config.Hosted != nil {
		e.Any("/mcp", echo.WrapHandler(s.mcpHTTP), s.operatorAuthority)
	}
}

type operatorAccessTimeKey struct{}

func operatorAccessTime(ctx context.Context, now time.Time) time.Time {
	if accepted, ok := ctx.Value(operatorAccessTimeKey{}).(time.Time); ok {
		return accepted
	}
	return now
}

func (s *Service) operatorAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return s.requestAuth().Operator(s.operatorAuthOptions())(next)
}

func operatorIdentity(credential hubapi.APICredential, organization string) operatortool.Identity {
	return hubauth.Identity(authCredential(credential), organization)
}

func (s *Service) operatorCurrentAuthority(ctx context.Context, credential hubapi.APICredential, organization string) (operatortool.Authority, error) {
	return s.requestAuth().CurrentAuthority(ctx, authCredential(credential), organization, s.operatorCheckOptions())
}
