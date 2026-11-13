package hostedplans

import (
	"math"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func TestHostedBillingConfiguration(t *testing.T) {
	t.Parallel()
	f, _ := newHostedBillingFixture(t)
	for _, test := range []struct {
		name string
		edit func(*HostedBillingConfig)
	}{
		{"missing provider", func(c *HostedBillingConfig) { c.Provider = nil }},
		{"missing account", func(c *HostedBillingConfig) { c.AccountID = "" }},
		{"missing customer", func(c *HostedBillingConfig) { c.CustomerID = "" }},
		{"missing portal", func(c *HostedBillingConfig) { c.PortalConfigurationID = "" }},
		{"missing secret", func(c *HostedBillingConfig) { c.WebhookSecret = nil }},
		{"negative multiplier", func(c *HostedBillingConfig) { c.CreditCostMultiplier = -1 }},
		{"NaN multiplier", func(c *HostedBillingConfig) { c.CreditCostMultiplier = math.NaN() }},
		{"infinite multiplier", func(c *HostedBillingConfig) { c.CreditCostMultiplier = math.Inf(1) }},
		{"negative grace", func(c *HostedBillingConfig) { c.GraceSeconds = -1 }},
		{"excess grace", func(c *HostedBillingConfig) { c.GraceSeconds = 8 * 86400 }},
		{"unbounded polling", func(c *HostedBillingConfig) { c.ReconcileSeconds = 1 }},
		{"unknown plan", func(c *HostedBillingConfig) { c.Prices[0].Plan.ID = "unknown" }},
		{"base plan purchase", func(c *HostedBillingConfig) { c.Prices[0].Plan.ID = "test_free" }},
		{"duplicate price", func(c *HostedBillingConfig) { c.Prices = append(c.Prices, c.Prices[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := *f.service.config.Hosted.Billing
			config.Prices = slices.Clone(config.Prices)
			test.edit(&config)
			if err := config.Validate(f.service.config.Hosted.Plans); err == nil {
				t.Fatal("invalid billing configuration accepted")
			}
		})
	}
	for _, test := range []struct {
		name             string
		multiplier, want float64
	}{
		{"default multiplier", 0, 1.5},
		{"custom multiplier", 2, 2},
		{"at cost", 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newHostedBillingFixture(t)
			config := *f.service.config.Hosted
			changed := *config.Billing
			config.Billing = &changed
			changed.CreditCostMultiplier = test.multiplier
			changed.Provider = runtimeCreditProvider{}
			changed.CreditPacks = []HostedCreditPack{{PriceID: "price_credit", Label: "AI credit pack", USDCents: 500}}
			if err := changed.Validate(config.Plans); err != nil {
				t.Fatal(err)
			}
			if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), PlansInput(&config))); err != nil {
				t.Fatal(err)
			}
			if changed.CreditCostMultiplier != test.want || f.service.database.aiCreditCostMultiplier != test.want {
				t.Fatalf("configured multiplier=%g database multiplier=%g want=%g", changed.CreditCostMultiplier, f.service.database.aiCreditCostMultiplier, test.want)
			}
		})
	}
	config := *f.service.config.Hosted
	changed := *config.Billing
	config.Billing = &changed
	changed.CustomerID = "cus_other"
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), PlansInput(&config))); err == nil {
		t.Fatal("customer binding changed")
	}
	changed.CustomerID = "cus_fixture"
	changed.Prices = slices.Clone(changed.Prices)
	changed.Prices[0].Plan.ID = "test_free"
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), PlansInput(&config))); err == nil {
		t.Fatal("price was repointed to another plan")
	}
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), PlansInput(f.service.config.Hosted))); err != nil {
		t.Fatalf("same configuration could not reopen: %v", err)
	}
}
