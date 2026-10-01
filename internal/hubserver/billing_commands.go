package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/mutation"
)

type billingDestination struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

type billingAuthorization func(context.Context) (apiCredential, error)

func billingFailure(status int, message string) error {
	return &nativeError{Code: "billing_unavailable", Message: message, status: status}
}

// checkoutBilling is the dashboard purchase command. Pending receipts resume
// the existing customer/checkout intents under the existing billing mutex.
// Reauthorize after acquiring it, before any provider effect or receipt replay.
func (s *Service) checkoutBilling(ctx context.Context, authorize billingAuthorization, price, key string) (billingDestination, error) {
	if _, err := authorize(ctx); err != nil {
		return billingDestination{}, err
	}
	if s.billing == nil || s.config.Hosted == nil || s.config.Hosted.Billing == nil {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Subscription checkout is unavailable")
	}
	w := s.billing
	w.mu.Lock()
	defer w.mu.Unlock()
	credential, err := authorize(ctx)
	if err != nil {
		return billingDestination{}, err
	}
	cfg := s.config.Hosted.Billing
	if cfg.CheckoutDisabled {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "New subscriptions are paused")
	}
	approved := false
	for _, configured := range cfg.Prices {
		approved = approved || configured.PriceID == price
	}
	if !approved {
		return billingDestination{}, billingFailure(http.StatusBadRequest, "Choose an approved subscription plan")
	}
	command := hostedCommand{actor: credential.ID, operation: "billing.checkout", key: key, input: struct{ Price, Account, Mode, ReturnURL string }{price, cfg.AccountID, cfg.mode(), s.hostedBillingReturn(true)}}
	if key != "" {
		_, response, err := s.claimHostedOperation(ctx, command)
		if err != nil && !errors.Is(err, mutation.ErrUncertain) {
			return billingDestination{}, err
		}
		if response != nil {
			var result billingDestination
			err := json.Unmarshal(response, &result)
			return result, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	binding, err := s.ensureHostedCustomer(ctx, credential.Hosted.Subject)
	if errors.Is(err, billing.ErrCustomerConflict) {
		return billingDestination{}, billingFailure(http.StatusConflict, "Billing needs operator repair before a purchase")
	}
	if err != nil {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Billing is temporarily unavailable; retry the same purchase")
	}
	if err := w.reconcile(ctx); err != nil {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Billing is temporarily unavailable")
	}
	state, err := s.database.readHostedBilling(ctx)
	if err != nil {
		return billingDestination{}, err
	}
	if state.Snapshot.SubscriptionID != "" || state.Status == "multiple_subscriptions" {
		return billingDestination{}, billingFailure(http.StatusConflict, "Manage the existing subscription through the billing portal")
	}
	checkout, err := s.prepareHostedCheckout(ctx, credential.Hosted.Subject, price)
	if err != nil {
		return billingDestination{}, billingFailure(http.StatusConflict, "A different checkout is pending")
	}
	if checkout.Session.URL == "" {
		session, err := cfg.Provider.Checkout(ctx, billing.CheckoutRequest{Binding: binding, PriceID: checkout.PriceID, IdempotencyKey: checkout.Key, ExpiresAt: checkout.ExpiresAt, ReturnURL: s.hostedBillingReturn(true)})
		if err != nil {
			return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Checkout is temporarily unavailable; retry the same purchase")
		}
		checkout.Session = session
		if err := s.saveHostedCheckout(ctx, credential.Hosted.Subject, checkout, "checkout_created"); err != nil {
			return billingDestination{}, err
		}
	}
	result := billingDestination{ID: checkout.Session.ID, URL: checkout.Session.URL, ExpiresAt: checkout.ExpiresAt}
	if key != "" {
		if _, err := s.completeHostedOperation(ctx, command, result); err != nil {
			return billingDestination{}, err
		}
	}
	return result, nil
}

// portalBilling shares the existing durable receipt. An uncertain provider
// response is never retried automatically: the provider has no portal key.
func (s *Service) portalBilling(ctx context.Context, authorize billingAuthorization, key string) (billingDestination, error) {
	if _, err := authorize(ctx); err != nil {
		return billingDestination{}, err
	}
	if s.config.Hosted == nil || s.config.Hosted.Billing == nil {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Billing portal is unavailable")
	}
	credential, err := authorize(ctx)
	if err != nil {
		return billingDestination{}, err
	}
	cfg := s.config.Hosted.Billing
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	binding, err := s.database.hostedBillingBinding(ctx, cfg)
	if errors.Is(err, errHostedBillingUnbound) {
		return billingDestination{}, billingFailure(http.StatusConflict, "There is no subscription to manage")
	}
	if err != nil {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Billing is temporarily unavailable")
	}
	command := hostedCommand{actor: credential.ID, operation: "billing.portal", key: key, input: struct {
		Configuration, ReturnURL, Mode string
		Binding                        billing.Binding
	}{cfg.PortalConfigurationID, s.hostedBillingReturn(true), cfg.mode(), binding}}
	if key != "" {
		_, response, err := s.claimHostedOperation(ctx, command)
		if err != nil {
			return billingDestination{}, err
		}
		if response != nil {
			var result billingDestination
			err := json.Unmarshal(response, &result)
			return result, err
		}
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "billing_portal_requested", "/organization/billing/portal", "", http.StatusOK); err != nil {
		return billingDestination{}, err
	}
	if err := s.recordBillingAction(ctx, credential.Hosted.Subject, "portal_requested"); err != nil {
		return billingDestination{}, err
	}
	session, err := cfg.Provider.Portal(ctx, binding, cfg.PortalConfigurationID, s.hostedBillingReturn(true))
	if err != nil {
		return billingDestination{}, billingFailure(http.StatusServiceUnavailable, "Billing portal is temporarily unavailable")
	}
	result := billingDestination{ID: session.ID, URL: session.URL}
	if key != "" {
		if _, err := s.completeHostedOperation(ctx, command, result); err != nil {
			return billingDestination{}, err
		}
	}
	return result, nil
}
