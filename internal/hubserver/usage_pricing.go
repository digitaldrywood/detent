package hubserver

import "github.com/digitaldrywood/detent/internal/hostedusage"

type UsagePrice = hostedusage.Price
type UsageConfig = hostedusage.Config

const defaultUsageCurrency = hostedusage.DefaultCurrency
const tokensPerPriceUnit = hostedusage.TokensPerPriceUnit

// usagePrices is the hub's effective price table, normalized once at
// configuration time. A hub with no table prices nothing and reports only
// what its runners priced themselves.
func (s *Service) usagePrices() UsageConfig {
	if s.config.Usage == nil {
		return UsageConfig{Currency: defaultUsageCurrency}
	}
	return *s.config.Usage
}
