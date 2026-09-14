package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

func (d *database) hostedPlanUsage(ctx context.Context, now time.Time) (HostedEntitlement, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return HostedEntitlement{}, err
	}
	defer tx.Rollback()
	entitlement, err := d.hostedEntitlement(ctx, tx, now)
	if err != nil {
		return entitlement, err
	}
	entitlement.Usage, err = d.hostedConsumption(ctx, tx, now)
	delete(entitlement.Usage, "events_total")
	return entitlement, errors.Join(err, tx.Commit())
}

// hostedPlanReport answers GET /plan for an owner or admin: the entitlement,
// its allowances with consumption, complimentary grants and the usage window
// (decisions section 12).
func (s *Service) hostedPlanReport(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "Organization plan and usage require owner or admin access")
	}
	entitlement, err := s.database.hostedPlanUsage(c.Request().Context(), s.config.now())
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Plan information is temporarily unavailable")
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, entitlement)
}
