package usagecosts

import "strings"

type Price struct {
	Input       float64
	CachedInput float64
	Output      float64
	Currency    string
}
type Prices struct {
	Prices   map[string]Price
	Currency string
}

const DefaultCurrency = "USD"
const TokensPerPriceUnit = 1_000_000

func (c Prices) Normalized() Prices {
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
		price.Currency = currency
		prices[key] = price
	}
	return Prices{Prices: prices, Currency: currency}
}
func (c Prices) PriceFor(model string) (Price, bool) {
	if len(c.Prices) == 0 {
		return Price{}, false
	}
	price, found := c.Prices[strings.ToLower(strings.TrimSpace(model))]
	return price, found
}
func (c Prices) Estimate(model string, input, cachedInput, output int64) (float64, string, bool) {
	price, found := c.PriceFor(model)
	if !found {
		return 0, c.CurrencyValue(), false
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
func (c Prices) CacheSaving(model string, input, cachedInput int64) float64 {
	price, found := c.PriceFor(model)
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

func (c Prices) CurrencyValue() string {
	if strings.TrimSpace(c.Currency) == "" {
		return DefaultCurrency
	}
	return c.Currency
}
