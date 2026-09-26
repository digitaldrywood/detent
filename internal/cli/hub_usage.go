package cli

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

// The `usage:` section of the hosted configuration file is the hub's price
// table (decisions section 17.5). Cost on the usage report is an estimate:
// what a runner priced its own turn at, or, when it priced nothing, what
// this table says a million tokens of that model costs.

type hostedUsagePriceFileConfig struct {
	Input       float64 `yaml:"input"`
	CachedInput float64 `yaml:"cached_input"`
	Output      float64 `yaml:"output"`
	Currency    string  `yaml:"currency"`
}

type hostedUsageFileConfig struct {
	// Currency is what a price without one of its own is denominated in.
	Currency string `yaml:"currency"`
	// Prices is keyed by model identifier. Every rate is per million tokens.
	Prices map[string]hostedUsagePriceFileConfig `yaml:"prices"`
}

// currencyCodeLength is the length of an ISO 4217 code, which is all the hub
// validates: it does not keep a currency list, it only refuses a value that
// cannot be one.
const currencyCodeLength = 3

// defaultUsageCurrency is what a table without a currency is denominated in.
const defaultUsageCurrency = "USD"

// readHostedUsageConfig reads the `usage:` section and reports whether the
// hub prices anything. A hub without the section, or with an empty price
// table, prices nothing and reports only what its runners priced themselves.
func readHostedUsageConfig(path string) (config hubserver.UsageConfig, priced bool, resultErr error) {
	if strings.TrimSpace(path) == "" {
		return hubserver.UsageConfig{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return hubserver.UsageConfig{}, false, errors.New("hosted configuration could not be opened")
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("hosted configuration could not be closed"))
		}
	}()
	decoder := yaml.NewDecoder(io.LimitReader(file, 128*1024))
	decoder.KnownFields(true)
	var section hostedFileConfig
	if err := decoder.Decode(&section); err != nil {
		return hubserver.UsageConfig{}, false, errors.New("hosted configuration is invalid")
	}
	return convertUsageConfig(section.Usage)
}

// convertUsageConfig validates the file section and turns it into the hub's
// price table.
func convertUsageConfig(section *hostedUsageFileConfig) (hubserver.UsageConfig, bool, error) {
	if section == nil || len(section.Prices) == 0 {
		return hubserver.UsageConfig{}, false, nil
	}
	currency, err := validUsageCurrency(section.Currency)
	if err != nil {
		return hubserver.UsageConfig{}, false, err
	}
	if currency == "" {
		currency = defaultUsageCurrency
	}
	prices := make(map[string]hubserver.UsagePrice, len(section.Prices))
	keys := make(map[string]string, len(section.Prices))
	for model, price := range section.Prices {
		name := strings.TrimSpace(model)
		if name == "" {
			return hubserver.UsageConfig{}, false, errors.New("usage prices must name a model")
		}
		key := strings.ToLower(name)
		if other, taken := keys[key]; taken {
			first, second := min(other, model), max(other, model)
			return hubserver.UsageConfig{}, false, errors.New("usage prices name model " + strconv.Quote(first) + " and " + strconv.Quote(second) + ", which match the same model")
		}
		keys[key] = model
		if price.Input < 0 || price.CachedInput < 0 || price.Output < 0 {
			return hubserver.UsageConfig{}, false, errors.New("usage prices for " + name + " must not be negative")
		}
		unit, err := validUsageCurrency(price.Currency)
		if err != nil {
			return hubserver.UsageConfig{}, false, err
		}
		if unit == "" {
			unit = currency
		}
		if unit != currency {
			return hubserver.UsageConfig{}, false, errors.New("usage prices for " + name + " must be in the table currency " + currency + ": a hub reports in one currency")
		}
		prices[name] = hubserver.UsagePrice{Input: price.Input, CachedInput: price.CachedInput, Output: price.Output, Currency: unit}
	}
	return hubserver.UsageConfig{Currency: currency, Prices: prices}, true, nil
}

// validUsageCurrency accepts an empty value, which inherits the table's
// currency, or a three-letter code.
func validUsageCurrency(value string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(value))
	if code == "" {
		return "", nil
	}
	if len(code) != currencyCodeLength {
		return "", errors.New("usage currency must be a three-letter code such as USD")
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return "", errors.New("usage currency must be a three-letter code such as USD")
		}
	}
	return code, nil
}
