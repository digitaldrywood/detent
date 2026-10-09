package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func (s *Service) adjustPlatformAICredits(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Subject == "" || claims.Email == "" {
		return c.NoContent(http.StatusForbidden)
	}
	var adjustment billing.CreditAdjustment
	if err := decodeAPIJSON(c, &adjustment); err != nil {
		return invalidAPIRequest(c, err)
	}
	balance, err := s.database.adjustAICredits(c.Request().Context(), claims.Email, adjustment)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"organization_id": s.database.hostedOrganization, "balance_micros": balance})
}

func (d *database) adjustAICredits(ctx context.Context, actor string, adjustment billing.CreditAdjustment) (int64, error) {
	amount, err := adjustment.AmountMicros()
	if err != nil {
		return 0, nativeInvalid(err.Error())
	}
	adjustment.Reason = strings.TrimSpace(adjustment.Reason)
	if !hostedSafeID(adjustment.IdempotencyKey) || adjustment.Reason == "" || len(adjustment.Reason) > 500 || actor == "" || len(actor) > 254 {
		return 0, nativeInvalid("A command ID, actor and reason of at most 500 characters are required")
	}
	if d.aiCreditMode == "" {
		return 0, nativeInvalid("AI credits are not configured for this organization")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var balance int64
	if err := tx.QueryRowContext(ctx, "SELECT balance_micros FROM ai_credit_accounts WHERE organization_id=? AND mode=?", d.hostedOrganization, d.aiCreditMode).Scan(&balance); err != nil {
		return 0, err
	}
	source := "adjustment:" + adjustment.IdempotencyKey
	var previousAmount int64
	var previousActor, previousReason string
	err = tx.QueryRowContext(ctx, "SELECT amount_micros,actor,reason FROM ai_credit_transactions WHERE organization_id=? AND mode=? AND source=?", d.hostedOrganization, d.aiCreditMode, source).Scan(&previousAmount, &previousActor, &previousReason)
	if err == nil {
		if previousAmount != amount || previousActor != actor || previousReason != adjustment.Reason {
			return 0, &nativeError{status: http.StatusConflict, Code: "idempotency_conflict", Message: "That idempotency key was used for a different credit adjustment"}
		}
		return balance, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if amount > 0 && balance > math.MaxInt64-amount || amount < 0 && balance < math.MinInt64-amount {
		return 0, nativeInvalid("The adjusted balance is out of range")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,actor,reason,recorded_at) VALUES(?,?,?,?,'complimentary',?,?,?)", d.hostedOrganization, d.aiCreditMode, source, amount, actor, adjustment.Reason, d.now().UnixMicro()); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE ai_credit_accounts SET balance_micros=balance_micros+? WHERE organization_id=? AND mode=?", amount, d.hostedOrganization, d.aiCreditMode); err != nil {
		return 0, err
	}
	return balance + amount, tx.Commit()
}
