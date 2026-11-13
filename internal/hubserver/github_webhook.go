package hubserver

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func (s *Service) githubWebhook(c echo.Context) error {
	return s.receiveGitHubWebhook(c, false)
}

func (s *Service) hostedGitHubWebhook(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	for header, query := range map[string]string{
		"X-GitHub-Delivery": "delivery_id", "X-GitHub-Event": "event_type",
		"X-GitHub-Hook-ID": "hook_id", "X-GitHub-Hook-Installation-Target-ID": "installation_target", "User-Agent": "user_agent",
	} {
		c.Request().Header.Set(header, c.QueryParam(query))
	}
	return s.receiveGitHubWebhook(c, true)
}

func (s *Service) receiveGitHubWebhook(c echo.Context, verifiedByEntry bool) error {
	return s.receiveGitHubWebhookRecord(c, verifiedByEntry)
}
