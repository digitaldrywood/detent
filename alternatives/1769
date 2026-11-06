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
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

type hostedCheckout struct {
	Key       string          `json:"key"`
	PriceID   string          `json:"price_id"`
	ExpiresAt time.Time       `json:"expires_at"`
	Session   billing.Session `json:"session"`
}

func (s *Service) hostedBillingOwner(ctx context.Context, c echo.Context) (apiCredential, error) {
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil || credential.Hosted == nil || credential.HostedRole != "owner" || credential.Hosted.SupportActor != "" {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	return credential, nil
}

func (s *Service) hostedBillingCheckout(c echo.Context) error {
	api := hostedBillingAPI(c)
	var request struct {
		hostedIdempotent
		Price string `json:"price"`
	}
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		if err := request.validate(true); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if !api {
		request.Price = c.FormValue("price")
	}
	authorize := func(ctx context.Context) (apiCredential, error) {
		return s.hostedBillingOwner(ctx, c)
	}
	result, err := s.checkoutBilling(c.Request().Context(), authorize, request.Price, request.IdempotencyKey)
	if err != nil {
		s.logStripeFailure(c.Request().Context(), err)
		var failure *nativeError
		switch {
		case errors.Is(err, auth.ErrHostedIdentity):
			return s.hostedBillingFailure(c, api, http.StatusForbidden, "Billing requires an organization owner without support impersonation")
		case errors.Is(err, mutation.ErrConflict):
			return s.hostedBillingFailure(c, api, http.StatusConflict, mutation.ErrConflict.Error())
		case errors.Is(err, mutation.ErrUncertain):
			return s.hostedBillingFailure(c, api, http.StatusConflict, mutation.ErrUncertain.Error())
		case errors.As(err, &failure):
			return s.hostedBillingFailure(c, api, failure.status, failure.Message)
		default:
			return s.nativeAPIError(c, err)
		}
	}
	if api {
		return c.JSON(http.StatusOK, result)
	}
	return s.hostedBillingDestination(c, api, result.URL)
}

func (s *Service) prepareHostedCheckout(ctx context.Context, actor, price string) (hostedCheckout, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	var checkout hostedCheckout

	err := s.database.readBillingTenant(readCtx, func(reader tenantstore.Reader) error {
		account, err := reader.BillingAccount(readCtx)
		if err != nil {
			return err
		}
		checkout = hostedCheckout{Key: account.Checkout.Key, PriceID: account.Checkout.PriceID, ExpiresAt: account.Checkout.ExpiresAt, Session: account.Checkout.Session}
		return nil
	})
	if err != nil {
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
	audit, err := json.Marshal(struct {
		Key     string `json:"request_id"`
		PriceID string `json:"price_id"`
	}{checkout.Key, checkout.PriceID})
	if err != nil {
		return err
	}

	if s.database.billingAuditStore == nil {
		return errors.New("hosted billing audit storage is unavailable")
	}
	archive, err := s.database.billingAuditStore.StoreDetail(ctx, string(s.database.hostedOrganization), audit)
	if err != nil {
		return err
	}
	return s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		account, err := writer.BillingAccount(ctx)
		if err != nil {
			return err
		}
		account.Checkout = billingstore.Checkout{Key: checkout.Key, PriceID: checkout.PriceID, ExpiresAt: checkout.ExpiresAt, Session: checkout.Session}
		if err := writer.SetBillingAccount(ctx, account); err != nil {
			return err
		}
		return writer.CreateBillingAudit(ctx, billingstore.Audit{OrganizationID: s.database.hostedOrganization, ActorID: actor, Action: action, RecordedAt: s.config.now().UTC(), Reference: billingstore.AuditReferenceFromArchive(archive)})
	})
}

func (s *Service) hostedBillingPortal(c echo.Context) error {
	api := hostedBillingAPI(c)
	var request struct{ hostedIdempotent }
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		if err := request.validate(true); err != nil {
			return s.nativeAPIError(c, err)
		}
	}

	authorize := func(ctx context.Context) (apiCredential, error) {
		return s.hostedBillingOwner(ctx, c)
	}
	result, err := s.portalBilling(c.Request().Context(), authorize, request.IdempotencyKey)
	if err != nil {
		s.logStripeFailure(c.Request().Context(), err)
		var failure *nativeError
		switch {
		case errors.Is(err, auth.ErrHostedIdentity):
			return s.hostedBillingFailure(c, api, http.StatusForbidden, "Billing requires an organization owner without support impersonation")
		case errors.Is(err, mutation.ErrConflict):
			return s.hostedBillingFailure(c, api, http.StatusConflict, mutation.ErrConflict.Error())
		case errors.Is(err, mutation.ErrUncertain):
			return s.hostedBillingFailure(c, api, http.StatusConflict, mutation.ErrUncertain.Error())
		case errors.As(err, &failure):
			return s.hostedBillingFailure(c, api, failure.status, failure.Message)
		default:
			return s.nativeAPIError(c, err)
		}
	}
	if api {
		return c.JSON(http.StatusOK, result)
	}
	return s.hostedBillingDestination(c, api, result.URL)
}

func (s *Service) recordBillingAction(ctx context.Context, actor, action string) error {

	if s.database.billingAuditStore == nil {
		return errors.New("hosted billing audit storage is unavailable")
	}
	raw, err := json.Marshal(map[string]string{"action": action})
	if err != nil {
		return err
	}
	archive, err := s.database.billingAuditStore.StoreDetail(ctx, string(s.database.hostedOrganization), raw)
	if err != nil {
		return err
	}
	return s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		return writer.CreateBillingAudit(ctx, billingstore.Audit{OrganizationID: s.database.hostedOrganization, ActorID: actor, Action: action, RecordedAt: s.config.now().UTC(), Reference: billingstore.AuditReferenceFromArchive(archive)})
	})
}

func (s *Service) ensureHostedCustomer(ctx context.Context, actor string) (billing.Binding, error) {
	cfg := s.hostedBillingConfig()
	if cfg == nil {
		return billing.Binding{}, errHostedBillingUnbound
	}
	binding, err := s.database.hostedBillingBinding(ctx, cfg)
	if !errors.Is(err, errHostedBillingUnbound) {
		return binding, err
	}
	provider, ok := cfg.Provider.(billing.CustomerProvider)
	if !ok {
		return billing.Binding{}, errors.New("billing provider cannot create customers")
	}
	organization := s.config.Hosted.OrganizationID

	return s.ensureHostedCustomerRecord(ctx, actor, cfg, provider, organization)
}

func (s *Service) ensureHostedCustomerRecord(ctx context.Context, actor string, cfg *HostedBillingConfig, provider billing.CustomerProvider, organization string) (billing.Binding, error) {
	d := s.database
	key := "detent-customer-" + cfg.AccountID + "-" + cfg.mode() + "-" + organization
	now := s.config.now().UTC()
	intent := billingstore.CustomerIntent{OrganizationID: d.hostedOrganization, AccountID: cfg.AccountID, Mode: cfg.mode(), IdempotencyKey: key, State: "pending", CreatedAt: now, UpdatedAt: now}
	if err := d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error { return writer.CreateBillingCustomerIntent(ctx, intent) }); err != nil {
		return billing.Binding{}, err
	}
	var existing billingstore.CustomerIntent
	if err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		existing, err = reader.BillingCustomerIntent(ctx)
		return err
	}); err != nil {
		return billing.Binding{}, err
	}
	if existing.State == "conflict" {
		return billing.Binding{}, billing.ErrCustomerConflict
	}
	if existing.AccountID != cfg.AccountID || existing.Mode != cfg.mode() || existing.IdempotencyKey != key {
		return billing.Binding{}, errors.New("a pending billing customer belongs to a different account or mode")
	}
	customer, err := provider.EnsureCustomer(ctx, billing.CustomerRequest{AccountID: cfg.AccountID, OrganizationID: organization, IdempotencyKey: key})
	if errors.Is(err, billing.ErrCustomerConflict) {
		existing.State, existing.UpdatedAt = "conflict", s.config.now().UTC()
		if updateErr := d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error { return writer.SetBillingCustomerIntent(ctx, existing) }); updateErr != nil {
			return billing.Binding{}, errors.Join(err, updateErr)
		}
		return billing.Binding{}, err
	}
	if err != nil {
		return billing.Binding{}, err
	}
	audit, err := json.Marshal(map[string]string{"status": "customer_bound", "mode": cfg.mode()})
	if err != nil {
		return billing.Binding{}, err
	}
	if d.billingAuditStore == nil {
		return billing.Binding{}, errors.New("hosted billing audit storage is unavailable")
	}
	archive, err := d.billingAuditStore.StoreDetail(ctx, organization, audit)
	if err != nil {
		return billing.Binding{}, err
	}
	existing.State, existing.CustomerID, existing.UpdatedAt = "bound", customer, s.config.now().UTC()
	err = d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		account, err := writer.BillingAccount(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			account = billingstore.Account{OrganizationID: d.hostedOrganization, Mode: cfg.mode(), Binding: billing.Binding{OrganizationID: organization, AccountID: cfg.AccountID, CustomerID: customer}, Status: "free"}
		} else if err != nil {
			return err
		} else if account.Binding.AccountID != cfg.AccountID || account.Binding.CustomerID != customer || account.Mode != cfg.mode() {
			return billing.ErrCustomerConflict
		}
		if err := writer.SetBillingAccount(ctx, account); err != nil {
			return err
		}
		if err := writer.SetBillingCustomerIntent(ctx, existing); err != nil {
			return err
		}
		return writer.CreateBillingAudit(ctx, billingstore.Audit{OrganizationID: d.hostedOrganization, ActorID: actor, Action: "customer_bound", RecordedAt: s.config.now().UTC(), Reference: billingstore.AuditReferenceFromArchive(archive)})
	})
	if err != nil {
		return billing.Binding{}, err
	}
	return d.hostedBillingBinding(ctx, cfg)
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
	return s.refusedAPIError(c, status, code, message, apiErrorResponse{Code: code, Message: message})
}

func (s *Service) hostedBillingDestination(c echo.Context, api bool, url string) error {
	if api {
		return c.JSON(http.StatusOK, map[string]string{"url": url})
	}
	return c.Redirect(http.StatusSeeOther, url)
}
