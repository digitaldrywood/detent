package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const defaultAICreditCostMultiplier = 1.5

type HostedBillingPrice struct {
	PriceID string        `yaml:"price_id" json:"price_id"`
	Label   string        `yaml:"label" json:"label"`
	Plan    PlanReference `yaml:"plan" json:"plan"`
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
	CreditCostMultiplier  float64
	Prices                []HostedBillingPrice
	Provider              billing.Provider
}

func (c *HostedBillingConfig) validate(plans *HostedPlansConfig) error {
	if c == nil {
		return nil
	}
	if c.CreditCostMultiplier == 0 {
		c.CreditCostMultiplier = defaultAICreditCostMultiplier
	}
	if c.CreditCostMultiplier < 0 || math.IsNaN(c.CreditCostMultiplier) || math.IsInf(c.CreditCostMultiplier, 0) {
		return errors.New("AI credit cost multiplier must be finite and positive")
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if d.billingRepository != nil {
		var result billing.Binding
		err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
			account, err := reader.BillingAccount(ctx)
			if err != nil {
				return err
			}
			if account.Binding.AccountID != c.AccountID || account.Mode != c.mode() {
				return errors.New("hosted Stripe binding belongs to a different account or mode")
			}
			result = account.Binding
			return nil
		})
		if errors.Is(err, corestore.ErrNotFound) {
			return billing.Binding{}, errHostedBillingUnbound
		}
		return result, err
	}
	return readHostedBillingBinding(ctx, d.reader, d.hostedOrganization, c)
}

func readHostedBillingBinding(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, c *HostedBillingConfig) (billing.Binding, error) {
	var account, customer, mode string
	err := query.QueryRowContext(ctx, "SELECT account_id,customer_id,mode FROM hosted_billing_accounts WHERE organization_id=?", organization).Scan(&account, &customer, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		return billing.Binding{}, errHostedBillingUnbound
	}
	if err != nil {
		return billing.Binding{}, err
	}
	if account != c.AccountID || mode != c.mode() {
		return billing.Binding{}, errors.New("hosted Stripe binding belongs to a different account or mode")
	}
	return billing.Binding{AccountID: account, CustomerID: customer, OrganizationID: string(organization)}, nil
}

func (d *database) configureHostedBilling(ctx context.Context, cfg *HostedConfig) error {
	if cfg == nil || cfg.Billing == nil {
		return nil
	}
	c := cfg.Billing
	if d.billingRepository != nil {
		return d.configureHostedBillingRecords(ctx, c)
	}
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
	if err := seedHostedBillingPrices(ctx, tx, c.Prices); err != nil {
		return err
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
		d.aiCreditCostMultiplier = c.CreditCostMultiplier
	} else {
		d.aiCreditMode = ""
	}
	d.hostedBilling = true
	return nil
}

func (d *database) configureHostedBillingRecords(ctx context.Context, config *HostedBillingConfig) error {
	var existing billingstore.Account
	var hasExisting bool
	if err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		existing, err = reader.BillingAccount(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err == nil {
			hasExisting = true
		}
		return err
	}); err != nil {
		return err
	}
	retire := hasExisting && existing.Mode == billing.ModeTest && config.mode() == billing.ModeLive && config.CustomerID == ""
	var retiredReference billingstore.AuditReference
	if retire {
		if d.billingAuditStore == nil {
			return errors.New("hosted billing audit storage is unavailable")
		}
		raw, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		archive, err := d.billingAuditStore.StoreDetail(ctx, string(d.hostedOrganization), raw)
		if err != nil {
			return fmt.Errorf("store retired billing account: %w", err)
		}
		retiredReference = billingstore.AuditReferenceFromArchive(archive)
	}
	now := d.now().UTC()
	err := d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		current, err := writer.BillingAccount(ctx)
		currentExists := err == nil
		if errors.Is(err, corestore.ErrNotFound) {
			err = nil
		}
		if err != nil {
			return err
		}
		if hasExisting {
			if !currentExists || current.Binding.AccountID != existing.Binding.AccountID || current.Binding.CustomerID != existing.Binding.CustomerID || current.Mode != existing.Mode {
				return errors.New("hosted Stripe binding changed during configuration")
			}
		} else if currentExists && (config.CustomerID == "" || current.Binding.AccountID != config.AccountID || current.Binding.CustomerID != config.CustomerID || current.Mode != config.mode()) {
			return errors.New("hosted Stripe binding changed during configuration")
		}
		if retire {
			if err := writer.DeleteBillingAccountIfMatches(ctx, existing.Binding.AccountID, existing.Binding.CustomerID, existing.Mode); err != nil {
				return err
			}
			row := billingstore.RetiredAccount{OrganizationID: d.hostedOrganization, AccountID: existing.Binding.AccountID, CustomerID: existing.Binding.CustomerID, Mode: existing.Mode, RetiredAt: now, Reference: retiredReference}
			if err := writer.CreateRetiredBillingAccount(ctx, row); err != nil {
				return err
			}
			if err := writer.DeleteBillingCustomerIntent(ctx, existing.Mode); err != nil {
				return err
			}
			currentExists = false
		}
		if config.CustomerID != "" {
			if !currentExists {
				account := billingstore.Account{OrganizationID: d.hostedOrganization, Mode: config.mode(), Binding: billing.Binding{OrganizationID: string(d.hostedOrganization), AccountID: config.AccountID, CustomerID: config.CustomerID}, Status: "free"}
				if _, err := writer.CreateBillingAccountIfAbsent(ctx, account); err != nil {
					return err
				}
				current, err = writer.BillingAccount(ctx)
				if err != nil {
					return err
				}
			}
			if current.Binding.AccountID != config.AccountID || current.Binding.CustomerID != config.CustomerID || current.Mode != config.mode() {
				return errors.New("hosted Stripe account, customer and mode binding is immutable; a mode change cannot reinterpret existing billing records")
			}
		} else if hasExisting && !retire && (existing.Binding.AccountID != config.AccountID || existing.Mode != config.mode()) {
			return errors.New("hosted Stripe account, customer and mode binding is immutable; a mode change cannot reinterpret existing billing records")
		}
		for _, price := range config.Prices {
			stored, err := writer.BillingPrice(ctx, price.PriceID)
			if errors.Is(err, corestore.ErrNotFound) {
				if err := writer.CreateBillingPrice(ctx, billingstore.Price{OrganizationID: d.hostedOrganization, PriceID: price.PriceID, Plan: billingstore.PlanReference{ID: price.Plan.ID, Version: price.Plan.Version}}); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if stored.Plan != (billingstore.PlanReference{ID: price.Plan.ID, Version: price.Plan.Version}) {
				return errors.New("hosted Stripe price mappings are immutable; configure a new price")
			}
		}
		for _, pack := range config.CreditPacks {
			stored, err := writer.CreditPack(ctx, pack.PriceID, config.mode())
			if errors.Is(err, corestore.ErrNotFound) {
				if err := writer.CreateCreditPack(ctx, billingstore.CreditPack{OrganizationID: d.hostedOrganization, PriceID: pack.PriceID, Mode: config.mode(), USDCents: pack.USDCents}); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if stored.USDCents != pack.USDCents {
				return errors.New("AI credit pack amounts are immutable; configure a new Stripe price")
			}
		}
		creditAccount, err := writer.BillingCreditAccount(ctx, config.mode())
		if errors.Is(err, corestore.ErrNotFound) && len(config.CreditPacks) > 0 {
			return writer.CreateBillingCreditAccount(ctx, billingstore.CreditAccount{OrganizationID: d.hostedOrganization, Mode: config.mode()})
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if len(config.CreditPacks) > 0 || creditAccount.OrganizationID != "" {
			if _, ok := config.Provider.(billing.CreditProvider); !ok {
				return errors.New("existing AI credits require a credit-capable billing provider")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	d.hostedBilling = true
	if len(config.CreditPacks) > 0 {
		d.aiCreditMode = config.mode()
		d.aiCreditCostMultiplier = config.CreditCostMultiplier
	} else {
		d.aiCreditMode = ""
	}
	return nil
}

func seedHostedBillingPrices(ctx context.Context, tx *sql.Tx, prices []HostedBillingPrice) error {
	for _, price := range prices {
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
	return nil
}
