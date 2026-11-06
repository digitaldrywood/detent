package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) BillingCreditPurchase(ctx context.Context, key string) (billingstore.CreditPurchase, error) {
	row, err := s.queries.BillingCreditPurchase(ctx, pgquery.BillingCreditPurchaseParams{OrganizationID: s.organization, PurchaseKey: key})
	result := billingstore.CreditPurchase{OrganizationID: row.OrganizationID, Key: row.PurchaseKey, Mode: row.Mode, PriceID: row.PriceID, USDCents: row.UsdCents, Automatic: row.Automatic, PaymentMethod: row.PaymentMethod, State: row.State, PaymentID: row.PaymentID, CreatedAt: row.CreatedAt, Session: billing.Session{ID: row.SessionID, URL: row.SessionUrl}}
	if row.SessionExpiresAt != nil {
		result.Session.ExpiresAt = *row.SessionExpiresAt
	}
	return result, mapError(ctx, err)
}

func (s *session) BillingPendingCreditPurchase(ctx context.Context, mode string, automatic bool) (billingstore.CreditPurchase, error) {
	row := s.db.QueryRowContext(ctx, `SELECT organization_id,purchase_key,mode,price_id,usd_cents,automatic,payment_method,state,payment_id,session_id,session_url,session_expires_at,created_at FROM ai_credit_purchases WHERE organization_id=$1 AND mode=$2 AND automatic=$3 AND state='pending' ORDER BY created_at LIMIT 1`, s.organization, mode, automatic)
	var result billingstore.CreditPurchase
	var sessionExpires *time.Time
	err := row.Scan(&result.OrganizationID, &result.Key, &result.Mode, &result.PriceID, &result.USDCents, &result.Automatic, &result.PaymentMethod, &result.State, &result.PaymentID, &result.Session.ID, &result.Session.URL, &sessionExpires, &result.CreatedAt)
	if sessionExpires != nil {
		result.Session.ExpiresAt = *sessionExpires
	}
	return result, mapError(ctx, err)
}

func (s *session) CreateBillingCreditPurchase(ctx context.Context, row billingstore.CreditPurchase) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ai_credit_purchases(organization_id,purchase_key,mode,price_id,usd_cents,automatic,payment_method,state,payment_id,session_id,session_url,session_expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(organization_id,purchase_key) DO NOTHING`, s.organization, row.Key, row.Mode, row.PriceID, row.USDCents, row.Automatic, row.PaymentMethod, row.State, row.PaymentID, row.Session.ID, row.Session.URL, optionalTime(row.Session.ExpiresAt), row.CreatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	stored, err := s.BillingCreditPurchase(ctx, row.Key)
	if err != nil {
		return err
	}
	if stored.Mode != row.Mode || stored.PriceID != row.PriceID || stored.USDCents != row.USDCents || stored.Automatic != row.Automatic {
		return errors.Join(corestore.ErrUniqueViolation, errors.New("credit purchase key has conflicting terms"))
	}
	return nil
}

func (s *session) SetBillingCreditPurchaseSession(ctx context.Context, key string, session billing.Session) (billing.Session, error) {
	n, err := s.queries.SetBillingCreditPurchaseSession(ctx, pgquery.SetBillingCreditPurchaseSessionParams{OrganizationID: s.organization, PurchaseKey: key, SessionID: session.ID, SessionUrl: session.URL, SessionExpiresAt: optionalTime(session.ExpiresAt)})
	if err != nil {
		return billing.Session{}, mapError(ctx, err)
	}
	if n == 1 {
		return session, nil
	}
	stored, err := s.BillingCreditPurchase(ctx, key)
	if err != nil {
		return billing.Session{}, err
	}
	if stored.Session.URL != "" {
		return stored.Session, nil
	}
	if stored.State != "pending" {
		return session, nil
	}
	return billing.Session{}, corestore.ErrRevisionConflict
}

func (s *session) MarkBillingCreditPurchasePaid(ctx context.Context, key, paymentID string) error {
	n, err := s.queries.MarkBillingCreditPurchasePaid(ctx, pgquery.MarkBillingCreditPurchasePaidParams{OrganizationID: s.organization, PurchaseKey: key, PaymentID: paymentID})
	if err != nil {
		return mapError(ctx, err)
	}
	if n == 1 {
		return nil
	}
	stored, err := s.BillingCreditPurchase(ctx, key)
	if err != nil {
		return err
	}
	if stored.State == "paid" && stored.PaymentID == paymentID {
		return nil
	}
	return corestore.ErrRevisionConflict
}

func (s *session) MarkBillingCreditPurchaseFailed(ctx context.Context, key string) (bool, error) {
	n, err := s.queries.MarkBillingCreditPurchaseFailed(ctx, pgquery.MarkBillingCreditPurchaseFailedParams{OrganizationID: s.organization, PurchaseKey: key})
	if err != nil {
		return false, mapError(ctx, err)
	}
	if n == 1 {
		return true, nil
	}
	stored, err := s.BillingCreditPurchase(ctx, key)
	if err != nil {
		return false, err
	}
	if stored.State == "paid" || stored.State == "failed" {
		return false, nil
	}
	return false, corestore.ErrRevisionConflict
}

func (s *session) BillingUsage(ctx context.Context, start time.Time, metric string) (int64, error) {
	amount, err := s.queries.BillingUsage(ctx, pgquery.BillingUsageParams{OrganizationID: s.organization, WindowStart: start, Metric: metric})
	return amount, mapError(ctx, err)
}

func (s *session) BillingUsages(ctx context.Context, start time.Time, limit int) ([]billingstore.Usage, error) {
	rows, err := s.queries.BillingUsages(ctx, pgquery.BillingUsagesParams{OrganizationID: s.organization, WindowStart: start.UTC(), Limit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]billingstore.Usage, 0, len(rows))
	for _, row := range rows {
		result = append(result, billingstore.Usage{OrganizationID: row.OrganizationID, WindowStart: row.WindowStart, Metric: row.Metric, Amount: row.Amount})
	}
	return result, nil
}

func (s *session) AddBillingUsage(ctx context.Context, row billingstore.Usage) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.Amount < 0 {
		return billingstore.ErrInvalidAmount
	}
	err := s.queries.AddBillingUsage(ctx, pgquery.AddBillingUsageParams{OrganizationID: s.organization, WindowStart: row.WindowStart, Metric: row.Metric, Amount: row.Amount})
	return mapError(ctx, err)
}
