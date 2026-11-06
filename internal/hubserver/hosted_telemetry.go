package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
)

type hostedRequestUsage struct {
	requests, duration, responseBytes, requestBytes, knownRequests int64
}

const hostedUsageAmountLimit = int64(1) << 50

func saturatingUsageAmount(a, b int64) int64 {
	return min(max(a, 0), hostedUsageAmountLimit) + min(max(b, 0), hostedUsageAmountLimit)
}

func (u *hostedRequestUsage) merge(other hostedRequestUsage) {
	u.requests = min(saturatingUsageAmount(u.requests, other.requests), hostedUsageAmountLimit)
	u.duration = min(saturatingUsageAmount(u.duration, other.duration), hostedUsageAmountLimit)
	u.responseBytes = min(saturatingUsageAmount(u.responseBytes, other.responseBytes), hostedUsageAmountLimit)
	u.requestBytes = min(saturatingUsageAmount(u.requestBytes, other.requestBytes), hostedUsageAmountLimit)
	u.knownRequests = min(saturatingUsageAmount(u.knownRequests, other.knownRequests), hostedUsageAmountLimit)
}

func hostedRequestUsageFor(c echo.Context, elapsed time.Duration) hostedRequestUsage {
	usage := hostedRequestUsage{requests: 1, duration: min(max(int64(0), elapsed.Microseconds()), hostedUsageAmountLimit), responseBytes: min(max(int64(0), c.Response().Size), hostedUsageAmountLimit)}
	if c.Request().ContentLength >= 0 {
		usage.requestBytes, usage.knownRequests = min(c.Request().ContentLength, hostedUsageAmountLimit), 1
	}
	return usage
}

func (s *Service) recordHostedRequest(c echo.Context, started time.Time) {
	m := s.requestMetrics
	if m == nil || explanationRead(c.Request().Context()) {
		return
	}
	credential := apiCredential{}
	if current, ok := c.Get("hub_api_credential").(apiCredential); ok {
		credential = current
	}
	kind, caller := metricCaller(credential)
	if kind == "anonymous" {
		if session, ok := c.Get("hosted_session").(auth.Session); ok && session.Identity != nil {
			kind, caller = "browser", session.Identity.Subject
		}
	}
	now := s.config.now()
	key := requestMetricKey{now.Unix() / 60 * 60, metricRoute(m.knownRoutes(s.echo), c.Request().Method, c.Path()), kind, caller, c.Response().Status / 100}
	usage := hostedRequestUsageFor(c, time.Since(started))
	m.mu.Lock()
	m.usage = s.database
	pending := m.pending[key]
	pending.http.merge(usage)
	m.pending[key] = pending
	m.mu.Unlock()
}

func hostedRequestUsageWindows(plans *HostedPlansConfig, now time.Time, pending map[requestMetricKey]requestLatency) map[int64]hostedRequestUsage {
	windows := make(map[int64]hostedRequestUsage)
	cutoff := now.Unix()/plans.WindowSeconds*plans.WindowSeconds - plans.RetentionWindows*plans.WindowSeconds
	for key, value := range pending {
		window := key.Minute / plans.WindowSeconds * plans.WindowSeconds
		if window <= cutoff || value.http.requests == 0 {
			delete(pending, key)
			continue
		}
		usage := windows[window]
		usage.merge(value.http)
		windows[window] = usage
	}
	return windows
}

func (s *Service) reserveHostedInvitationSeatFor(ctx context.Context, credential apiCredential, email string) (bool, error) {
	return s.reserveAccessInvitationSeat(ctx, credential, email)
}

func (s *Service) releaseHostedInvitation(ctx context.Context, email string) error {

	return s.releaseAccessInvitation(ctx, email)
}
