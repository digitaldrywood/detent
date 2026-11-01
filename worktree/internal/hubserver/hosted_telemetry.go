package hubserver

import (
	"context"
	"errors"
	"maps"
	"slices"
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

func writeHostedRequestUsage(ctx context.Context, owner *database, now time.Time, pending map[requestMetricKey]requestLatency) error {
	plans := owner.plans()
	if plans == nil {
		clear(pending)
		return nil
	}
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
	ordered := slices.Sorted(maps.Keys(windows))
	for _, window := range ordered[:min(4, len(ordered))] {
		if err := writeHostedRequestWindow(ctx, owner, window, windows[window]); err != nil {
			return err
		}
		for key := range pending {
			if key.Minute/plans.WindowSeconds*plans.WindowSeconds == window {
				delete(pending, key)
			}
		}
	}
	return nil
}

func writeHostedRequestWindow(ctx context.Context, owner *database, window int64, usage hostedRequestUsage) error {
	tx, err := owner.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sample := range []struct {
		metric string
		amount int64
	}{
		{"http_requests", usage.requests},
		{"http_duration_microseconds", usage.duration},
		{"http_response_bytes", usage.responseBytes},
		{"http_request_bytes", usage.requestBytes},
		{"http_request_bytes_known", usage.knownRequests},
	} {
		if err := owner.recordHostedUsage(ctx, tx, time.Unix(window, 0), sample.metric, sample.amount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) reserveHostedInvitation(ctx context.Context, email string) error {
	_, err := s.reserveHostedInvitationSeat(ctx, email)
	return err
}

func (s *Service) reserveHostedInvitationSeatFor(ctx context.Context, credential apiCredential, email string) (bool, error) {
	if s.config.AccessRepository != nil {
		return s.reserveAccessInvitationSeat(ctx, credential, email)
	}
	return s.reserveHostedInvitationSeat(ctx, email)
}

// reserveHostedInvitationSeat holds a member seat for the invited address and
// reports whether this call created the reservation. A reservation that was
// already held belongs to an earlier pending invitation, so a failed attempt
// must leave it in place.
func (s *Service) reserveHostedInvitationSeat(ctx context.Context, email string) (bool, error) {
	runtimePlans := s.database.plans()
	if runtimePlans == nil {
		return false, errors.New("hosted entitlements are disabled")
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := s.config.now()
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_member_reservations WHERE expires_at <= ?", now.Unix()); err != nil {
		return false, err
	}
	var held int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM hosted_member_reservations WHERE email = ?", email).Scan(&held); err != nil {
		return false, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM hosted_members WHERE email = ? AND active = 1", email).Scan(&active); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_member_reservations(email,expires_at) SELECT ?,? WHERE NOT EXISTS(SELECT 1 FROM hosted_members WHERE email = ? AND active = 1) ON CONFLICT(email) DO UPDATE SET expires_at = excluded.expires_at`, email, now.Add(time.Duration(runtimePlans.InvitationSeconds)*time.Second).Unix(), email); err != nil {
		return false, err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return false, err
	}
	return held == 0 && active == 0, tx.Commit()
}

func (s *Service) releaseHostedInvitation(ctx context.Context, email string) error {
	if s.config.AccessRepository != nil {
		return s.releaseAccessInvitation(ctx, email)
	}
	_, err := s.database.db.ExecContext(ctx, "DELETE FROM hosted_member_reservations WHERE email = ?", email)
	return err
}
