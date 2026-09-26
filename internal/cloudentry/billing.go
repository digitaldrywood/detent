package cloudentry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
)

type BillingConfig struct {
	Mode          string
	AccountID     string
	WebhookSecret []byte
	Provider      billing.CustomerProvider
}

func (b *BillingConfig) validate() error {
	if b == nil {
		return nil
	}
	if !billing.ValidMode(b.Mode) || !safeID(b.AccountID) || len(b.AccountID) < 6 || b.AccountID[:5] != "acct_" || len(b.WebhookSecret) < 16 || string(b.WebhookSecret[:6]) != "whsec_" || b.Provider == nil {
		return errors.New("shared billing requires an explicit test or live mode, account, webhook secret and matching provider")
	}
	return nil
}

type tenantBilling struct {
	Enabled         bool      `json:"enabled"`
	AccountID       string    `json:"account_id"`
	Mode            string    `json:"mode"`
	CustomerID      string    `json:"customer_id"`
	Status          string    `json:"status"`
	AccessUntil     time.Time `json:"access_until"`
	CheckoutPending bool      `json:"checkout_pending"`
}

func (s *Service) tenantBillingState(ctx context.Context, organization Organization, reconcile bool) (tenantBilling, error) {
	var result tenantBilling
	status, body, err := s.serviceCall(ctx, organization, "/internal/v1/billing/binding", map[string]bool{"reconcile": reconcile}, nil)
	if err != nil {
		return result, err
	}
	if status != http.StatusOK || json.Unmarshal(body, &result) != nil {
		return result, errors.New("tenant billing binding is unavailable")
	}
	return result, nil
}

func (s *Service) stripeWebhook(c echo.Context) error {
	config := s.config.Billing
	if config == nil || c.Param("mode") != config.Mode {
		return c.NoContent(http.StatusNotFound)
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 1024*1024))
	if err != nil {
		return c.NoContent(http.StatusBadRequest)
	}
	event, err := billing.VerifyModeEvent(body, c.Request().Header.Get("Stripe-Signature"), config.WebhookSecret, s.config.now(), config.Mode == billing.ModeLive)
	if err != nil {
		return c.NoContent(http.StatusBadRequest)
	}
	status := "pending"
	if !event.Relevant() || event.Customer == "" && event.Charge == "" {
		status = "ignored"
	}
	now := formatTime(s.config.now())
	inserted, err := s.registry.store.db.ExecContext(c.Request().Context(), "INSERT INTO billing_events(event_id,mode,event_type,customer_id,charge_id,status,received_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING",
		event.ID, config.Mode, event.Type, event.Customer, event.Charge, status, now, now)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if rows, err := inserted.RowsAffected(); err == nil && rows == 1 && status == "pending" {
		s.deliverBillingEvent(c.Request().Context(), event.ID)
	}
	return c.NoContent(http.StatusOK)
}

func (s *Service) billingOrganization(ctx context.Context, customer string) (Organization, error) {
	config := s.config.Billing
	var id string
	err := s.registry.store.db.QueryRowContext(ctx, "SELECT organization_id FROM billing_customers WHERE mode = ? AND account_id = ? AND customer_id = ?", config.Mode, config.AccountID, customer).Scan(&id)
	if err == nil {
		return s.registry.Organization(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Organization{}, err
	}
	candidate, err := config.Provider.CustomerOrganization(ctx, customer)
	if err != nil {
		return Organization{}, errQuarantine
	}
	organization, err := s.readyOrganization(ctx, candidate)
	if err != nil {
		return Organization{}, errQuarantine
	}
	binding, err := s.tenantBillingState(ctx, organization, false)
	if err != nil {
		return Organization{}, err
	}
	if !binding.Enabled || binding.CustomerID != customer || binding.AccountID != config.AccountID || binding.Mode != config.Mode {
		return Organization{}, errQuarantine
	}
	if _, err := s.registry.store.db.ExecContext(ctx, "INSERT INTO billing_customers(mode,account_id,customer_id,organization_id,created_at) VALUES(?,?,?,?,?)", config.Mode, config.AccountID, customer, organization.ID, formatTime(s.config.now())); err != nil {
		return Organization{}, errQuarantine
	}
	return organization, nil
}

var errQuarantine = errors.New("billing event cannot be routed to a verified organization")

func (s *Service) deliverBillingEvent(parent context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancel()
	var eventType, customer, charge string
	var attempts int
	if err := s.registry.store.db.QueryRowContext(ctx, "SELECT event_type,customer_id,charge_id,attempts FROM billing_events WHERE event_id = ? AND status = 'pending'", id).Scan(&eventType, &customer, &charge, &attempts); err != nil {
		return
	}
	status, organizationID := "pending", ""
	var organization Organization
	var err error
	if customer == "" {
		customer, err = s.config.Billing.Provider.ChargeCustomer(ctx, charge)
		if err == nil {
			_, err = s.registry.store.db.ExecContext(ctx, "UPDATE billing_events SET customer_id = ? WHERE event_id = ?", customer, id)
		}
	}
	if err == nil {
		organization, err = s.billingOrganization(ctx, customer)
	}
	switch {
	case errors.Is(err, errQuarantine):
		status = "quarantined"
	case err == nil:
		organizationID = organization.ID
		code, _, callErr := s.serviceCall(ctx, organization, "/internal/v1/billing/events", map[string]string{"event_id": id, "event_type": eventType, "customer_id": customer}, nil)
		switch {
		case callErr == nil && code == http.StatusNoContent:
			status = "delivered"
		case callErr == nil && code == http.StatusConflict:
			status = "quarantined"
		}
	}
	if status == "pending" && attempts+1 >= 20 {
		status = "quarantined"
	}
	if status == "quarantined" {
		s.config.Logger.Warn("billing event quarantined", "event", id)
	}
	if _, err := s.registry.store.db.ExecContext(ctx, "UPDATE billing_events SET status = ?, organization_id = ?, attempts = attempts + 1, updated_at = ? WHERE event_id = ? AND status = 'pending'", status, organizationID, formatTime(s.config.now()), id); err != nil {
		s.config.Logger.Warn("billing event state could not be recorded", "event", id)
	}
}

func (s *Service) retryBillingEvents(ctx context.Context) {
	if s.config.Billing == nil {
		return
	}
	ids, err := s.pendingBillingEvents(ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		s.deliverBillingEvent(ctx, id)
	}
}

func (s *Service) pendingBillingEvents(ctx context.Context) ([]string, error) {
	rows, err := s.registry.store.db.QueryContext(ctx, "SELECT event_id FROM billing_events WHERE status = 'pending' ORDER BY received_at LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) startBillingWorker(parent context.Context) {
	if s.config.Billing == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.stopBilling = cancel
	s.billingDone = make(chan struct{})
	go func() {
		defer close(s.billingDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.retryBillingEvents(ctx)
			}
		}
	}()
}

func (s *Service) closeBillingWorker() {
	if s.stopBilling != nil {
		s.stopBilling()
		<-s.billingDone
	}
}

func billingBlocksDeletion(state tenantBilling, now time.Time) bool {
	if state.CheckoutPending {
		return true
	}
	switch state.Status {
	case "active", "trialing", "past_due", "grace", "canceling", "payment_failed":
		return true
	}
	return state.AccessUntil.After(now)
}
