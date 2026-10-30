package hostedusage

import "strings"

// The hub's own price table. Cost on the usage
// report is an estimate, labelled "API estimate" by the client. A runner
// that priced its own turn wins; a runner that reported no price is priced
// here, so a fleet whose runners have no price table still gets a report.

// Price is what a million tokens of one model costs. CachedInput is the
// discounted rate a provider charges for input it served from its cache;
// leaving it zero means cached input is free.
type Price struct {
	Input       float64
	CachedInput float64
	Output      float64
	Currency    string
}

// Config is the `usage:` section of the hosted configuration.
type Config struct {
	// Prices is keyed by model identifier, matched case-insensitively.
	Prices map[string]Price
	// Currency is what a price without one is denominated in. Empty selects
	// DefaultCurrency.
	Currency string
}

const DefaultCurrency = "USD"

// TokensPerPriceUnit is the unit every price is quoted in.
const TokensPerPriceUnit = 1_000_000

// Normalized lowercases the model keys and fills in the table currency, so a
// lookup does not have to care how the file was written.
func (c Config) Normalized() Config {
	currency := strings.TrimSpace(c.Currency)
	if currency == "" {
		currency = DefaultCurrency
	}
	prices := make(map[string]Price, len(c.Prices))
	for model, price := range c.Prices {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			continue
		}
		// A hub prices in one currency: the loader refuses a table that mixes
		// them, so every entry carries the table's.
		price.Currency = currency
		prices[key] = price
	}
	return Config{Prices: prices, Currency: currency}
}

// price reports the table entry for a model.
func (c Config) price(model string) (Price, bool) {
	if len(c.Prices) == 0 {
		return Price{}, false
	}
	price, found := c.Prices[strings.ToLower(strings.TrimSpace(model))]
	return price, found
}

// Estimate prices usage the runner did not price. input is every input
// token and cachedInput the part of it the provider served from cache, so
// the uncached remainder is what the full rate applies to.
func (c Config) Estimate(model string, input, cachedInput, output int64) (float64, string, bool) {
	price, found := c.price(model)
	if !found {
		return 0, c.CurrencyName(), false
	}
	uncached := input - cachedInput
	if uncached < 0 {
		uncached = 0
		cachedInput = input
	}
	cost := float64(uncached)/TokensPerPriceUnit*price.Input +
		float64(cachedInput)/TokensPerPriceUnit*price.CachedInput +
		float64(output)/TokensPerPriceUnit*price.Output
	return cost, price.Currency, true
}

// CacheSaving is what serving cachedInput of input tokens from cache saved
// against the full input rate. A model the table does not know saved nothing it can
// prove (totals.cache_savings).
func (c Config) CacheSaving(model string, input, cachedInput int64) float64 {
	price, found := c.price(model)
	// Cached input beyond the input total is capped exactly as estimate
	// caps it, so the saving never exceeds what was priced.
	cachedInput = min(cachedInput, max(input, 0))
	if !found || cachedInput <= 0 {
		return 0
	}
	saving := float64(cachedInput) / TokensPerPriceUnit * (price.Input - price.CachedInput)
	if saving < 0 {
		return 0
	}
	return saving
}

func (c Config) CurrencyName() string {
	if strings.TrimSpace(c.Currency) == "" {
		return DefaultCurrency
	}
	return c.Currency
}
