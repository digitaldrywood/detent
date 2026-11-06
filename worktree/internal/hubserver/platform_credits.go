package hubserver

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) platformTenantAICreditBalance(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.hostedJSONError(c, http.StatusForbidden, "Shared service access is required to read AI credits")
	}
	d := s.database
	var result struct {
		BalanceMicros int64                 `json:"balance_micros"`
		SpentMicros   int64                 `json:"spent_micros"`
		Month         string                `json:"month"`
		AutoEnabled   bool                  `json:"auto_enabled"`
		History       []aiCreditTransaction `json:"history"`
	}
	var creditRows []billingstore.CreditTransaction
	now := d.now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	result.Month = start.Format("2006-01")
	result.History = []aiCreditTransaction{}

	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		account, err := reader.BillingCreditAccount(ctx, d.aiCreditMode)
		if err != nil {
			return err
		}
		result.BalanceMicros, result.AutoEnabled = account.BalanceMicros, account.AutoEnabled
		result.SpentMicros, err = reader.BillingCreditUsage(ctx, d.aiCreditMode, start, start.AddDate(0, 1, 0))
		if err != nil {
			return err
		}
		rows, err := reader.BillingRecentCreditTransactions(ctx, d.aiCreditMode, 50)
		if err != nil {
			return err
		}
		creditRows = rows
		return nil
	})
	if err != nil {
		return s.internalAPIError(c, "ai_credits_unavailable", "AI credits are unavailable", err)
	}
	for _, row := range creditRows {
		reason, err := d.readBillingReason(ctx, row.ReasonReference)
		if err != nil {
			return s.internalAPIError(c, "ai_credits_unavailable", "AI credits are unavailable", err)
		}
		result.History = append(result.History, aiCreditTransaction{AmountMicros: row.AmountMicros, Kind: row.Kind, Actor: row.Actor, Reason: reason, At: row.RecordedAt.UTC()})
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) adjustPlatformAICredits(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Subject == "" || claims.Email == "" {
		return s.hostedJSONError(c, http.StatusForbidden, "Shared service identity and email are required to adjust AI credits")
	}
	var adjustment billing.CreditAdjustment
	if err := decodeAPIJSON(c, &adjustment); err != nil {
		return s.invalidAPIRequest(c, err)
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
		return 0, nativePublicInvalid("A command ID, actor and reason of at most 500 characters are required")
	}
	if d.aiCreditMode == "" {
		return 0, nativePublicInvalid("AI credits are not configured for this organization")
	}

	reference, err := d.archiveBillingReason(ctx, adjustment.Reason)
	if err != nil {
		return 0, err
	}
	var balance int64
	err = d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		account, err := writer.BillingCreditAccount(ctx, d.aiCreditMode)
		if err != nil {
			return err
		}
		balance = account.BalanceMicros
		source := "adjustment:" + adjustment.IdempotencyKey
		previous, err := writer.BillingCreditTransactionBySource(ctx, d.aiCreditMode, source)
		if err == nil {
			if previous.AmountMicros != amount || previous.Actor != actor || previous.ReasonReference.SHA256 != reference.SHA256 {
				return &nativeError{status: http.StatusConflict, Code: "idempotency_conflict", Message: "That idempotency key was used for a different credit adjustment", publicMessage: true}
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if amount > 0 && balance > math.MaxInt64-amount || amount < 0 && balance < math.MinInt64-amount {
			return nativePublicInvalid("The adjusted balance is out of range")
		}
		_, err = writer.PostBillingCredit(ctx, billingstore.CreditTransaction{OrganizationID: d.hostedOrganization, Mode: d.aiCreditMode, Source: source, AmountMicros: amount, Kind: "complimentary", Actor: actor, ReasonReference: reference, RecordedAt: d.now().UTC()})
		if err == nil {
			balance += amount
		}
		return err
	})
	return balance, err
}
