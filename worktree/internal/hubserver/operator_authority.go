package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func operatorRequestCredential(c echo.Context) (hubapi.APICredential, error) {
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok || credential.ID == "" {
		return hubapi.APICredential{}, operatortool.ErrAccessDenied
	}
	return credential, nil
}

func (s *Service) registerOperatorTools(e *echo.Echo) {
	// Hubs expose native work commands and hosted billing/usage operations.
	s.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{s}, chat.WithClock(s.config.Clock), chat.WithSessionStore(operatorChatStore{s.database}, s.resolveOperatorChatAuthority))
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

func (s *Service) operatorCurrentAuthority(ctx context.Context, credential hubapi.APICredential, organization string) (operatortool.Authority, error) {
	return s.requestAuth().CurrentAuthority(ctx, hubscope.AuthCredential(credential), organization, s.operatorCheckOptions())
}
