package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/billing"
)

type HostedBillingPrice struct {
	PriceID string        `yaml:"price_id"`
	Label   string        `yaml:"label"`
	Plan    PlanReference `yaml:"plan"`
}

type HostedCreditPack struct {
	PriceID  string `yaml:"price_id" json:"price_id"`
	Label    string `yaml:"label" json:"label"`
	USDCents int64  `yaml:"usd_cents" json:"usd_cents"`
}

type HostedBillingConfig struct {
	CheckoutDisabled      bool
	Mode                  string
	AccountID             string
	CustomerID            string
	PortalConfigurationID string
	WebhookSecret         []byte
	GraceSeconds          int64
	ReconcileSeconds      int64
	CreditPacks           []HostedCreditPack
	Prices                []HostedBillingPrice
	Provider              billing.Provider
}

func (c *HostedBillingConfig) validate(plans *HostedPlansConfig) error {
	if c == nil {
		return nil
	}
	if c.Mode == "" {
		c.Mode = billing.ModeTest
	}
	if !billing.ValidMode(c.Mode) {
		return errors.New("hosted billing mode must be test or live")
	}
	if c.CustomerID == "" {
		if _, ok := c.Provider.(billing.CustomerProvider); !ok {
			return errors.New("hosted billing without a configured customer requires a provider that can create customers")
		}
	} else if !strings.HasPrefix(c.CustomerID, "cus_") || !hostedSafeID(c.CustomerID) {
		return errors.New("hosted billing customer ID is invalid")
	}
	if plans == nil || c.Provider == nil || !strings.HasPrefix(c.AccountID, "acct_") || !hostedSafeID(c.AccountID) || !strings.HasPrefix(c.PortalConfigurationID, "bpc_") || !hostedSafeID(c.PortalConfigurationID) || len(c.WebhookSecret) < 16 || !strings.HasPrefix(string(c.WebhookSecret), "whsec_") || c.GraceSeconds < 0 || c.GraceSeconds > 7*86400 || c.ReconcileSeconds < 60 || c.ReconcileSeconds > 3600 || len(c.Prices) == 0 || len(c.Prices) > 20 {
		return errors.New("hosted billing requires a test provider, explicit account/customer/portal binding, webhook secret, plans and bounded grace/reconciliation settings")
	}
	seen := make(map[string]bool)
	for _, price := range c.Prices {
		found := false
		for _, plan := range plans.Plans {
			found = found || plan.PlanReference == price.Plan
		}
		if !strings.HasPrefix(price.PriceID, "price_") || !hostedSafeID(price.PriceID) || seen[price.PriceID] || strings.TrimSpace(price.Label) == "" || len(price.Label) > 80 || !found || price.Plan == plans.Base {
			return errors.New("hosted billing prices require unique approved paid plans and bounded labels")
		}
		seen[price.PriceID] = true
	}
	if len(c.CreditPacks) > 20 {
		return errors.New("too many AI credit packs")
	}
	if len(c.CreditPacks) > 0 {
		if _, ok := c.Provider.(billing.CreditProvider); !ok {
			return errors.New("billing provider cannot sell AI credits")
		}
	}
	for _, pack := range c.CreditPacks {
		if !strings.HasPrefix(pack.PriceID, "price_") || !hostedSafeID(pack.PriceID) || seen[pack.PriceID] || strings.TrimSpace(pack.Label) == "" || len(pack.Label) > 80 || pack.USDCents <= 0 || pack.USDCents > 1000000 {
			return errors.New("AI credit packs require unique one-time prices, bounded labels and positive USD cents")
		}
		seen[pack.PriceID] = true
	}
	return nil
}

func (c *HostedBillingConfig) mode() string {
	if c.Mode == "" {
		return billing.ModeTest
	}
	return c.Mode
}

var errHostedBillingUnbound = errors.New("organization has no billing customer yet")

func (d *database) hostedBillingBinding(ctx context.Context, c *HostedBillingConfig) (billing.Binding, error) {
	var account, customer, mode string
	err := d.db.QueryRowContext(ctx, "SELECT account_id,customer_id,mode FROM hosted_billing_accounts WHERE organization_id=?", d.hostedOrganization).Scan(&account, &customer, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		return billing.Binding{}, errHostedBillingUnbound
	}
	if err != nil {
		return billing.Binding{}, err
	}
	if account != c.AccountID || mode != c.mode() {
		return billing.Binding{}, errors.New("hosted Stripe binding belongs to a different account or mode")
	}
	return billing.Binding{AccountID: account, CustomerID: customer, OrganizationID: string(d.hostedOrganization)}, nil
}

func (d *database) configureHostedBilling(ctx context.Context, cfg *HostedConfig) error {
	if cfg == nil || cfg.Billing == nil {
		return nil
	}
	c := cfg.Billing
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if c.CustomerID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_billing_accounts(organization_id,account_id,customer_id,mode) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, d.hostedOrganization, c.AccountID, c.CustomerID, c.mode()); err != nil {
			return err
		}
	}
	var account, customer, mode string
	err = tx.QueryRowContext(ctx, "SELECT account_id,customer_id,mode FROM hosted_billing_accounts WHERE organization_id=?", d.hostedOrganization).Scan(&account, &customer, &mode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && mode == billing.ModeTest && c.mode() == billing.ModeLive && c.CustomerID == "" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_billing_retired(organization_id,account_id,customer_id,mode,state_json,retired_at) SELECT organization_id,account_id,customer_id,mode,state_json,? FROM hosted_billing_accounts WHERE organization_id=?", formatHubTime(d.now()), d.hostedOrganization); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_billing_accounts WHERE organization_id=?", d.hostedOrganization); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_billing_customer_intents WHERE organization_id=? AND mode='test'", d.hostedOrganization); err != nil {
			return err
		}
		err = sql.ErrNoRows
	}
	if err == nil && (account != c.AccountID || mode != c.mode() || c.CustomerID != "" && customer != c.CustomerID) {
		return errors.New("hosted Stripe account, customer and mode binding is immutable; a mode change cannot reinterpret existing billing records")
	}
	for _, price := range c.Prices {
		var existing PlanReference
		err := tx.QueryRowContext(ctx, "SELECT plan_id,plan_version FROM hosted_billing_prices WHERE price_id=?", price.PriceID).Scan(&existing.ID, &existing.Version)
		if err == nil && existing != price.Plan {
			return errors.New("hosted Stripe price mappings are immutable; configure a new price")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_billing_prices(price_id,plan_id,plan_version) VALUES(?,?,?) ON CONFLICT DO NOTHING", price.PriceID, price.Plan.ID, price.Plan.Version); err != nil {
			return err
		}
	}
	for _, pack := range c.CreditPacks {
		var cents int64
		err := tx.QueryRowContext(ctx, "SELECT usd_cents FROM ai_credit_packs WHERE price_id=? AND mode=?", pack.PriceID, c.mode()).Scan(&cents)
		if err == nil && cents != pack.USDCents {
			return errors.New("AI credit pack amounts are immutable; configure a new Stripe price")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO ai_credit_packs(price_id,mode,usd_cents) VALUES(?,?,?) ON CONFLICT DO NOTHING", pack.PriceID, c.mode(), pack.USDCents); err != nil {
			return err
		}
	}
	if len(c.CreditPacks) > 0 {
		if _, err := tx.ExecContext(ctx, "INSERT INTO ai_credit_accounts(organization_id,mode) VALUES(?,?) ON CONFLICT DO NOTHING", d.hostedOrganization, c.mode()); err != nil {
			return err
		}
	}
	var creditAccounts int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ai_credit_accounts WHERE organization_id=? AND mode=?", d.hostedOrganization, c.mode()).Scan(&creditAccounts); err != nil {
		return err
	}
	if creditAccounts > 0 {
		if _, ok := c.Provider.(billing.CreditProvider); !ok {
			return errors.New("existing AI credits require a credit-capable billing provider")
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if creditAccounts > 0 {
		d.aiCreditMode = c.mode()
	} else {
		d.aiCreditMode = ""
	}
	d.hostedBilling = true
	return nil
}
