package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/labstack/echo/v4"
)

func (s *Service) recordHostedRequest(c echo.Context, started time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request().Context()), 2*time.Second)
	defer cancel()
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		s.config.Logger.Warn("hosted request metrics unavailable")
		return
	}
	defer tx.Rollback()
	now := s.config.now()
	for _, sample := range []struct {
		metric string
		amount int64
	}{
		{"http_requests", 1},
		{"http_duration_microseconds", max(int64(0), time.Since(started).Microseconds())},
		{"http_response_bytes", max(int64(0), c.Response().Size)},
	} {
		if err := s.database.recordHostedUsage(ctx, tx, now, sample.metric, sample.amount); err != nil {
			s.config.Logger.Warn("hosted request metrics unavailable")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.config.Logger.Warn("hosted request metrics unavailable")
	}
}

func (s *Service) reserveHostedInvitation(ctx context.Context, email string) error {
	_, err := s.reserveHostedInvitationSeat(ctx, email)
	return err
}

// reserveHostedInvitationSeat holds a member seat for the invited address and
// reports whether this call created the reservation. A reservation that was
// already held belongs to an earlier pending invitation, so a failed attempt
// must leave it in place.
func (s *Service) reserveHostedInvitationSeat(ctx context.Context, email string) (bool, error) {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_member_reservations(email,expires_at) SELECT ?,? WHERE NOT EXISTS(SELECT 1 FROM hosted_members WHERE email = ? AND active = 1) ON CONFLICT(email) DO UPDATE SET expires_at = excluded.expires_at`, email, now.Add(time.Duration(s.database.hostedPlans.InvitationSeconds)*time.Second).Unix(), email); err != nil {
		return false, err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return false, err
	}
	return held == 0 && active == 0, tx.Commit()
}

func (s *Service) releaseHostedInvitation(ctx context.Context, email string) error {
	_, err := s.database.db.ExecContext(ctx, "DELETE FROM hosted_member_reservations WHERE email = ?", email)
	return err
}

func (s *Service) hostedInvitationFailure(c echo.Context, email string, reserved bool, cause error) error {
	s.releaseFailedHostedInvitation(c, email, reserved, cause)
	return s.hostedError(c, 503, "The invitation could not be sent")
}

// releaseFailedHostedInvitation gives back the seat a failed attempt reserved.
// A seat the attempt found already held stays with the earlier invitation.
func (s *Service) releaseFailedHostedInvitation(c echo.Context, email string, reserved bool, cause error) {
	if !reserved {
		if cause != nil {
			s.config.Logger.Warn("hosted invitation failed")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request().Context()), 2*time.Second)
	defer cancel()
	if err := errors.Join(cause, s.releaseHostedInvitation(ctx, email)); err != nil {
		s.config.Logger.Warn("hosted invitation failed")
	}
}
