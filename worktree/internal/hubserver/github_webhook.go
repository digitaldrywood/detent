package hubserver

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const githubWebhookMaxBodyBytes = 2 << 20

type webhookReceipt struct {
	DeliveryID       string
	EventType        string
	Action           string
	HeadersJSON      string
	Payload          []byte
	PayloadSHA256    string
	ReceivedAt       time.Time
	PayloadExpiresAt time.Time
}

type webhookReceiptResult struct {
	InboxID   int64
	Duplicate bool
}

type webhookResponse struct {
	DeliveryID string `json:"delivery_id"`
	Duplicate  bool   `json:"duplicate"`
}

type webhookErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Service) githubWebhook(c echo.Context) error {
	return s.receiveGitHubWebhook(c, false)
}

func (s *Service) hostedGitHubWebhook(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
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
	return c.JSON(http.StatusGone, webhookErrorResponse{Code: "github_webhook_retired", Message: "GitHub webhook ingestion is retired"})
}
