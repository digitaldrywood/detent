package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
)

type hostedCheckout struct {
	Key       string          `json:"key"`
	PriceID   string          `json:"price_id"`
	ExpiresAt time.Time       `json:"expires_at"`
	Session   billing.Session `json:"session"`
}

func (s *Service) hostedBillingOwner(c echo.Context) (apiCredential, error) {
	credential, _, err := s.hostedCredential(c)
	if err != nil || credential.Hosted == nil || credential.HostedRole != "owner" || credential.Hosted.SupportActor != "" {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	return credential, nil
}

func (s *Service) hostedBillingCheckout(c echo.Context) error {
	api := hostedBillingAPI(c)
	var request struct {
		Price          string `json:"price"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return invalidAPIRequest(c, err)
		}
	}
	if _, err := s.hostedBillingOwner(c); err != nil {
		return s.hostedBillingFailure(c, api, http.StatusForbidden, "Billing requires an organization owner without support impersonation")
	}
	if s.billing == nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Subscription checkout is not enabled for this organization")
	}
	w := s.billing
	w.mu.Lock()
	defer w.mu.Unlock()
	credential, err := s.hostedBillingOwner(c)
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusForbidden, "Organization ownership changed; sign in again")
	}
	cfg := s.config.Hosted.Billing
	if cfg.CheckoutDisabled {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "New subscriptions are paused. Existing subscriptions and the billing portal keep working.")
	}
	priceID := request.Price
	if !api {
		priceID = c.FormValue("price")
	}
	approved := false
	for _, price := range cfg.Prices {
		approved = approved || price.PriceID == priceID
	}
	if !approved {
		return s.hostedBillingFailure(c, api, http.StatusBadRequest, "Choose an approved subscription plan")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 45*time.Second)
	defer cancel()
	binding, err := s.ensureHostedCustomer(ctx, credential.Hosted.Subject)
	if errors.Is(err, billing.ErrCustomerConflict) {
		return s.hostedBillingFailure(c, api, http.StatusConflict, "Billing needs operator repair before a purchase. No charge was made.")
	}
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Billing is temporarily unavailable. Retry to resume the same purchase.")
	}
	if err := w.reconcile(ctx); err != nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Billing is temporarily unavailable. Your current access deadline is unchanged.")
	}
	state, err := s.database.readHostedBilling(ctx)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if state.Snapshot.SubscriptionID != "" || state.Status == "multiple_subscriptions" {
		return s.hostedBillingFailure(c, api, http.StatusConflict, "An existing subscription must be managed through the billing portal")
	}
	checkout, err := s.prepareHostedCheckout(ctx, credential.Hosted.Subject, priceID)
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusConflict, "A checkout is already pending. Retry the same plan or wait for that checkout to expire.")
	}
	if checkout.Session.URL == "" {
		session, err := cfg.Provider.Checkout(ctx, billing.CheckoutRequest{Binding: binding, PriceID: checkout.PriceID, IdempotencyKey: checkout.Key, ExpiresAt: checkout.ExpiresAt, ReturnURL: s.hostedBillingReturn(api)})
		if err != nil {
			return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Checkout is temporarily unavailable. Retry to resume the same purchase.")
		}
		checkout.Session = session
		if err := s.saveHostedCheckout(ctx, credential.Hosted.Subject, checkout, "checkout_created"); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return s.hostedBillingDestination(c, api, checkout.Session.URL)
}

func (s *Service) prepareHostedCheckout(ctx context.Context, actor, price string) (hostedCheckout, error) {
	var checkout hostedCheckout
	var raw string
	if err := s.database.db.QueryRowContext(ctx, "SELECT checkout_json FROM hosted_billing_accounts WHERE organization_id=?", s.config.Hosted.OrganizationID).Scan(&raw); err != nil {
		return checkout, err
	}
	if err := json.Unmarshal([]byte(raw), &checkout); err != nil {
		return checkout, err
	}
	now := s.config.now()
	if checkout.ExpiresAt.After(now) {
		if checkout.PriceID != price {
			return checkout, errors.New("a different checkout is pending")
		}
		return checkout, nil
	}
	checkout = hostedCheckout{Key: "detent_" + s.config.newLeaseID(), PriceID: price, ExpiresAt: now.Truncate(time.Second).Add(time.Hour)}
	return checkout, s.saveHostedCheckout(ctx, actor, checkout, "checkout_requested")
}

func (s *Service) saveHostedCheckout(ctx context.Context, actor string, checkout hostedCheckout, action string) error {
	raw, err := json.Marshal(checkout)
	if err != nil {
		return err
	}
	audit, err := json.Marshal(struct {
		Key     string `json:"request_id"`
		PriceID string `json:"price_id"`
	}{checkout.Key, checkout.PriceID})
	if err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE hosted_billing_accounts SET checkout_json=? WHERE organization_id=?", string(raw), s.config.Hosted.OrganizationID); err != nil {
		return err
	}
	if err := s.database.insertBillingAudit(ctx, tx, actor, action, string(audit), s.config.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) hostedBillingPortal(c echo.Context) error {
	api := hostedBillingAPI(c)
	if api {
		var request struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := decodeAPIJSON(c, &request); err != nil {
			return invalidAPIRequest(c, err)
		}
	}
	credential, err := s.hostedBillingOwner(c)
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusForbidden, "Billing requires an organization owner without support impersonation")
	}
	cfg := s.config.Hosted.Billing
	if cfg == nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "The subscription portal is not enabled for this organization")
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "billing_portal_requested", "/organization/billing/portal", "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.recordBillingAction(c.Request().Context(), credential.Hosted.Subject, "portal_requested"); err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 45*time.Second)
	defer cancel()
	binding, err := s.database.hostedBillingBinding(ctx, cfg)
	if errors.Is(err, errHostedBillingUnbound) {
		return s.hostedBillingFailure(c, api, http.StatusConflict, "There is no subscription to manage yet. Choose a plan to start one.")
	}
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Billing is temporarily unavailable")
	}
	session, err := cfg.Provider.Portal(ctx, binding, cfg.PortalConfigurationID, s.hostedBillingReturn(api))
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "The billing portal is temporarily unavailable. Existing data and exports remain available.")
	}
	return s.hostedBillingDestination(c, api, session.URL)
}

func (s *Service) recordBillingAction(ctx context.Context, actor, action string) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.database.insertBillingAudit(ctx, tx, actor, action, "{}", s.config.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) ensureHostedCustomer(ctx context.Context, actor string) (billing.Binding, error) {
	cfg := s.config.Hosted.Billing
	binding, err := s.database.hostedBillingBinding(ctx, cfg)
	if !errors.Is(err, errHostedBillingUnbound) {
		return binding, err
	}
	provider, ok := cfg.Provider.(billing.CustomerProvider)
	if !ok {
		return billing.Binding{}, errors.New("billing provider cannot create customers")
	}
	organization := s.config.Hosted.OrganizationID
	key := "detent-customer-" + cfg.AccountID + "-" + cfg.mode() + "-" + organization
	now := formatHubTime(s.config.now())
	if _, err := s.database.db.ExecContext(ctx, "INSERT INTO hosted_billing_customer_intents(organization_id,account_id,mode,idempotency_key,state,created_at,updated_at) VALUES(?,?,?,?,'pending',?,?) ON CONFLICT DO NOTHING", organization, cfg.AccountID, cfg.mode(), key, now, now); err != nil {
		return billing.Binding{}, err
	}
	var account, mode, state string
	if err := s.database.db.QueryRowContext(ctx, "SELECT account_id,mode,state FROM hosted_billing_customer_intents WHERE organization_id=?", organization).Scan(&account, &mode, &state); err != nil {
		return billing.Binding{}, err
	}
	if state == "conflict" {
		return billing.Binding{}, billing.ErrCustomerConflict
	}
	if account != cfg.AccountID || mode != cfg.mode() {
		return billing.Binding{}, errors.New("a pending billing customer belongs to a different account or mode")
	}
	customer, err := provider.EnsureCustomer(ctx, billing.CustomerRequest{AccountID: cfg.AccountID, OrganizationID: organization, IdempotencyKey: key})
	if errors.Is(err, billing.ErrCustomerConflict) {
		if _, updateErr := s.database.db.ExecContext(ctx, "UPDATE hosted_billing_customer_intents SET state='conflict',updated_at=? WHERE organization_id=?", formatHubTime(s.config.now()), organization); updateErr != nil {
			return billing.Binding{}, errors.Join(err, updateErr)
		}
		return billing.Binding{}, err
	}
	if err != nil {
		return billing.Binding{}, err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return billing.Binding{}, err
	}
	defer tx.Rollback()
	stamp := s.config.now()
	if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_billing_accounts(organization_id,account_id,customer_id,mode) VALUES(?,?,?,?)", organization, cfg.AccountID, customer, cfg.mode()); err != nil {
		return billing.Binding{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE hosted_billing_customer_intents SET state='bound',customer_id=?,updated_at=? WHERE organization_id=?", customer, formatHubTime(stamp), organization); err != nil {
		return billing.Binding{}, err
	}
	record, err := json.Marshal(map[string]string{"status": "customer_bound", "mode": cfg.mode()})
	if err != nil {
		return billing.Binding{}, err
	}
	if err := s.database.insertBillingAudit(ctx, tx, actor, "customer_bound", string(record), stamp); err != nil {
		return billing.Binding{}, err
	}
	if err := tx.Commit(); err != nil {
		return billing.Binding{}, err
	}
	return s.database.hostedBillingBinding(ctx, cfg)
}

func hostedBillingAPI(c echo.Context) bool {
	return strings.HasPrefix(c.Path(), "/api/v2/")
}

func (s *Service) hostedBillingReturn(bool) string {
	if _, err := fs.Stat(conversationClientFS, conversationClientShell); err == nil {
		return s.config.Hosted.PublicURL + s.hostedPath("/settings/billing")
	}
	return s.config.Hosted.PublicURL + s.hostedPath("/organization/billing")
}

func (s *Service) hostedBillingFailure(c echo.Context, api bool, status int, message string) error {
	if !api {
		return s.hostedError(c, status, message)
	}
	code := map[int]string{http.StatusForbidden: "forbidden", http.StatusConflict: "billing_conflict", http.StatusBadRequest: "invalid_price", http.StatusServiceUnavailable: "billing_unavailable"}[status]
	if code == "" {
		code = "billing_failed"
	}
	return c.JSON(status, apiErrorResponse{Code: code, Message: message})
}

func (s *Service) hostedBillingDestination(c echo.Context, api bool, url string) error {
	if api {
		return c.JSON(http.StatusOK, map[string]string{"url": url})
	}
	return c.Redirect(http.StatusSeeOther, url)
}
