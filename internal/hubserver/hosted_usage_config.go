package hubserver

import "strings"

// The hub's own price table (decisions section 17.5). Cost on the usage
// report is an estimate, labelled "API estimate" by the client. A runner
// that priced its own turn wins; a runner that reported no price is priced
// here, so a fleet whose runners have no price table still gets a report.

// UsagePrice is what a million tokens of one model costs. CachedInput is the
// discounted rate a provider charges for input it served from its cache;
// leaving it zero means cached input is free.
type UsagePrice struct {
	Input       float64
	CachedInput float64
	Output      float64
	Currency    string
}

// UsageConfig is the `usage:` section of the hosted configuration.
type UsageConfig struct {
	// Prices is keyed by model identifier, matched case-insensitively.
	Prices map[string]UsagePrice
	// Currency is what a price without one is denominated in. Empty selects
	// defaultUsageCurrency.
	Currency string
}

const defaultUsageCurrency = "USD"

// tokensPerPriceUnit is the unit every price is quoted in.
const tokensPerPriceUnit = 1_000_000

// normalized lowercases the model keys and fills in the table currency, so a
// lookup does not have to care how the file was written.
func (c UsageConfig) normalized() UsageConfig {
	currency := strings.TrimSpace(c.Currency)
	if currency == "" {
		currency = defaultUsageCurrency
	}
	prices := make(map[string]UsagePrice, len(c.Prices))
	for model, price := range c.Prices {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			continue
		}
		if strings.TrimSpace(price.Currency) == "" {
			price.Currency = currency
		}
		prices[key] = price
	}
	return UsageConfig{Prices: prices, Currency: currency}
}

// price reports the table entry for a model.
func (c UsageConfig) price(model string) (UsagePrice, bool) {
	if len(c.Prices) == 0 {
		return UsagePrice{}, false
	}
	price, found := c.Prices[strings.ToLower(strings.TrimSpace(model))]
	return price, found
}

// estimate prices usage the runner did not price. input is every input
// token and cachedInput the part of it the provider served from cache, so
// the uncached remainder is what the full rate applies to.
func (c UsageConfig) estimate(model string, input, cachedInput, output int64) (float64, string, bool) {
	price, found := c.price(model)
	if !found {
		return 0, c.currency(), false
	}
	uncached := input - cachedInput
	if uncached < 0 {
		uncached = 0
		cachedInput = input
	}
	cost := float64(uncached)/tokensPerPriceUnit*price.Input +
		float64(cachedInput)/tokensPerPriceUnit*price.CachedInput +
		float64(output)/tokensPerPriceUnit*price.Output
	return cost, price.Currency, true
}

// cacheSaving is what serving cachedInput tokens from cache saved against
// the full input rate. A model the table does not know saved nothing it can
// prove (decisions section 17.5, totals.cache_savings).
func (c UsageConfig) cacheSaving(model string, cachedInput int64) float64 {
	price, found := c.price(model)
	if !found || cachedInput <= 0 {
		return 0
	}
	saving := float64(cachedInput) / tokensPerPriceUnit * (price.Input - price.CachedInput)
	if saving < 0 {
		return 0
	}
	return saving
}

func (c UsageConfig) currency() string {
	if strings.TrimSpace(c.Currency) == "" {
		return defaultUsageCurrency
	}
	return c.Currency
}

// usagePrices is the hub's effective price table, normalized once at
// configuration time. A hub with no table prices nothing and reports only
// what its runners priced themselves.
func (s *Service) usagePrices() UsageConfig {
	if s.config.Usage == nil {
		return UsageConfig{Currency: defaultUsageCurrency}
	}
	return *s.config.Usage
}
