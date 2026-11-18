package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *session) BillingAccount(ctx context.Context) (billingstore.Account, error) {
	var row billingstore.Account
	var date7, date8, date9, date15, date16, date17, date18, date19, date24, date27 pgtype.Timestamptz
	err := s.db.QueryRow(ctx, `SELECT organization_id,account_id,customer_id,mode,status,plan_id,plan_version,paid_through,access_until,grace_until,subscription_id,price_id,subscription_status,invoice_id,invoice_status,invoice_created_at,period_start,period_end,trial_end,cancel_at,cancel_at_period_end,payment_hold,checkout_key,checkout_price_id,checkout_expires_at,checkout_session_id,checkout_url,checkout_session_expires_at,reconciled_at FROM hosted_billing_accounts WHERE organization_id=$1`, s.organization).Scan(&row.OrganizationID, &row.Binding.AccountID, &row.Binding.CustomerID, &row.Mode, &row.Status, &row.Plan.ID, &row.Plan.Version, &date7, &date8, &date9, &row.Snapshot.SubscriptionID, &row.Snapshot.PriceID, &row.Snapshot.Status, &row.Snapshot.InvoiceID, &row.Snapshot.InvoiceStatus, &date15, &date16, &date17, &date18, &date19, &row.Snapshot.CancelAtPeriodEnd, &row.Snapshot.PaymentHold, &row.Checkout.Key, &row.Checkout.PriceID, &date24, &row.Checkout.Session.ID, &row.Checkout.Session.URL, &date27, &row.ReconciledAt)
	if date7.Valid {
		row.PaidThrough = date7.Time
	}
	if date8.Valid {
		row.AccessUntil = date8.Time
	}
	if date9.Valid {
		row.GraceUntil = date9.Time
	}
	if date15.Valid {
		row.Snapshot.InvoiceCreatedAt = date15.Time
	}
	if date16.Valid {
		row.Snapshot.PeriodStart = date16.Time
	}
	if date17.Valid {
		row.Snapshot.PeriodEnd = date17.Time
	}
	if date18.Valid {
		row.Snapshot.TrialEnd = date18.Time
	}
	if date19.Valid {
		row.Snapshot.CancelAt = date19.Time
	}
	if date24.Valid {
		row.Checkout.ExpiresAt = date24.Time
	}
	if date27.Valid {
		row.Checkout.Session.ExpiresAt = date27.Time
	}
	row.Binding.OrganizationID = string(row.OrganizationID)
	return row, mapError(ctx, err)
}
func (s *session) SetBillingAccount(ctx context.Context, row billingstore.Account) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `INSERT INTO hosted_billing_accounts(organization_id,account_id,customer_id,mode,status,plan_id,plan_version,paid_through,access_until,grace_until,subscription_id,price_id,subscription_status,invoice_id,invoice_status,invoice_created_at,period_start,period_end,trial_end,cancel_at,cancel_at_period_end,payment_hold,checkout_key,checkout_price_id,checkout_expires_at,checkout_session_id,checkout_url,checkout_session_expires_at,reconciled_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29) ON CONFLICT(organization_id) DO UPDATE SET status=excluded.status,plan_id=excluded.plan_id,plan_version=excluded.plan_version,paid_through=excluded.paid_through,access_until=excluded.access_until,grace_until=excluded.grace_until,subscription_id=excluded.subscription_id,price_id=excluded.price_id,subscription_status=excluded.subscription_status,invoice_id=excluded.invoice_id,invoice_status=excluded.invoice_status,invoice_created_at=excluded.invoice_created_at,period_start=excluded.period_start,period_end=excluded.period_end,trial_end=excluded.trial_end,cancel_at=excluded.cancel_at,cancel_at_period_end=excluded.cancel_at_period_end,payment_hold=excluded.payment_hold,checkout_key=excluded.checkout_key,checkout_price_id=excluded.checkout_price_id,checkout_expires_at=excluded.checkout_expires_at,checkout_session_id=excluded.checkout_session_id,checkout_url=excluded.checkout_url,checkout_session_expires_at=excluded.checkout_session_expires_at,reconciled_at=excluded.reconciled_at WHERE hosted_billing_accounts.account_id=excluded.account_id AND hosted_billing_accounts.customer_id=excluded.customer_id AND hosted_billing_accounts.mode=excluded.mode`, row.OrganizationID, row.Binding.AccountID, row.Binding.CustomerID, row.Mode, row.Status, row.Plan.ID, row.Plan.Version, optionalTime(row.PaidThrough), optionalTime(row.AccessUntil), optionalTime(row.GraceUntil), row.Snapshot.SubscriptionID, row.Snapshot.PriceID, row.Snapshot.Status, row.Snapshot.InvoiceID, row.Snapshot.InvoiceStatus, optionalTime(row.Snapshot.InvoiceCreatedAt), optionalTime(row.Snapshot.PeriodStart), optionalTime(row.Snapshot.PeriodEnd), optionalTime(row.Snapshot.TrialEnd), optionalTime(row.Snapshot.CancelAt), row.Snapshot.CancelAtPeriodEnd, row.Snapshot.PaymentHold, row.Checkout.Key, row.Checkout.PriceID, optionalTime(row.Checkout.ExpiresAt), row.Checkout.Session.ID, row.Checkout.Session.URL, optionalTime(row.Checkout.Session.ExpiresAt), row.ReconciledAt)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	return affected(ctx, n, err)
}

func (s *session) CreateBillingAccountIfAbsent(ctx context.Context, row billingstore.Account) (bool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return false, err
	}
	result, err := s.db.Exec(ctx, `INSERT INTO hosted_billing_accounts(organization_id,account_id,customer_id,mode,status,plan_id,plan_version,paid_through,access_until,grace_until,subscription_id,price_id,subscription_status,invoice_id,invoice_status,invoice_created_at,period_start,period_end,trial_end,cancel_at,cancel_at_period_end,payment_hold,checkout_key,checkout_price_id,checkout_expires_at,checkout_session_id,checkout_url,checkout_session_expires_at,reconciled_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29) ON CONFLICT(organization_id) DO NOTHING`, row.OrganizationID, row.Binding.AccountID, row.Binding.CustomerID, row.Mode, row.Status, row.Plan.ID, row.Plan.Version, optionalTime(row.PaidThrough), optionalTime(row.AccessUntil), optionalTime(row.GraceUntil), row.Snapshot.SubscriptionID, row.Snapshot.PriceID, row.Snapshot.Status, row.Snapshot.InvoiceID, row.Snapshot.InvoiceStatus, optionalTime(row.Snapshot.InvoiceCreatedAt), optionalTime(row.Snapshot.PeriodStart), optionalTime(row.Snapshot.PeriodEnd), optionalTime(row.Snapshot.TrialEnd), optionalTime(row.Snapshot.CancelAt), row.Snapshot.CancelAtPeriodEnd, row.Snapshot.PaymentHold, row.Checkout.Key, row.Checkout.PriceID, optionalTime(row.Checkout.ExpiresAt), row.Checkout.Session.ID, row.Checkout.Session.URL, optionalTime(row.Checkout.Session.ExpiresAt), row.ReconciledAt)
	if err != nil {
		return false, mapError(ctx, err)
	}
	count := result.RowsAffected()
	return count == 1, mapError(ctx, err)
}

func (s *session) DeleteBillingAccount(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM hosted_billing_accounts WHERE organization_id=$1`, s.organization)
	return mapError(ctx, err)
}

func (s *session) DeleteBillingAccountIfMatches(ctx context.Context, accountID, customerID, mode string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM hosted_billing_accounts WHERE organization_id=$1 AND account_id=$2 AND customer_id=$3 AND mode=$4`, s.organization, accountID, customerID, mode)
	if err != nil {
		return mapError(ctx, err)
	}
	count := result.RowsAffected()
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
