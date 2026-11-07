package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type creditFundingInput struct {
	Enabled   bool   `json:"enabled"`
	Threshold int64  `json:"threshold_cents"`
	Price     string `json:"price"`
}

func (s *Service) creditAvailable() bool {
	cfg := s.hostedBillingConfig()
	if s.config.Hosted == nil || cfg == nil || s.billing == nil || s.database.aiCreditMode == "" {
		return false
	}
	_, ok := cfg.Provider.(billing.CreditProvider)
	return ok
}

func (s *Service) checkoutCredits(ctx context.Context, authorize billingAuthorization, priceID, requestID string) (billingDestination, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	cfg := s.hostedBillingConfig()
	if _, err := authorize(ctx); err != nil {
		return billingDestination{}, err
	}
	if cfg == nil || !s.creditAvailable() {
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
		return billingDestination{}, errors.Join(billingFailure(http.StatusServiceUnavailable, "Billing is temporarily unavailable"), fmt.Errorf("ensure credit billing customer: %w", err))
	}
	if _, err := authorize(ctx); err != nil {
		return billingDestination{}, err
	}
	key := "detent-credit-" + s.config.newLeaseID()
	if requestID != "" {
		digest := sha256.Sum256([]byte(s.config.Hosted.OrganizationID + "\x00" + cfg.mode() + "\x00" + requestID))
		key = fmt.Sprintf("detent-credit-%x", digest)
	}

	var purchase billingstore.CreditPurchase
	err = s.database.readBillingTenant(readCtx, func(reader tenantstore.Reader) error {
		var err error
		purchase, err = reader.BillingCreditPurchase(readCtx, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		purchase = billingstore.CreditPurchase{OrganizationID: s.database.hostedOrganization, Key: key, Mode: cfg.mode(), PriceID: pack.PriceID, USDCents: pack.USDCents, State: "pending", CreatedAt: s.config.now().UTC()}
		if err := s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error { return writer.CreateBillingCreditPurchase(ctx, purchase) }); err != nil {
			return billingDestination{}, err
		}
	} else if err != nil {
		return billingDestination{}, err
	}
	if purchase.PriceID != pack.PriceID || purchase.USDCents != pack.USDCents || purchase.Mode != cfg.mode() || purchase.Automatic {
		return billingDestination{}, billingFailure(http.StatusConflict, "The purchase key already refers to another credit pack")
	}
	session := purchase.Session
	if session.URL == "" {
		if purchase.State != "pending" || s.config.now().Sub(purchase.CreatedAt) >= 23*time.Hour {
			return billingDestination{}, billingFailure(http.StatusConflict, "This purchase needs billing review")
		}
		provider, ok := cfg.Provider.(billing.CreditProvider)
		if !ok {
			return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "AI credit purchases are unavailable")
		}
		session, err = provider.CreditCheckout(ctx, billing.CreditRequest{CheckoutRequest: billing.CheckoutRequest{Binding: binding, PriceID: purchase.PriceID, IdempotencyKey: key, ReturnURL: s.hostedBillingReturn(true), ExpiresAt: purchase.CreatedAt.Truncate(time.Second).Add(time.Hour)}, USDCents: purchase.USDCents})
		if err != nil {
			var failure *billing.StripeError
			if errors.As(err, &failure) && failure.StatusCode >= 400 && failure.StatusCode < 500 && failure.StatusCode != http.StatusConflict && failure.Type != "idempotency_error" {
				if failure.Method == http.MethodPost && failure.Path == "checkout/sessions" {
					err = errors.Join(err, s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
						_, err := writer.MarkBillingCreditPurchaseFailed(ctx, purchase.Key)
						return err
					}))
				}
				return billingDestination{}, checkoutProviderFailure("", fmt.Errorf("create credit checkout: %w", err))
			}
			return billingDestination{}, errors.Join(billingFailure(http.StatusServiceUnavailable, "Credit checkout is temporarily unavailable; retry the same purchase"), fmt.Errorf("create credit checkout: %w", err))
		}
		if err := s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
			stored, err := writer.SetBillingCreditPurchaseSession(ctx, purchase.Key, session)
			if err == nil {
				session = stored
			}
			return err
		}); err != nil {
			return billingDestination{}, err
		}
	}
	return billingDestination{ID: session.ID, URL: session.URL, ExpiresAt: session.ExpiresAt}, nil
}

func (s *Service) configureCreditFunding(ctx context.Context, authorize billingAuthorization, request creditFundingInput, key string) (creditFundingInput, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	if _, err := authorize(ctx); err != nil {
		return creditFundingInput{}, err
	}
	cfg := s.hostedBillingConfig()
	if s.config.Hosted == nil || cfg == nil || s.billing == nil || s.database.aiCreditMode == "" || request.Enabled && !s.creditAvailable() {
		return creditFundingInput{}, billingFailure(http.StatusServiceUnavailable, "AI credit purchases are unavailable")
	}
	s.billing.mu.Lock()
	defer s.billing.mu.Unlock()
	credential, err := authorize(ctx)
	if err != nil {
		return creditFundingInput{}, err
	}
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
		{
			err := s.database.readBillingTenant(readCtx, func(reader tenantstore.Reader) error {
				account, err := reader.BillingCreditAccount(readCtx, s.database.aiCreditMode)
				method = account.PaymentMethod
				return err
			})
			if err != nil {
				return creditFundingInput{}, err
			}
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
			return creditFundingInput{}, errors.Join(billingFailure(http.StatusServiceUnavailable, "The saved payment method is temporarily unavailable"), fmt.Errorf("read saved payment method: %w", err))
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

	err = s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		account, err := writer.BillingCreditAccount(ctx, s.database.aiCreditMode)
		if err != nil {
			return err
		}
		if request.Enabled && method == "" {
			return billingFailure(http.StatusConflict, "Buy credits or save a payment method in the billing portal first")
		}
		account.AutoEnabled = request.Enabled
		account.ThresholdCents = max(request.Threshold, 0)
		account.PriceID = request.Price
		account.Failure = ""
		if request.Enabled {
			account.PaymentMethod = method
		}
		return writer.SetBillingCreditFunding(ctx, account)
	})
	if err != nil {
		return creditFundingInput{}, err
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
