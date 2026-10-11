package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
)

type creditFundingInput struct {
	Enabled   bool   `json:"enabled"`
	Threshold int64  `json:"threshold_cents"`
	Price     string `json:"price"`
}

func (s *Service) creditAvailable() bool {
	if s.config.Hosted == nil || s.config.Hosted.Billing == nil || s.billing == nil || s.database.aiCreditMode == "" {
		return false
	}
	_, ok := s.config.Hosted.Billing.Provider.(billing.CreditProvider)
	return ok
}

func (s *Service) checkoutCredits(ctx context.Context, authorize billingAuthorization, priceID, requestID string) (billingDestination, error) {
	if _, err := authorize(ctx); err != nil {
		return billingDestination{}, err
	}
	if !s.creditAvailable() {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "AI credit purchases are unavailable")
	}
	s.billing.mu.Lock()
	defer s.billing.mu.Unlock()
	credential, err := authorize(ctx)
	if err != nil {
		return billingDestination{}, err
	}
	pack, ok := s.creditPack(priceID)
	if !ok {
		return billingDestination{}, billingFailure(http.StatusBadRequest, "Choose a configured AI credit pack")
	}
	binding, err := s.ensureHostedCustomer(ctx, credential.Hosted.Subject)
	if err != nil {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Billing is temporarily unavailable")
	}
	if _, err := authorize(ctx); err != nil {
		return billingDestination{}, err
	}
	key := "detent-credit-" + s.config.newLeaseID()
	if requestID != "" {
		digest := sha256.Sum256([]byte(s.config.Hosted.OrganizationID + "\x00" + s.config.Hosted.Billing.mode() + "\x00" + requestID))
		key = fmt.Sprintf("detent-credit-%x", digest)
	}
	var raw string
	var cents, at int64
	var price, mode, state string
	err = s.database.db.QueryRowContext(ctx, "SELECT price_id,usd_cents,created_at,session_json,mode,state FROM ai_credit_purchases WHERE purchase_key=? AND organization_id=? AND automatic=0", key, binding.OrganizationID).Scan(&price, &cents, &at, &raw, &mode, &state)
	if errors.Is(err, sql.ErrNoRows) {
		price, cents, at, mode, state = pack.PriceID, pack.USDCents, s.config.now().UnixMicro(), s.config.Hosted.Billing.mode(), "pending"
		_, err = s.database.db.ExecContext(ctx, "INSERT INTO ai_credit_purchases(purchase_key,organization_id,mode,price_id,usd_cents,automatic,created_at) VALUES(?,?,?,?,?,0,?)", key, binding.OrganizationID, mode, price, cents, at)
		raw = "{}"
	}
	if err != nil {
		return billingDestination{}, err
	}
	if price != pack.PriceID || cents != pack.USDCents || mode != s.config.Hosted.Billing.mode() {
		return billingDestination{}, billingFailure(http.StatusConflict, "The purchase key already refers to another credit pack")
	}
	var session billing.Session
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return billingDestination{}, err
	}
	if session.URL == "" {
		if state != "pending" || s.config.now().Sub(time.UnixMicro(at)) >= 23*time.Hour {
			return billingDestination{}, billingFailure(http.StatusConflict, "This purchase needs billing review")
		}
		provider, ok := s.config.Hosted.Billing.Provider.(billing.CreditProvider)
		if !ok {
			return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "AI credit purchases are unavailable")
		}
		session, err = provider.CreditCheckout(ctx, billing.CreditRequest{CheckoutRequest: billing.CheckoutRequest{Binding: binding, PriceID: price, IdempotencyKey: key, ReturnURL: s.hostedBillingReturn(true), ExpiresAt: time.UnixMicro(at).Truncate(time.Second).Add(time.Hour)}, USDCents: cents})
		if err != nil {
			return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Credit checkout is temporarily unavailable; retry the same purchase")
		}
		encoded, err := json.Marshal(session)
		if err != nil {
			return billingDestination{}, err
		}
		if _, err := s.database.db.ExecContext(ctx, "UPDATE ai_credit_purchases SET session_json=? WHERE purchase_key=?", string(encoded), key); err != nil {
			return billingDestination{}, err
		}
	}
	return billingDestination{ID: session.ID, URL: session.URL, ExpiresAt: session.ExpiresAt}, nil
}

func (s *Service) configureCreditFunding(ctx context.Context, authorize billingAuthorization, request creditFundingInput, key string) (creditFundingInput, error) {
	if _, err := authorize(ctx); err != nil {
		return creditFundingInput{}, err
	}
	if s.config.Hosted == nil || s.config.Hosted.Billing == nil || s.billing == nil || s.database.aiCreditMode == "" || request.Enabled && !s.creditAvailable() {
		return creditFundingInput{}, billingFailure(http.StatusServiceUnavailable, "AI credit purchases are unavailable")
	}
	s.billing.mu.Lock()
	defer s.billing.mu.Unlock()
	credential, err := authorize(ctx)
	if err != nil {
		return creditFundingInput{}, err
	}
	cfg := s.config.Hosted.Billing
	command := hostedCommand{actor: credential.ID, operation: "billing.credit_auto_fund", key: key, input: struct {
		Account, Mode string
		Settings      creditFundingInput
	}{cfg.AccountID, cfg.mode(), request}}
	if key != "" {
		_, response, err := s.claimHostedOperation(ctx, command)
		if err != nil {
			return creditFundingInput{}, err
		}
		if response != nil {
			var result creditFundingInput
			err := json.Unmarshal(response, &result)
			return result, err
		}
	}
	completed := false
	defer func() {
		if key != "" && !completed {
			s.abandonHostedOperation(ctx, command)
		}
	}()
	var method string
	if request.Enabled {
		pack, ok := s.creditPack(request.Price)
		if !ok || request.Threshold <= 0 || request.Threshold >= pack.USDCents {
			return creditFundingInput{}, billingFailure(http.StatusBadRequest, "Choose a threshold greater than zero and smaller than the credit pack")
		}
		if err := s.database.db.QueryRowContext(ctx, "SELECT payment_method FROM ai_credit_accounts WHERE organization_id=? AND mode=?", s.database.hostedOrganization, s.database.aiCreditMode).Scan(&method); err != nil {
			return creditFundingInput{}, err
		}
		binding, err := s.database.hostedBillingBinding(ctx, cfg)
		if err != nil {
			return creditFundingInput{}, billingFailure(http.StatusConflict, "Buy credits or save a payment method in the billing portal first")
		}
		provider, available := cfg.Provider.(billing.CreditProvider)
		if !available {
			return creditFundingInput{}, billingFailure(http.StatusServiceUnavailable, "AI credit purchases are unavailable")
		}
		current, err := provider.SavedCreditPaymentMethod(ctx, binding)
		if err != nil && !errors.Is(err, billing.ErrPaymentFailed) {
			return creditFundingInput{}, billingFailure(http.StatusServiceUnavailable, "The saved payment method is temporarily unavailable")
		}
		if current != "" {
			method = current
		}
		if method == "" {
			return creditFundingInput{}, billingFailure(http.StatusConflict, "Buy credits or save a payment method in the billing portal first")
		}
		if _, err := authorize(ctx); err != nil {
			return creditFundingInput{}, err
		}
	}
	result, err := s.database.db.ExecContext(ctx, `UPDATE ai_credit_accounts SET auto_enabled=?,threshold_cents=?,price_id=?,failure='',payment_method=CASE WHEN ? THEN ? ELSE payment_method END WHERE organization_id=? AND mode=? AND (?=0 OR ?<>'')`, request.Enabled, max(request.Threshold, 0), request.Price, request.Enabled, method, s.database.hostedOrganization, s.database.aiCreditMode, request.Enabled, method)
	if err != nil {
		return creditFundingInput{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return creditFundingInput{}, err
	}
	if n != 1 {
		return creditFundingInput{}, billingFailure(http.StatusConflict, "Buy credits or save a payment method in the billing portal first")
	}
	completed = true
	request.Threshold = max(request.Threshold, 0)
	if key != "" {
		if _, err := s.completeHostedOperation(ctx, command, request); err != nil {
			return creditFundingInput{}, err
		}
	}
	return request, nil
}
